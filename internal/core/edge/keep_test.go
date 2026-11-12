package edge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// apiTimeout is what the Cloudflare adapter returned while its API stopped
// answering: the control plane failing, with the tunnel itself still up.
var apiTimeout = errs.New(errs.AdapterUnavailable, "Pando could not reach Cloudflare's API.")

// TestR174_AnEdgeIsKeptRunningWhenItsAdapterCannotBeChecked asserts R-174's
// "reconciles" does not mean "removes on any error": a routing adapter that is
// still configured but whose provider API is failing keeps the edge it already
// runs. Tearing the tunnel down made a slow API call an outage of the console
// and every app routed through it, and a restart could not bring it back.
func TestR174_AnEdgeIsKeptRunningWhenItsAdapterCannotBeChecked(t *testing.T) {
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}
	cf := &fakeRouting{needs: true, name: "cloudflared"}
	s := setup(t, rt, map[string]*fakeRouting{"rte_cloudflare": cf})

	require.NoError(t, s.Reconcile(context.Background()))
	require.Contains(t, rt.edges, "cloudflared")

	// The provider's API stops answering.
	cf.err = apiTimeout
	err := s.Reconcile(context.Background())
	require.Error(t, err, "the failure is still reported")
	require.Contains(t, rt.edges, "cloudflared", "the running edge is kept")
	require.Empty(t, rt.removed, "nothing was torn down")

	st, ok := s.Status("rte_cloudflare")
	require.True(t, ok)
	require.True(t, st.Running, "degraded, not down")
	require.Contains(t, st.Message, "kept the edge running on its last configuration")
	require.Contains(t, st.Message, "could not reach Cloudflare's API")

	// It keeps failing for many passes: still kept.
	for i := 0; i < 5; i++ {
		_ = s.Reconcile(context.Background())
	}
	require.Contains(t, rt.edges, "cloudflared")
	require.Empty(t, rt.removed)

	// The API comes back: applied normally, the warning clears.
	cf.err = nil
	require.NoError(t, s.Reconcile(context.Background()))
	st, _ = s.Status("rte_cloudflare")
	require.True(t, st.Running)
	require.Empty(t, st.Message)
}

// After a restart there is no remembered plan, so the edge an adapter that
// cannot be checked would have asked for is the one named for it — the name
// an edge gets when its plan sets none.
func TestR174_AnEdgeIsKeptAcrossARestartDuringAnOutage(t *testing.T) {
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{"rte_cloudflare": {}}}
	s := setup(t, rt, map[string]*fakeRouting{"rte_cloudflare": {needs: true, err: apiTimeout}})

	require.Error(t, s.Reconcile(context.Background()))
	require.Contains(t, rt.edges, "rte_cloudflare")
	require.Empty(t, rt.removed)
}

// A failure applying a changed plan keeps the edge that was running, rather
// than leaving the adapter with none.
func TestR174_AnEdgeIsKeptWhenApplyingItsNewPlanFails(t *testing.T) {
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}
	s := setup(t, rt, map[string]*fakeRouting{"rte_traefik": {needs: true}})
	require.NoError(t, s.Reconcile(context.Background()))

	rt.applyErr = errs.New(errs.AdapterFailed, "port 80 is taken.")
	require.Error(t, s.Reconcile(context.Background()))
	require.Contains(t, rt.edges, "rte_traefik")
	require.Empty(t, rt.removed)
	st, _ := s.Status("rte_traefik")
	require.True(t, st.Running)
	require.Contains(t, st.Message, "port 80 is taken.")
}

// Keeping is only for an adapter that is still configured. One that stops
// asking for an edge, or is removed, still has its edge removed — and a
// failing adapter keeps only its own edge, not one nothing asks for.
func TestR174_KeepingAnEdgeDoesNotKeepOnesNothingAsksFor(t *testing.T) {
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}
	cf := &fakeRouting{needs: true, name: "cloudflared"}
	s := setup(t, rt, map[string]*fakeRouting{"rte_cloudflare": cf})
	require.NoError(t, s.Reconcile(context.Background()))

	rt.edges["rte_gone"] = api.EdgePlan{}
	cf.err = apiTimeout
	_ = s.Reconcile(context.Background())
	require.Contains(t, rt.edges, "cloudflared")
	require.NotContains(t, rt.edges, "rte_gone", "an edge nothing asks for is still removed")

	// The adapter is switched to an operator-run edge: it asks for none.
	cf.err = nil
	cf.needs = false
	require.NoError(t, s.Reconcile(context.Background()))
	require.NotContains(t, rt.edges, "cloudflared")

	// And once it asked for none, a later failure has nothing to keep.
	cf.err = apiTimeout
	rt.edges["cloudflared"] = api.EdgePlan{}
	_ = s.Reconcile(context.Background())
	require.NotContains(t, rt.edges, "cloudflared")
}

// An adapter that renames its edge and then fails to apply the new one keeps
// the old, still-running edge: the new name is only remembered once it runs.
func TestR174_ARenamedEdgeThatFailsToApplyKeepsTheOldOne(t *testing.T) {
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}
	r := &fakeRouting{needs: true, name: "edge-old"}
	s := setup(t, rt, map[string]*fakeRouting{"rte_cloudflare": r})
	require.NoError(t, s.Reconcile(context.Background()))
	require.Contains(t, rt.edges, "edge-old")

	r.name = "edge-new"
	rt.applyErr = errs.New(errs.AdapterFailed, "the image could not be pulled.")
	require.Error(t, s.Reconcile(context.Background()))
	require.Contains(t, rt.edges, "edge-old", "the running edge is kept")
	require.Empty(t, rt.removed)

	// Once the new edge applies, the old one is no longer wanted.
	rt.applyErr = nil
	require.NoError(t, s.Reconcile(context.Background()))
	require.Contains(t, rt.edges, "edge-new")
	require.NotContains(t, rt.edges, "edge-old")
}
