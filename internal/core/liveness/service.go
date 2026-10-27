package liveness

import (
	"context"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// Failover starts an app's standby on its standby host, which becomes the
// app's home (R-419). The standby slice implements it; Service calls it once
// per app homed on a lost host that has a standby and is not failed.
type Failover interface {
	PromoteStandby(ctx context.Context, appID string) error
}

// HomedApp is an app whose home is a host, and whether it has a standby.
type HomedApp struct {
	AppID      string
	HasStandby bool
}

// Homes lists the apps homed on a host. The standby slice implements it, with
// Failover.
type Homes interface {
	AppsHomedOn(ctx context.Context, hostID string) ([]HomedApp, error)
}

// Returns is what a lost host coming back needs (R-419): the apps failed
// over away from it, whose copies it still holds, and removing each copy,
// never restarting it (R-148 does not apply to a copy that was failed over).
// The standby slice implements it, with Failover.
type Returns interface {
	AppsLeftOn(ctx context.Context, hostID string) ([]string, error)
	RemoveStale(ctx context.Context, appID string) error
}

// Store is the slice of state.Cluster the service uses.
type Store interface {
	Host(ctx context.Context, hostID string) (state.Host, error)
	MarkHostLost(ctx context.Context, hostID, by string) (bool, error)
	ClearHostLost(ctx context.Context, hostID string) error
	LostHosts(ctx context.Context) (map[string]bool, error)
	RecordHostFence(ctx context.Context, hostID, fenceID string) (bool, error)
}

// Apps reads an app, for its state: a failed app is never failed over
// (R-151).
type Apps interface {
	ByID(ctx context.Context, appID string) (state.App, bool, error)
}

// Auditor writes the audit log.
type Auditor interface {
	Write(ctx context.Context, e audit.Event) error
}

// Actor is who said a host is lost.
type Actor struct {
	Kind       audit.PrincipalKind
	ID         string
	OnBehalfOf string
	RequestID  string
}

// system is the leader, acting on the majority rule.
var system = Actor{Kind: audit.KindSystem, ID: "liveness"}

// How a host came to be taken for lost, in its audit events.
const (
	triggerMajority = "majority"
	triggerManual   = "manual"
)

// Why an app on a lost host was not failed over.
const (
	SkipNoStandby  = "no_standby"
	SkipFailed     = "failed"
	SkipGone       = "gone"
	SkipNotWired   = "standbys_unavailable"
	SkipPromoteErr = "promote_failed"
)

// SkippedApp is an app on a lost host that was not started elsewhere.
type SkippedApp struct {
	AppID  string `json:"app_id"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// HostLoss is what taking a host for lost did.
type HostLoss struct {
	HostID      string       `json:"host_id"`
	Name        string       `json:"name"`
	AlreadyLost bool         `json:"already_lost"`
	Started     []string     `json:"started"`
	Skipped     []SkippedApp `json:"skipped"`
}

// Service takes hosts for lost and starts their apps' standbys (R-419,
// R-421): automatically when Watch, on the leader, finds a majority has not
// reached a host for three Periods, and by hand through MarkLost. It also
// audits the fences hosts report.
type Service struct {
	Store    Store
	Apps     Apps
	Audit    Auditor
	Logger   *zap.Logger
	Homes    Homes
	Failover Failover
	// Returns removes a returned host's copies before its lost mark is
	// cleared. Nil leaves a lost host marked: without it nothing would
	// remove the copies, and a host still marked lost starts nothing.
	Returns Returns

	// Answering reports whether the host's agent answers a probe now. A
	// host that answers is not gone, and is not marked lost by hand: its
	// apps may be running, and a standby started beside them would be a
	// second copy (R-153). Nil skips the check.
	Answering func(ctx context.Context, hostID string) bool
}

func (s *Service) logger() *zap.Logger {
	if s.Logger == nil {
		return zap.NewNop()
	}
	return s.Logger
}

// MarkLost is a person saying a host is gone (R-421): the host is marked lost
// and its apps' standbys started. It is how a cluster under three hosts fails
// over at all, and is allowed in any cluster. Authorization is the caller's:
// install.cluster.manage. Marking a host already lost again starts the
// standbys of apps still homed on it, which is how a failover that failed is
// retried.
func (s *Service) MarkLost(ctx context.Context, hostID string, by Actor) (HostLoss, error) {
	h, err := s.Store.Host(ctx, hostID)
	if err != nil {
		return HostLoss{}, err
	}
	if s.Answering != nil && s.Answering(ctx, h.ID) {
		return HostLoss{}, errs.Newf(errs.StateInvalid,
			"Host %s (%s) answers on its agent port, so it is not gone. Starting its apps' standbys now would run a second copy of each.", h.Name, h.ID).
			WithRemedy("If the host is up but its apps are not, turn the host off or stop its Pando agent, then mark it lost again.")
	}
	return s.lose(ctx, h, by, triggerManual)
}

// declare is the leader taking a host for lost by the majority rule. Only
// the first time: a host already lost has had its failover.
func (s *Service) declare(ctx context.Context, hostID string) {
	h, err := s.Store.Host(ctx, hostID)
	if err != nil {
		s.logger().Warn("could not read a host a majority has not reached", zap.String("host_id", hostID), zap.Error(err))
		return
	}
	out, err := s.lose(ctx, h, system, triggerMajority)
	if err != nil {
		s.logger().Error("could not take a host for lost", zap.String("host_id", hostID), zap.Error(err))
		return
	}
	s.logger().Warn("a majority of the cluster's hosts has not reached a host; took it for lost",
		zap.String("host_id", hostID), zap.Strings("started", out.Started), zap.Int("skipped", len(out.Skipped)))
}

// back is the leader finding a host marked lost reachable again (R-419):
// the copies it holds of apps failed over away from it are removed, never
// started, and only once every one is gone is its lost mark cleared, so a
// removal that fails is tried again the next round. An app that was not
// failed over (its promotion failed, or it had no standby) is not touched:
// it is still homed there, and the reconciler starts it again (R-148).
func (s *Service) back(ctx context.Context, hostID string) error {
	if s.Returns == nil {
		return nil
	}
	apps, err := s.Returns.AppsLeftOn(ctx, hostID)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if err := s.Returns.RemoveStale(ctx, app); err != nil {
			return err
		}
	}
	if err := s.Store.ClearHostLost(ctx, hostID); err != nil {
		return err
	}
	if apps == nil {
		apps = []string{}
	}
	s.write(ctx, system, audit.Event{
		Action: "cluster.host.returned", TargetKind: "host", TargetID: hostID,
		Detail: map[string]any{"removed_copies": apps},
	})
	return nil
}

func (s *Service) lose(ctx context.Context, h state.Host, by Actor, trigger string) (HostLoss, error) {
	newly, err := s.Store.MarkHostLost(ctx, h.ID, by.ID)
	if err != nil {
		return HostLoss{}, err
	}
	out := HostLoss{HostID: h.ID, Name: h.Name, AlreadyLost: !newly, Started: []string{}, Skipped: []SkippedApp{}}
	if !newly && trigger == triggerMajority {
		return out, nil
	}
	s.write(ctx, by, audit.Event{
		Action: "cluster.host.lost", TargetKind: "host", TargetID: h.ID,
		Detail: map[string]any{"name": h.Name, "trigger": trigger, "already_lost": !newly},
	})
	s.failover(ctx, h, by, trigger, &out)
	return out, nil
}

func (s *Service) failover(ctx context.Context, h state.Host, by Actor, trigger string, out *HostLoss) {
	if s.Homes == nil || s.Failover == nil {
		out.Skipped = append(out.Skipped, SkippedApp{Reason: SkipNotWired,
			Detail: "This installation does not keep standbys yet, so no app was started elsewhere."})
		return
	}
	homed, err := s.Homes.AppsHomedOn(ctx, h.ID)
	if err != nil {
		out.Skipped = append(out.Skipped, SkippedApp{Reason: SkipPromoteErr, Detail: messageOf(err)})
		return
	}
	for _, a := range homed {
		reason, detail := s.skip(ctx, a)
		if reason != "" {
			out.Skipped = append(out.Skipped, SkippedApp{AppID: a.AppID, Reason: reason, Detail: detail})
			continue
		}
		// Written before the standby starts (R-228).
		s.write(ctx, by, audit.Event{
			Action: "cluster.failover", AppID: a.AppID, TargetKind: "app", TargetID: a.AppID,
			Detail: map[string]any{"from_host": h.ID, "from_host_name": h.Name, "trigger": trigger},
		})
		if err := s.Failover.PromoteStandby(ctx, a.AppID); err != nil {
			out.Skipped = append(out.Skipped, SkippedApp{AppID: a.AppID, Reason: SkipPromoteErr, Detail: messageOf(err)})
			continue
		}
		out.Started = append(out.Started, a.AppID)
	}
}

// skip says why app is not failed over, or "" when it is.
func (s *Service) skip(ctx context.Context, a HomedApp) (reason, detail string) {
	if !a.HasStandby {
		return SkipNoStandby, ""
	}
	app, ok, err := s.Apps.ByID(ctx, a.AppID)
	switch {
	case err != nil:
		return SkipPromoteErr, messageOf(err)
	case !ok:
		return SkipGone, ""
	case app.State == state.StateFailed:
		// R-151: a failed app stays failed until a human intervenes, and
		// failing it over would be Pando retrying it on another host.
		return SkipFailed, "A failed app is not failed over. Fix it and deploy it again."
	}
	return "", ""
}

// RecordFence audits a fence a host reported, once per fence however often
// the leader hears of it (R-421).
func (s *Service) RecordFence(ctx context.Context, hostID string, v View) {
	if v.FenceID == "" {
		return
	}
	newly, err := s.Store.RecordHostFence(ctx, hostID, v.FenceID)
	if err != nil || !newly {
		if err != nil {
			s.logger().Warn("could not record a host's fence", zap.String("host_id", hostID), zap.Error(err))
		}
		return
	}
	s.write(ctx, system, audit.Event{
		Action: "cluster.host.fence", TargetKind: "host", TargetID: hostID,
		Detail: map[string]any{"fence_id": v.FenceID, "stopped": v.Stopped, "still_cut_off": v.Fenced},
	})
}

func (s *Service) write(ctx context.Context, by Actor, e audit.Event) {
	if s.Audit == nil {
		return
	}
	e.PrincipalKind, e.PrincipalID, e.OnBehalfOf, e.RequestID = by.Kind, by.ID, by.OnBehalfOf, by.RequestID
	if err := s.Audit.Write(ctx, e); err != nil {
		s.logger().Error("audit write failed", zap.String("action", e.Action), zap.Error(err))
	}
}

// messageOf is err's message for a person: the envelope's, or a plain one
// for an error that carries none.
func messageOf(err error) string {
	if e := errs.As(err); e != nil {
		return e.Message
	}
	return "Pando could not start the standby: " + err.Error()
}
