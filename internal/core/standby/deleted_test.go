package standby

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// TestR419_AnAppDeletedWhileItsHostIsLostIsCleanedUp asserts that deleting
// an app whose host is lost does not leave its standby, or the copy a
// failover left, behind: the standby is removed from every host that
// answers, and tried again until every one has; a copy left on a lost host
// is removed when the host returns, with the delete's choice about the
// app's storage (R-204), and never started (R-148, R-419). The app's own
// copy on its lost home is the teardown's, with the rest of the app.
func TestR419_AnAppDeletedWhileItsHostIsLostIsCleanedUp(t *testing.T) {
	ctx := context.Background()
	r := newFailoverRig(t)
	r.store.apps = nil
	r.addApp("app_home", 0, state.StateRunning)
	r.addApp("app_moved", 0, state.StateRunning)
	r.svc.Once(ctx)
	require.Equal(t, "app-2", r.store.rows["app_home"].Host)

	// app_home's home, app-1, is lost; app_moved was failed over from app-1
	// to app-2 before, and has a new standby on app-3.
	r.store.rows["app_moved"] = state.Standby{AppID: "app_moved", RuntimeRef: "rt_hosts", Host: "app-3", Home: "app-2", StaleHost: "app-1"}
	r.cluster.lost["host_app-1"] = true
	r.store.discard = map[string]bool{"app_moved": false}
	for i := range r.store.apps {
		r.store.apps[i].Deleted = true
		r.store.apps[i].HomeLost = r.store.apps[i].AppID == "app_home"
		delete(r.store.states, r.store.apps[i].AppID)
	}

	// A host that does not answer keeps the standby's row, for the next pass.
	r.rt.removeErr = errs.New(errs.AdapterUnavailable, "Docker on app-1 did not answer.")
	r.svc.Once(ctx)
	require.ElementsMatch(t, []string{"app_home", "app_moved"}, r.rt.removed, "removed from every host that answers")
	require.Contains(t, r.store.rows, "app_home")
	require.Equal(t, "app-3", r.store.rows["app_moved"].Host)

	r.rt.removeErr = nil
	r.svc.Once(ctx)
	require.NotContains(t, r.store.rows, "app_home", "nothing left of it to remember")
	moved := r.store.rows["app_moved"]
	require.Empty(t, moved.Host, "its standby is gone")
	require.Equal(t, "app-1", moved.StaleHost, "the copy on the lost host is remembered until the host returns")

	// Passes while the host stays lost change nothing.
	removed := len(r.rt.removed)
	r.svc.Once(ctx)
	require.Len(t, r.rt.removed, removed)

	// The host returns: the copy is removed, never started, keeping its
	// volumes as the delete said (R-204), and the row is forgotten.
	w := r.watch(map[string]bool{"host_app-1": true, "host_app-2": true, "host_app-3": true})
	w.Round(ctx)
	require.Equal(t, []string{"app_moved@app-1"}, r.rt.stale)
	require.Equal(t, []bool{true}, r.rt.staleKept, "the delete kept the app's storage")
	require.Equal(t, []bool{true}, r.rt.staleDeleted)
	require.NotContains(t, r.store.rows, "app_moved")
	require.False(t, r.cluster.lost["host_app-1"])
	require.Empty(t, r.rt.promoted, "nothing is started")
	for _, p := range r.rt.applied {
		require.NotEqual(t, "app-1", p.Host)
	}
}

// TestADeletedAppsPromotionUnderWayEndsBeforeItsStandbyIsRemoved covers a
// deleted app whose promotion was under way: left while a promotion could
// still be running, then cleared and removed.
func TestADeletedAppsPromotionUnderWayEndsBeforeItsStandbyIsRemoved(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 1))
	at := h.svc.now()
	h.store.apps[0].Deleted = true
	delete(h.store.states, "app_a")
	h.store.rows["app_a"] = state.Standby{AppID: "app_a", RuntimeRef: "rt_hosts", Host: "app-2", Home: "app-2", PromotingAt: &at, PromotedAt: &at}

	h.svc.Once(ctx)
	require.Empty(t, h.rt.removed, "a promotion may still be running")
	require.Contains(t, h.store.rows, "app_a")

	h.clock.Advance(h.svc.promoteGrace() + time.Second)
	h.svc.Once(ctx)
	require.Equal(t, []string{"app_a"}, h.rt.removed)
	require.NotContains(t, h.store.rows, "app_a")
}
