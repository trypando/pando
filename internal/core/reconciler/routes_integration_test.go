//go:build integration

package reconciler_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
)

// recordingRouting keeps every route it was asked to write.
type recordingRouting struct {
	api.RoutingAdapter
	mu  sync.Mutex
	got []api.RouteRequest
}

func (r *recordingRouting) Ensure(_ context.Context, req api.RouteRequest) (api.RouteHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, req)
	return api.RouteHandle{AppID: req.AppID}, nil
}

type routedRegistry struct {
	runtime *fakeRuntime
	routing *recordingRouting
}

func (r routedRegistry) Runtime(string) (api.RuntimeAdapter, bool) { return r.runtime, true }
func (r routedRegistry) Routing(string) (api.RoutingAdapter, bool) { return r.routing, true }

// TestR169_ASingleHostDriftCorrectionKeepsTheAppsTLS asserts that the route
// the reconciler rewrites after correcting drift asks for TLS as the deploy's
// did. It used to leave TLS out, so on one host a killed workload's
// correction rewrote the app's Traefik route without its certificate, and
// the app served HTTPS no longer (R-169, R-174).
func TestR169_ASingleHostDriftCorrectionKeepsTheAppsTLS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, state.StateRunning)

	rev1, _, err := h.apps.RevisionByNumber(ctx, h.appID, 1)
	require.NoError(t, err)
	s := *rev1.Body
	s.Routing = spec.Routing{AdapterRef: "rte_traefik", Mode: spec.RoutingSubdomain, Hostname: "notes.example.com"}
	rev, err := h.apps.CreateRevision(ctx, h.appID, &s, spec.OriginEdited, "")
	require.NoError(t, err)
	require.NoError(t, h.apps.Pin(ctx, h.appID, rev.ID, state.StateRunning, ""))

	routing := &recordingRouting{}
	h.rec.Registry = routedRegistry{runtime: h.runtime, routing: routing}
	h.rec.ProxyUpstream = "http://pando:8080"
	h.runtime.setObserved(api.ObservedBundle{Exists: true})

	h.rec.Tick(ctx)

	routing.mu.Lock()
	defer routing.mu.Unlock()
	require.Len(t, routing.got, 1, "the correction rewrites the route")
	got := routing.got[0]
	require.Equal(t, "http://pando:8080", got.ProxyUpstream, "a route points at Pando, never a workload (R-023)")
	require.Equal(t, api.TLSRequest{Enabled: true, Hostname: "notes.example.com"}, got.TLS)
}
