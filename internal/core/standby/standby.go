// Package standby keeps each app's warm standby (R-419, R-420,
// notes-cluster.md "Warm standby").
//
// The pass runs on the leader. For each app on a runtime that can keep a
// standby, and that host policy or its own spec says should have one, it has
// the runtime make or refresh the stopped standby when the app's pinned spec
// changed, and ships the app's volumes to it when a copy is due [P: every 5
// minutes]. An app whose standby is turned off, or that is deleted, has its
// standby removed. Nothing here decides a host is lost: that is the
// liveness pass's (R-421), which calls Promote.
//
// A failed app is not failed over (R-151): Promote refuses it, and the pass
// leaves its standby as it was rather than refreshing it.
package standby

import (
	"context"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// DefaultEvery is how often the pass looks at every app, and DefaultShipEvery
// how old an app's newest copy may get before another is shipped (R-420 [P]).
const (
	DefaultEvery     = time.Minute
	DefaultShipEvery = 5 * time.Minute
)

// Store is the standby state. *state.Standbys is one.
type Store interface {
	Candidates(ctx context.Context) ([]state.StandbyApp, error)
	Get(ctx context.Context, appID string) (state.Standby, bool, error)
	Save(ctx context.Context, sb state.Standby) error
	Delete(ctx context.Context, appID string) error
}

// Plans builds what an app is started with. *reconciler.Reconciler is one.
// Shape is cheap and read on every pass; Complete resolves the pull
// credential and the environment, and is asked only before something is
// created from the plan.
type Plans interface {
	Shape(ctx context.Context, appID string) (api.BundlePlan, *spec.AppSpec, error)
	Complete(ctx context.Context, plan *api.BundlePlan, s *spec.AppSpec) error
}

// Runtimes resolves runtime adapters by reference.
type Runtimes interface {
	Runtime(ref string) (api.RuntimeAdapter, bool)
}

// Services resolves the services adapter that fills a slot type.
type Services interface {
	ServicesFor(t spec.SlotType) (api.ServicesAdapter, string, bool)
}

// PolicyLoader reads host policy.
type PolicyLoader interface {
	Load(ctx context.Context) (policy.Document, error)
}

// Auditor writes audit events.
type Auditor interface {
	Write(ctx context.Context, e audit.Event) error
}

// Service is the standby pass, and the actions on one app's standby.
type Service struct {
	Store    Store
	Plans    Plans
	Runtimes Runtimes
	Policy   PolicyLoader
	Apps     AppStates

	// Services and Auditor are optional. Without Services every provisioned
	// service counts as copied with the volumes.
	Services Services
	Auditor  Auditor

	Clock  clock.Clock
	Logger *zap.Logger

	// Every and ShipEvery override the defaults; Keep is the copies kept
	// (zero is api.DefaultStandbyCopies), HealthTimeout what a promoted copy
	// is given to come up (zero is the runtime's default).
	Every         time.Duration
	ShipEvery     time.Duration
	Keep          int
	HealthTimeout time.Duration
}

// Run passes until ctx is canceled.
func (s *Service) Run(ctx context.Context) {
	every := s.Every
	if every <= 0 {
		every = DefaultEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		s.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Once runs one pass.
func (s *Service) Once(ctx context.Context) {
	apps, err := s.Store.Candidates(ctx)
	if err != nil {
		s.logger().Warn("could not list apps for the standby pass", zap.Error(err))
		return
	}
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		s.logger().Warn("could not read host policy for the standby pass", zap.Error(err))
		return
	}
	for _, app := range apps {
		if ctx.Err() != nil {
			return
		}
		s.visit(ctx, app, doc.KeepsStandbys())
	}
}

// standbyOf is the app's runtime as a standby runtime, when it is one.
func (s *Service) standbyOf(ctx context.Context, ref string) (api.StandbyRuntime, bool) {
	rt, ok := s.Runtimes.Runtime(ref)
	if !ok {
		return nil, false
	}
	caps, err := rt.Capabilities(ctx)
	if err != nil || !caps.SupportsStandby {
		return nil, false
	}
	sr, ok := rt.(api.StandbyRuntime) // SupportsStandby says it is (R-254)
	return sr, ok
}

func (s *Service) visit(ctx context.Context, app state.StandbyApp, policyDefault bool) {
	if app.Deleted {
		s.remove(ctx, app.AppID, app.Standby, "the app was deleted")
		return
	}
	plan, sp, err := s.Plans.Shape(ctx, app.AppID)
	if err != nil {
		s.logger().Warn("could not build an app's plan for its standby", zap.String("app_id", app.AppID), zap.Error(err))
		return
	}
	ref := sp.Runtime.AdapterRef
	sr, ok := s.standbyOf(ctx, ref)
	if !ok {
		// A runtime with one place keeps none; a row left from before it
		// changed is forgotten. A standby on a runtime the app left is the
		// runtime swap's to remove, with the rest of the app (R-257).
		if app.Standby != nil {
			s.forget(ctx, app.AppID)
		}
		return
	}
	if !sp.Availability.WantsStandby(policyDefault) {
		s.remove(ctx, app.AppID, app.Standby, "the standby was turned off")
		return
	}
	if app.State == state.StateFailed {
		return // R-151: left as it is, and never started
	}

	sb := state.Standby{AppID: app.AppID}
	if app.Standby != nil {
		sb = *app.Standby
	}
	sb.RuntimeRef = ref
	sb.Volumes = len(plan.Volumes)
	sb.Unreplicated = s.unreplicated(sp)
	if sb.Host == "" || sb.SpecID != app.PinnedSpecID {
		if !s.apply(ctx, sr, plan, sp, app.PinnedSpecID, &sb) {
			s.save(ctx, sb)
			return
		}
	}
	if sb.Volumes > 0 && s.due(sb.ShippedAt) {
		s.ship(ctx, sr, plan, &sb)
	}
	s.save(ctx, sb)
}

// apply makes or refreshes the standby, and reports whether it now exists.
func (s *Service) apply(ctx context.Context, sr api.StandbyRuntime, plan api.BundlePlan, sp *spec.AppSpec, specID string, sb *state.Standby) bool {
	if err := s.Plans.Complete(ctx, &plan, sp); err != nil {
		sb.LastError = message(err)
		return false
	}
	handle, err := sr.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan, Host: sb.Host})
	if err != nil {
		sb.LastError = message(err)
		return false
	}
	if handle.Host != sb.Host {
		// A new place starts with no copies: the first ship there is full.
		sb.Copies, sb.ShippedAt = 0, nil
		s.audit(ctx, "app.standby.create", sb.AppID, "", map[string]any{"host": handle.Host, "previous_host": sb.Host})
	}
	sb.Host, sb.SpecID, sb.LastError = handle.Host, specID, ""
	return true
}

func (s *Service) ship(ctx context.Context, sr api.StandbyRuntime, plan api.BundlePlan, sb *state.Standby) {
	ids := make([]string, 0, len(plan.Volumes))
	for _, v := range plan.Volumes {
		ids = append(ids, v.VolumeID)
	}
	got, err := sr.ShipVolumes(ctx, api.ShipRequest{BundleID: plan.BundleID, Host: sb.Host, VolumeIDs: ids, Keep: s.Keep})
	if err != nil {
		sb.LastError = message(err)
		s.logger().Warn("could not ship an app's volumes to its standby", zap.String("app_id", sb.AppID), zap.Error(err))
		return
	}
	now := s.now()
	sb.ShippedAt, sb.Copies, sb.LastError = &now, got.Copies, ""
}

func (s *Service) due(shipped *time.Time) bool {
	every := s.ShipEvery
	if every <= 0 {
		every = DefaultShipEvery
	}
	return shipped == nil || !s.now().Before(shipped.Add(every))
}

// unreplicated is the app's provisioned slot types whose services adapter
// does not replicate them natively, so they travel in the volume copy
// (R-420).
func (s *Service) unreplicated(sp *spec.AppSpec) []string {
	seen := map[string]bool{}
	for _, slot := range sp.Slots {
		if slot.Resolution == nil || slot.Resolution.Mode != spec.ResolutionProvisioned {
			continue
		}
		if s.Services != nil {
			if a, _, ok := s.Services.ServicesFor(slot.Type); ok && a.Capabilities().Replicates(slot.Type) {
				continue
			}
		}
		seen[string(slot.Type)] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// remove takes the standby off the runtime and forgets it. A runtime that
// cannot be reached keeps the row, so the next pass tries again.
func (s *Service) remove(ctx context.Context, appID string, sb *state.Standby, why string) {
	if sb == nil {
		return
	}
	if sr, ok := s.standbyOf(ctx, sb.RuntimeRef); ok {
		if err := sr.RemoveStandby(ctx, api.BundleRef{BundleID: appID}); err != nil {
			s.logger().Warn("could not remove an app's standby", zap.String("app_id", appID), zap.Error(err))
			return
		}
	}
	s.forget(ctx, appID)
	s.audit(ctx, "app.standby.remove", appID, "", map[string]any{"host": sb.Host, "reason": why})
}

func (s *Service) forget(ctx context.Context, appID string) {
	if err := s.Store.Delete(ctx, appID); err != nil {
		s.logger().Warn("could not forget an app's standby", zap.String("app_id", appID), zap.Error(err))
	}
}

func (s *Service) save(ctx context.Context, sb state.Standby) {
	if err := s.Store.Save(ctx, sb); err != nil {
		s.logger().Warn("could not record an app's standby", zap.String("app_id", sb.AppID), zap.Error(err))
	}
}

func (s *Service) audit(ctx context.Context, action, appID, by string, detail map[string]any) {
	if s.Auditor == nil {
		return
	}
	e := audit.Event{PrincipalKind: audit.KindSystem, PrincipalID: "standby",
		Action: action, AppID: appID, TargetKind: "app", TargetID: appID, Detail: detail}
	if by != "" {
		e.PrincipalKind, e.PrincipalID = audit.KindUser, by
	}
	if err := s.Auditor.Write(ctx, e); err != nil {
		s.logger().Error("audit write failed", zap.String("action", action), zap.Error(err))
	}
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now().UTC()
}

func (s *Service) logger() *zap.Logger {
	if s.Logger == nil {
		return zap.NewNop()
	}
	return s.Logger
}

// message is what an error says, for the standby record.
func message(err error) string {
	if e := errs.As(err); e != nil {
		return e.Message
	}
	return err.Error()
}
