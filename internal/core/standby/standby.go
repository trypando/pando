// Package standby keeps each app's warm standby (R-419, R-420,
// notes-cluster.md "Warm standby").
//
// The pass runs on the leader. For each app on a runtime that can keep a
// standby, and that host policy or its own spec says should have one, it has
// the runtime make or refresh the stopped standby when the app's pinned spec
// changed, and ships the app's volumes to it when a copy is due [P: every 5
// minutes]. An app whose standby is turned off, or that is deleted, has its
// standby removed; a deleted app's copy a failover left on a lost host is
// kept on record until the host returns and it is removed (RemoveStale).
// Nothing here decides a host is lost: that is the liveness pass's (R-421),
// which calls Promote.
//
// A failed app is not failed over (R-151): Promote refuses it, and the pass
// leaves its standby as it was rather than refreshing it.
//
// One app's standby has one writer at a time (R-153). In this process the
// pass and a promotion take the app's lock (Service.lock); across processes
// every write is conditional on the row being as it was read (SaveIf, on the
// row's version), so a pass working from a reading taken before a promotion
// never puts the old home back. The pass leaves alone an app whose home is lost or whose
// promotion is under way: that standby is the failover's.
package standby

import (
	"context"
	"sort"
	"sync"
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
	// SaveIf writes sb only if the row is still prev (nil: no row), and
	// reports whether it did.
	SaveIf(ctx context.Context, sb state.Standby, prev *state.Standby) (bool, error)
	// SaveIfLive is SaveIf that also writes only while the app is live and
	// not failed: a promotion's write-ahead (R-151).
	SaveIfLive(ctx context.Context, sb state.Standby, prev *state.Standby) (bool, error)
	// Delete forgets the app's standby, unless a promotion of it is under
	// way.
	Delete(ctx context.Context, appID string) error
	// Fate reads whether the app is deleted, and what its delete said of
	// its storage (R-204).
	Fate(ctx context.Context, appID string) (state.AppFate, error)
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

	// RuntimeRefs lists the runtime adapters. When none of them can keep a
	// standby, the pass builds no app's plan: a single-host install does no
	// standby work at all. Nil asks each app's runtime.
	RuntimeRefs func() []string

	// LocalFenced reports whether this host is fenced (R-421): cut off from
	// most of the cluster, so nothing here is placed, shipped or started.
	// Nil is never.
	LocalFenced func() bool

	Clock  clock.Clock
	Logger *zap.Logger

	// Every and ShipEvery override the defaults; Keep is the copies kept
	// (zero is api.DefaultStandbyCopies), HealthTimeout what a promoted copy
	// is given to come up (zero is the runtime's default).
	Every         time.Duration
	ShipEvery     time.Duration
	Keep          int
	HealthTimeout time.Duration

	locks sync.Map // app ID -> *sync.Mutex
}

// lock takes the app's standby for a promotion, waiting for the pass.
func (s *Service) lock(appID string) func() {
	mu := s.mutex(appID)
	mu.Lock()
	return mu.Unlock
}

// tryLock takes the app's standby for the pass unless a promotion has it.
func (s *Service) tryLock(appID string) (func(), bool) {
	mu := s.mutex(appID)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

func (s *Service) mutex(appID string) *sync.Mutex {
	mu, _ := s.locks.LoadOrStore(appID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func (s *Service) fenced() bool { return s.LocalFenced != nil && s.LocalFenced() }

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
	if s.fenced() {
		return // R-421: a host cut off from most of the cluster changes nothing
	}
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
	possible := s.anyStandbyRuntime(ctx)
	for _, app := range apps {
		if ctx.Err() != nil {
			return
		}
		if !possible && app.Standby == nil {
			continue // no runtime keeps standbys, and nothing is left to clear
		}
		s.visit(ctx, app, doc)
	}
}

// anyStandbyRuntime reports whether any runtime adapter can keep a standby;
// true when RuntimeRefs is not set, so each app's runtime is asked.
func (s *Service) anyStandbyRuntime(ctx context.Context) bool {
	if s.RuntimeRefs == nil {
		return true
	}
	for _, ref := range s.RuntimeRefs() {
		if _, ok := s.standbyOf(ctx, ref); ok {
			return true
		}
	}
	return false
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

func (s *Service) visit(ctx context.Context, app state.StandbyApp, doc policy.Document) {
	if app.Deleted {
		// Before the failover's check: a deleted app is never failed over,
		// so a lost home does not keep its standby (R-419).
		s.clearDeleted(ctx, app)
		return
	}
	if sb := app.Standby; sb != nil && (sb.PromotingAt != nil || app.HomeLost) {
		// R-153: the failover's. Placing, shipping or removing this standby
		// now could remove or overwrite what a promotion starts from.
		s.resume(ctx, app)
		return
	}
	unlock, ok := s.tryLock(app.AppID)
	if !ok {
		return // a promotion has it (R-153)
	}
	defer unlock()
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
	if !sp.Availability.WantsStandby(doc.KeepsStandbys()) {
		s.remove(ctx, app.AppID, app.Standby, "the standby was turned off")
		return
	}
	if app.State == state.StateFailed {
		return // R-151: left as it is, and never started
	}
	s.refresh(ctx, sr, plan, sp, app, ref, doc)
}

// refresh places or refreshes an app's standby, ships its volumes when they
// are due, and saves what happened (R-419, R-420).
func (s *Service) refresh(ctx context.Context, sr api.StandbyRuntime, plan api.BundlePlan, sp *spec.AppSpec,
	app state.StandbyApp, ref string, doc policy.Document) {
	sb, prev := state.Standby{AppID: app.AppID}, app.Standby
	if prev != nil {
		sb = *prev
	}
	sb.RuntimeRef = ref
	sb.Volumes = len(plan.Volumes)
	sb.Unreplicated = s.unreplicated(sp)
	// Host policy allow_failover_oversubscription reaches the runtime here:
	// the standby pass is what places standbys, and Shape, like the
	// reconciler, sends false (R-422).
	plan.Failover.AllowOversubscription = doc.AllowFailoverOversubscription
	if sb.Host == "" || sb.SpecID != app.PinnedSpecID {
		if !s.apply(ctx, sr, plan, sp, app.PinnedSpecID, &sb) {
			s.save(ctx, sb, prev)
			return
		}
	}
	if sb.Volumes > 0 && s.due(sb.ShippedAt) {
		s.ship(ctx, sr, plan, &sb)
	}
	s.save(ctx, sb, prev)
}

// apply makes or refreshes the standby, and reports whether it now exists.
func (s *Service) apply(ctx context.Context, sr api.StandbyRuntime, plan api.BundlePlan, sp *spec.AppSpec, specID string, sb *state.Standby) bool {
	if err := s.Plans.Complete(ctx, &plan, sp); err != nil {
		failed(sb, err)
		return false
	}
	handle, err := sr.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan, Host: sb.Host})
	if err != nil {
		// No room is recorded like any other failure, with its code: the
		// app keeps running, and Standby turns it into the app's notice. It
		// never blocks a deploy (R-422).
		failed(sb, err)
		return false
	}
	if handle.Host != sb.Host {
		// A new place starts with no copies: the first ship there is full.
		sb.Copies, sb.ShippedAt = 0, nil
		_ = s.audit(ctx, "app.standby.create", sb.AppID, "", map[string]any{"host": handle.Host, "previous_host": sb.Host})
	}
	sb.Host, sb.SpecID = handle.Host, specID
	if handle.Home != "" {
		sb.Home = handle.Home
	}
	cleared(sb)
	return true
}

func (s *Service) ship(ctx context.Context, sr api.StandbyRuntime, plan api.BundlePlan, sb *state.Standby) {
	ids := make([]string, 0, len(plan.Volumes))
	for _, v := range plan.Volumes {
		ids = append(ids, v.VolumeID)
	}
	got, err := sr.ShipVolumes(ctx, api.ShipRequest{BundleID: plan.BundleID, Host: sb.Host, VolumeIDs: ids, Keep: s.Keep})
	if err != nil {
		failed(sb, err)
		s.logger().Warn("could not ship an app's volumes to its standby", zap.String("app_id", sb.AppID), zap.Error(err))
		return
	}
	now := s.now()
	sb.ShippedAt, sb.Copies = &now, got.Copies
	cleared(sb)
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
// cannot be reached keeps the row, so the next pass tries again. Never
// during a promotion: the pass does not reach here for one (visit).
func (s *Service) remove(ctx context.Context, appID string, sb *state.Standby, why string) {
	if sb == nil || !s.removeStandby(ctx, appID, sb) {
		return
	}
	s.forget(ctx, appID)
	_ = s.audit(ctx, "app.standby.remove", appID, "", map[string]any{"host": sb.Host, "reason": why})
}

// removeStandby takes the standby off the runtime, from every host that
// answers, and reports whether every host did. A runtime no longer
// configured has nothing left to remove.
func (s *Service) removeStandby(ctx context.Context, appID string, sb *state.Standby) bool {
	sr, ok := s.standbyOf(ctx, sb.RuntimeRef)
	if !ok {
		return true
	}
	if err := sr.RemoveStandby(ctx, api.BundleRef{BundleID: appID}); err != nil {
		s.logger().Warn("could not remove an app's standby from every host; trying again next pass",
			zap.String("app_id", appID), zap.Error(err))
		return false
	}
	return true
}

const deletedReason = "the app was deleted"

// clearDeleted removes a deleted app's standby (R-419). Its own copy, on its
// home, is the reconciler's teardown, with the rest of the app and as the
// delete said of its storage (R-204), lost home or not. What is left here:
//
//   - a promotion under way is left to end, or to be taken for cut short
//     (promoteGrace), since it may be running in another process;
//   - the standby is removed from every host that answers, and tried again
//     each pass until every host has;
//   - a copy a failover left on another host (stale_host) is not the
//     teardown's: the row is kept, with no standby, until that host returns
//     and RemoveStale removes it. A host removed from the cluster instead
//     clears it (state.forgetStandbyHost), and the row then goes.
func (s *Service) clearDeleted(ctx context.Context, app state.StandbyApp) {
	sb := app.Standby
	if sb == nil {
		return
	}
	promoting := sb.PromotingAt != nil
	if promoting && s.now().Sub(*sb.PromotingAt) < s.promoteGrace() {
		return
	}
	if !promoting && sb.StaleHost != "" && sb.Host == "" {
		return // only the stale copy is left, for when its host returns
	}
	unlock, ok := s.tryLock(app.AppID)
	if !ok {
		return // a promotion has it (R-153)
	}
	defer unlock()
	if !promoting && sb.StaleHost == "" {
		s.remove(ctx, app.AppID, sb, deletedReason)
		return
	}
	if !s.removeStandby(ctx, app.AppID, sb) {
		return
	}
	left := *sb
	left.Host, left.SpecID, left.Copies, left.ShippedAt, left.PromotingAt = "", "", 0, nil, nil
	if !s.save(ctx, left, sb) {
		return
	}
	if left.StaleHost == "" {
		s.forget(ctx, app.AppID)
	}
	_ = s.audit(ctx, "app.standby.remove", app.AppID, "", map[string]any{"host": sb.Host, "reason": deletedReason})
}

func (s *Service) forget(ctx context.Context, appID string) {
	if err := s.Store.Delete(ctx, appID); err != nil {
		s.logger().Warn("could not forget an app's standby", zap.String("app_id", appID), zap.Error(err))
	}
}

// save writes sb over prev, the row as it was read. A row changed meanwhile
// — a promotion, by this process or another — is left as it now is, and the
// next pass reads it again (R-153).
func (s *Service) save(ctx context.Context, sb state.Standby, prev *state.Standby) bool {
	ok, err := s.Store.SaveIf(ctx, sb, prev)
	switch {
	case err != nil:
		s.logger().Warn("could not record an app's standby", zap.String("app_id", sb.AppID), zap.Error(err))
	case !ok:
		s.logger().Info("an app's standby changed while it was being kept; left as it now is",
			zap.String("app_id", sb.AppID))
	}
	return ok && err == nil
}

// audit writes one event, and returns the error when it could not be
// written, having logged it.
func (s *Service) audit(ctx context.Context, action, appID, by string, detail map[string]any) error {
	if s.Auditor == nil {
		return nil
	}
	e := audit.Event{PrincipalKind: audit.KindSystem, PrincipalID: "standby",
		Action: action, AppID: appID, TargetKind: "app", TargetID: appID, Detail: detail}
	if by != "" {
		e.PrincipalKind, e.PrincipalID = audit.KindUser, by
	}
	err := s.Auditor.Write(ctx, e)
	if err != nil {
		s.logger().Error("audit write failed", zap.String("action", action), zap.Error(err))
	}
	return err
}

// now is the clock's time to the microsecond, as Postgres keeps it, so a row
// written with it compares equal when read back (SaveIf).
func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC().Truncate(time.Microsecond)
	}
	return s.Clock.Now().UTC().Truncate(time.Microsecond)
}

func (s *Service) logger() *zap.Logger {
	if s.Logger == nil {
		return zap.NewNop()
	}
	return s.Logger
}

// failed records err on the standby: its message, and its code and remedy
// when it is an envelope.
func failed(sb *state.Standby, err error) {
	sb.LastError, sb.LastErrorCode, sb.LastErrorRemedy = message(err), "", ""
	if e := errs.As(err); e != nil {
		sb.LastErrorCode, sb.LastErrorRemedy = string(e.Code), e.Remedy
	}
}

// cleared forgets the standby's last error.
func cleared(sb *state.Standby) {
	sb.LastError, sb.LastErrorCode, sb.LastErrorRemedy = "", "", ""
}

// message is what an error says, for the standby record.
func message(err error) string {
	if e := errs.As(err); e != nil {
		return e.Message
	}
	return err.Error()
}
