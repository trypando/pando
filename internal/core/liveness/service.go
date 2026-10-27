package liveness

import (
	"context"
	"fmt"
	"sync"

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
	// SkipAuditErr is a failover whose audit event could not be written:
	// it is not started without one (R-421, R-228), and the leader tries
	// again each round until it is.
	SkipAuditErr = "audit_unavailable"
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

	// Witness says whether a host has been heard from lately. A host that
	// has is not gone, and is not marked lost by hand: its apps may be
	// running, and a standby started beside them would be a second copy
	// (R-153). Nil skips the check.
	Witness interface {
		Silent(ctx context.Context, h state.Host) error
	}

	mu       sync.Mutex
	inflight map[string]int
	running  sync.WaitGroup

	// retry is each lost host with an app whose failover was not audited,
	// so not started, and who asked for it: the leader's next round tries
	// it again (refail).
	retry map[string]asked
}

// asked is who asked for a host's failover, and how.
type asked struct {
	by      Actor
	trigger string
}

// Settle waits for the failovers under way to end.
func (s *Service) Settle() { s.running.Wait() }

// promoting marks a failover from hostID begun, or ended with done.
func (s *Service) promoting(hostID string, delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight == nil {
		s.inflight = map[string]int{}
	}
	s.inflight[hostID] += delta
	if s.inflight[hostID] <= 0 {
		delete(s.inflight, hostID)
	}
}

// failingOver reports whether a failover from hostID is under way here.
func (s *Service) failingOver(hostID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[hostID] > 0
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
	if s.Witness != nil {
		if err := s.Witness.Silent(ctx, h); err != nil {
			return HostLoss{}, err
		}
	}
	out, _, err := s.mark(ctx, h, by, triggerManual)
	if err != nil {
		return HostLoss{}, err
	}
	s.promoting(h.ID, 1)
	defer s.promoting(h.ID, -1)
	s.failover(ctx, h, asked{by, triggerManual}, &out)
	return out, nil
}

// declare is the leader taking a host for lost by the majority rule. Only
// the first time: a host already lost has had its failover. The host is
// marked here; its apps' standbys start beside the leader's round, which goes
// on probing, auditing fences and welcoming hosts back meanwhile (R-421).
func (s *Service) declare(ctx context.Context, hostID string) {
	h, err := s.Store.Host(ctx, hostID)
	if err != nil {
		s.logger().Warn("could not read a host a majority has not reached", zap.String("host_id", hostID), zap.Error(err))
		return
	}
	out, newly, err := s.mark(ctx, h, system, triggerMajority)
	if err != nil {
		s.logger().Error("could not take a host for lost", zap.String("host_id", hostID), zap.Error(err))
		return
	}
	if !newly {
		return
	}
	s.background(ctx, h, asked{system, triggerMajority}, out, "a majority of the cluster's hosts has not reached a host; took it for lost")
}

// background runs h's failover beside the leader's round, and logs what it
// did.
func (s *Service) background(ctx context.Context, h state.Host, a asked, out HostLoss, msg string) {
	s.promoting(h.ID, 1)
	s.running.Add(1)
	go func() {
		defer s.running.Done()
		defer s.promoting(h.ID, -1)
		s.failover(ctx, h, a, &out)
		s.logger().Warn(msg, zap.String("host_id", h.ID), zap.Strings("started", out.Started), zap.Int("skipped", len(out.Skipped)))
	}()
}

// refail tries again, beside the leader's round, the failover of each host
// still lost whose apps were not started for want of their audit event
// (SkipAuditErr), as whoever asked for it.
func (s *Service) refail(ctx context.Context, lost map[string]bool) {
	s.mu.Lock()
	again := make(map[string]asked, len(s.retry))
	for hostID, a := range s.retry {
		switch {
		case !lost[hostID]:
			delete(s.retry, hostID)
		case s.inflight[hostID] == 0:
			again[hostID] = a
		}
	}
	s.mu.Unlock()
	for hostID, a := range again {
		h, err := s.Store.Host(ctx, hostID)
		if err != nil {
			s.logger().Warn("could not read a lost host to start its apps' standbys again", zap.String("host_id", hostID), zap.Error(err))
			continue
		}
		out := HostLoss{HostID: h.ID, Name: h.Name, AlreadyLost: true, Started: []string{}, Skipped: []SkippedApp{}}
		s.background(ctx, h, a, out, "tried again the failover of a lost host's apps, which could not be audited before")
	}
}

// retryLater records whether h's failover is to be tried again, as a.
func (s *Service) retryLater(hostID string, a asked, again bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !again {
		delete(s.retry, hostID)
		return
	}
	if s.retry == nil {
		s.retry = map[string]asked{}
	}
	s.retry[hostID] = a
}

// back is the leader finding a host marked lost reachable again (R-419):
// the copies it holds of apps failed over away from it are removed, never
// started, and only once every one is gone is its lost mark cleared, so a
// removal that fails is tried again the next round. An app that was not
// failed over (its promotion failed, or it had no standby) is not touched:
// it is still homed there, and the reconciler starts it again (R-148).
//
// Not while a failover from the host is under way: an app being promoted is
// not yet among those left on it, and clearing the mark now would leave its
// copy there for good. The next round tries again.
func (s *Service) back(ctx context.Context, hostID string) error {
	if s.Returns == nil || s.failingOver(hostID) {
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
	// The copies are removed and the mark cleared; an event that cannot be
	// written now is logged, and changes nothing that was done.
	_ = s.write(ctx, system, audit.Event{
		Action: "cluster.host.returned", TargetKind: "host", TargetID: hostID,
		Detail: map[string]any{"removed_copies": apps},
	})
	return nil
}

// mark audits h lost and records it, and reports whether it was not lost
// before. The leader acts only on a host not lost when read; a person, every
// time. The audit event is written first, and the host is not marked when it
// cannot be (R-421, R-228): a loss the log does not show starts nothing. The
// leader's next round tries again.
func (s *Service) mark(ctx context.Context, h state.Host, by Actor, trigger string) (HostLoss, bool, error) {
	out := HostLoss{HostID: h.ID, Name: h.Name, AlreadyLost: h.Lost, Started: []string{}, Skipped: []SkippedApp{}}
	if h.Lost && trigger == triggerMajority {
		return out, false, nil
	}
	if err := s.write(ctx, by, audit.Event{
		Action: "cluster.host.lost", TargetKind: "host", TargetID: h.ID,
		Detail: map[string]any{"name": h.Name, "trigger": trigger, "already_lost": h.Lost},
	}); err != nil {
		return HostLoss{}, false, errs.Wrap(errs.Internal, fmt.Sprintf(
			"Pando could not write to the audit log that host %s (%s) is lost, so it did not mark the host lost or start any app elsewhere.", h.Name, h.ID), err).
			WithRemedy("Check that the database accepts writes, then mark the host lost again.")
	}
	newly, err := s.Store.MarkHostLost(ctx, h.ID, by.ID)
	if err != nil {
		return HostLoss{}, false, err
	}
	out.AlreadyLost = !newly
	return out, newly, nil
}

// failover starts the standby of each app homed on h, each only once its
// cluster.failover event is written (R-421, R-228). An app whose event
// cannot be written is skipped, and the leader tries the host again next
// round (refail).
func (s *Service) failover(ctx context.Context, h state.Host, a asked, out *HostLoss) {
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
	unaudited := false
	for _, app := range homed {
		reason, detail := s.skip(ctx, app)
		if reason == "" {
			reason, detail = s.start(ctx, h, a, app.AppID)
		}
		switch reason {
		case "":
			out.Started = append(out.Started, app.AppID)
		case SkipAuditErr:
			unaudited = true
			fallthrough
		default:
			out.Skipped = append(out.Skipped, SkippedApp{AppID: app.AppID, Reason: reason, Detail: detail})
		}
	}
	s.retryLater(h.ID, a, unaudited)
}

// start audits one app's failover and then starts its standby, and says why
// not when it did not. Nothing starts without its event (R-421, R-228).
func (s *Service) start(ctx context.Context, h state.Host, a asked, appID string) (reason, detail string) {
	if err := s.write(ctx, a.by, audit.Event{
		Action: "cluster.failover", AppID: appID, TargetKind: "app", TargetID: appID,
		Detail: map[string]any{"from_host": h.ID, "from_host_name": h.Name, "trigger": a.trigger},
	}); err != nil {
		return SkipAuditErr, "Pando could not write this failover to the audit log, so it did not start the standby. It tries again shortly."
	}
	if err := s.Failover.PromoteStandby(ctx, appID); err != nil {
		return SkipPromoteErr, messageOf(err)
	}
	return "", ""
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
	_ = s.write(ctx, system, audit.Event{
		Action: "cluster.host.fence", TargetKind: "host", TargetID: hostID,
		Detail: map[string]any{"fence_id": v.FenceID, "stopped": v.Stopped, "still_cut_off": v.Fenced},
	})
}

// write audits e as by, and returns the error when it could not be written,
// having logged it.
func (s *Service) write(ctx context.Context, by Actor, e audit.Event) error {
	if s.Audit == nil {
		return nil
	}
	e.PrincipalKind, e.PrincipalID, e.OnBehalfOf, e.RequestID = by.Kind, by.ID, by.OnBehalfOf, by.RequestID
	err := s.Audit.Write(ctx, e)
	if err != nil {
		s.logger().Error("audit write failed", zap.String("action", e.Action), zap.Error(err))
	}
	return err
}

// messageOf is err's message for a person: the envelope's, or a plain one
// for an error that carries none.
func messageOf(err error) string {
	if e := errs.As(err); e != nil {
		return e.Message
	}
	return "Pando could not start the standby: " + err.Error()
}
