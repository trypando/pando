package standby

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// placed is a harness whose app has a standby on app-2 and runs on app-1.
func placed(t *testing.T) *harness {
	t.Helper()
	h := newHarness(appSpec("", 1))
	h.svc.Once(context.Background())
	sb := h.store.rows["app_a"]
	require.Equal(t, "app-2", sb.Host)
	require.Equal(t, "app-1", sb.Home)
	return h
}

// TestR153_AStandbyPassNeverPutsBackAPromotedAppsOldHome asserts R-153 for
// two writers of one app's standby: a pass working from a row read before a
// promotion (another replica's, here made while the pass refreshes the
// standby) loses, and the promoted home stays; and a pass leaves alone an app
// whose promotion is under way or whose home is lost, so nothing moves,
// ships or removes the copies a promotion starts from.
func TestR153_AStandbyPassNeverPutsBackAPromotedAppsOldHome(t *testing.T) {
	ctx := context.Background()
	h := placed(t)
	h.store.apps[0].PinnedSpecID = "spec_2"
	promoted := time.Date(2026, 10, 26, 12, 30, 0, 0, time.UTC)
	h.rt.onApply = func() {
		sb := h.store.rows["app_a"]
		sb.Home, sb.StaleHost, sb.Host, sb.PromotedAt = "app-2", "app-1", "", &promoted
		sb.Version++ // another process's write
		h.store.rows["app_a"] = sb
	}
	h.svc.Once(ctx)
	sb := h.store.rows["app_a"]
	require.Equal(t, "app-2", sb.Home, "the promotion's home is kept")
	require.Equal(t, "app-1", sb.StaleHost)
	require.Empty(t, sb.Host)
	require.Equal(t, 1, h.store.lost, "the pass's write lost")
	h.rt.onApply = nil

	// Under way: nothing is applied, shipped or removed.
	for _, hold := range []func(*state.StandbyApp, *state.Standby){
		func(_ *state.StandbyApp, sb *state.Standby) { at := h.clock.Now(); sb.PromotingAt = &at },
		func(app *state.StandbyApp, _ *state.Standby) { app.HomeLost = true },
	} {
		h := placed(t)
		h.store.apps[0].PinnedSpecID = "spec_2"
		h.plans.specs["app_a"] = appSpec("off", 1)
		sb := h.store.rows["app_a"]
		hold(&h.store.apps[0], &sb)
		h.store.rows["app_a"] = sb
		applied, shipped := len(h.rt.applied), len(h.rt.shipped)
		h.clock.Advance(time.Hour)
		h.svc.Once(ctx)
		require.Len(t, h.rt.applied, applied)
		require.Len(t, h.rt.shipped, shipped)
		require.Empty(t, h.rt.removed)
		require.Contains(t, h.store.rows, "app_a")
	}
}

// TestR153_APromotionInThisProcessHoldsTheAppsStandby asserts the in-process
// half: while a promotion runs, a pass touches nothing of the app's.
func TestR153_APromotionInThisProcessHoldsTheAppsStandby(t *testing.T) {
	ctx := context.Background()
	h := placed(t)
	h.rt.promoteOK = api.PromoteResult{Host: "app-2", FormerHost: "app-1", Healthy: true}
	h.store.apps[0].PinnedSpecID = "spec_2"
	h.clock.Advance(time.Hour)
	applied := len(h.rt.applied)
	h.rt.onPromote = func() {
		// The row says the promotion is under way, so even another process's
		// pass leaves it; and this process's pass waits for nothing.
		require.NotNil(t, h.store.rows["app_a"].PromotingAt)
		_, held := h.svc.tryLock("app_a")
		require.False(t, held)
		h.svc.Once(ctx)
	}
	_, err := h.svc.Promote(ctx, "app_a", "")
	require.NoError(t, err)
	require.Len(t, h.rt.applied, applied, "the pass did not refresh the standby being started")
	require.Empty(t, h.rt.removed)
}

// TestR153_APromotionCutShortLeavesTheHomeOnTheNewHost asserts R-153 for a
// promotion that does not finish: it is written ahead, so a process that
// stops while the runtime starts the app has recorded its new home; an error
// after the runtime started it there keeps that home; and only a refusal
// before anything started puts the row back.
func TestR153_APromotionCutShortLeavesTheHomeOnTheNewHost(t *testing.T) {
	ctx := context.Background()

	// Written ahead: what a process stopped mid-start leaves.
	h := placed(t)
	var mid state.Standby
	h.rt.onPromote = func() { mid = h.store.rows["app_a"] }
	h.rt.promoteOK = api.PromoteResult{Host: "app-2", FormerHost: "app-1"}
	h.rt.promoteFail = errors.New("lost the lease")
	_, err := h.svc.Promote(ctx, "app_a", "")
	require.Error(t, err)
	require.Equal(t, "app-2", mid.Home, "the new home is recorded before the start")
	require.Equal(t, "app-1", mid.StaleHost)
	require.NotNil(t, mid.PromotingAt)
	sb := h.store.rows["app_a"]
	require.Equal(t, "app-2", sb.Home, "an error after the start keeps the new home")
	require.Equal(t, "app-1", sb.StaleHost)
	require.Nil(t, sb.PromotingAt)
	require.Empty(t, sb.Host)
	require.Equal(t, "lost the lease", sb.LastError)

	// Refused before anything started: as it was.
	h = placed(t)
	before := h.store.rows["app_a"]
	h.rt.promoteErr = map[string]error{"app_a": errs.New(errs.CapacityWouldOversubscribe, "no room")}
	_, err = h.svc.Promote(ctx, "app_a", "")
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	after := h.store.rows["app_a"]
	require.Greater(t, after.Version, before.Version, "written ahead, then put back")
	after.Version = before.Version
	require.Equal(t, before, after)
	require.Contains(t, h.auditor.actions, "app.standby.promote.failed")

	// A row changed since it was read is not promoted over.
	h = placed(t)
	h.store.rows["app_a"] = func() state.Standby { sb := h.store.rows["app_a"]; sb.Home = "app-3"; sb.Version++; return sb }()
	h.svc.Store = &staleReads{fakeStore: h.store, row: before}
	_, err = h.svc.Promote(ctx, "app_a", "")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Empty(t, h.rt.promoted)
}

// staleReads answers Get with a row read earlier.
type staleReads struct {
	*fakeStore
	row state.Standby
}

func (s *staleReads) Get(context.Context, string) (state.Standby, bool, error) {
	return s.row, true, nil
}

// TestR153_APromotionCutShortIsFinishedByTheNextPass asserts a promotion a
// stopped process left under way is finished once it has been longer than a
// promotion takes, and not before; and a failed app's is not (R-151).
func TestR153_APromotionCutShortIsFinishedByTheNextPass(t *testing.T) {
	ctx := context.Background()
	h := placed(t)
	sb := h.store.rows["app_a"]
	at := h.clock.Now()
	sb.Home, sb.StaleHost, sb.PromotingAt, sb.PromotedAt = "app-2", "app-1", &at, &at
	h.store.rows["app_a"] = sb
	h.rt.promoteOK = api.PromoteResult{Host: "app-2", Healthy: true}

	h.clock.Advance(time.Minute)
	h.svc.Once(ctx)
	require.Empty(t, h.rt.promoted, "maybe still running elsewhere")

	h.store.apps[0].State = state.StateFailed
	h.clock.Advance(h.svc.promoteGrace())
	h.svc.Once(ctx)
	require.Empty(t, h.rt.promoted, "R-151")

	h.store.apps[0].State = state.StateRunning
	h.svc.Once(ctx)
	require.Len(t, h.rt.promoted, 1)
	done := h.store.rows["app_a"]
	require.Nil(t, done.PromotingAt)
	require.Equal(t, "app-2", done.Home)
	require.Equal(t, "app-1", done.StaleHost)

	// Refused when tried again: back to the host it left, so the failover
	// can be tried again.
	h = placed(t)
	sb = h.store.rows["app_a"]
	sb.Home, sb.StaleHost, sb.PromotingAt, sb.PromotedAt = "app-2", "app-1", &at, &at
	h.store.rows["app_a"] = sb
	h.rt.promoteErr = map[string]error{"app_a": errors.New("no copy")}
	h.clock.Advance(time.Hour)
	h.svc.Once(ctx)
	back := h.store.rows["app_a"]
	require.Nil(t, back.PromotingAt)
	require.Equal(t, "app-1", back.Home)
	require.Empty(t, back.StaleHost)
	require.Equal(t, "app-2", back.Host, "the standby is still there to start")
}

// TestR419_AReturningHostWaitsForAPromotionUnderWay asserts a host that comes
// back while an app's promotion away from it is under way keeps its copy, and
// is told to try again, so it stays marked lost until the start is done.
func TestR419_AReturningHostWaitsForAPromotionUnderWay(t *testing.T) {
	ctx := context.Background()
	h := placed(t)
	sb := h.store.rows["app_a"]
	at := h.clock.Now()
	sb.Home, sb.StaleHost, sb.PromotingAt = "app-2", "app-1", &at
	h.store.rows["app_a"] = sb
	err := h.svc.RemoveStale(ctx, "app_a", "")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Empty(t, h.rt.stale)
}

// TestR419_ASingleHostInstallDoesNoStandbyWork asserts the pass builds no
// app's plan when no runtime can keep a standby, and still clears a row left
// from before.
func TestR419_ASingleHostInstallDoesNoStandbyWork(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 1))
	h.rt.supports = false
	h.svc.RuntimeRefs = func() []string { return []string{"rt_hosts"} }
	h.svc.Once(ctx)
	require.Zero(t, h.plans.shaped)
	require.Empty(t, h.rt.applied)

	h.store.rows["app_a"] = state.Standby{AppID: "app_a", RuntimeRef: "rt_hosts", Host: "app-2"}
	h.svc.Once(ctx)
	require.NotContains(t, h.store.rows, "app_a", "a row left from before is forgotten")

	// A fenced host does nothing at all (R-421).
	h = newHarness(appSpec("", 1))
	h.svc.LocalFenced = func() bool { return true }
	h.svc.Once(ctx)
	require.Zero(t, h.plans.shaped)
}

// TestR421_APromotionCutShortIsNotFinishedWithoutItsAuditEvent asserts R-421
// and R-228 for the one start the liveness pass does not audit itself:
// finishing a promotion cut short is written to the audit log first, and
// waits for the next pass when it cannot be.
func TestR421_APromotionCutShortIsNotFinishedWithoutItsAuditEvent(t *testing.T) {
	ctx := context.Background()
	h := placed(t)
	sb := h.store.rows["app_a"]
	at := h.clock.Now()
	sb.Home, sb.StaleHost, sb.PromotingAt, sb.PromotedAt = "app-2", "app-1", &at, &at
	h.store.rows["app_a"] = sb
	h.rt.promoteOK = api.PromoteResult{Host: "app-2", Healthy: true}

	h.auditor.fail = true
	h.clock.Advance(h.svc.promoteGrace() + time.Minute)
	h.svc.Once(ctx)
	require.Empty(t, h.rt.promoted, "not started without its audit event")
	require.NotNil(t, h.store.rows["app_a"].PromotingAt, "still under way, for the next pass")

	h.auditor.fail = false
	h.svc.Once(ctx)
	require.Len(t, h.rt.promoted, 1)
	require.Contains(t, h.auditor.actions, "cluster.failover")
}

// TestR151_AnAppThatFailsWhileAPromotionWaitsIsNotPromoted asserts R-151 for
// an app that fails just after Promote reads its state: while the promotion
// waits for the app's lock, or between the read and the write-ahead. Its
// standby is not started, the refusal says why, and the row is as it was.
func TestR151_AnAppThatFailsWhileAPromotionWaitsIsNotPromoted(t *testing.T) {
	ctx := context.Background()
	h := placed(t)
	before := h.store.rows["app_a"]
	h.rt.promoteOK = api.PromoteResult{Host: "app-2", FormerHost: "app-1", Healthy: true}
	h.store.stateRead = func() {
		h.store.states["app_a"] = state.StateFailed
		h.store.stateRead = nil
	}
	_, err := h.svc.Promote(ctx, "app_a", "")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "has failed")
	require.Empty(t, h.rt.promoted)
	require.Equal(t, before, h.store.rows["app_a"])
}
