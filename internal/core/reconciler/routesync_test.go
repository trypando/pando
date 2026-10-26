package reconciler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/spec"
)

type syncRouting struct {
	api.RoutingAdapter
	mu   sync.Mutex
	got  []api.RouteRequest
	fail string
}

func (r *syncRouting) Ensure(_ context.Context, req api.RouteRequest) (api.RouteHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.AppID == r.fail {
		return api.RouteHandle{}, errors.New("disk full")
	}
	r.got = append(r.got, req)
	return api.RouteHandle{AppID: req.AppID}, nil
}

type syncRegistry struct{ routing *syncRouting }

func (syncRegistry) Runtime(string) (api.RuntimeAdapter, bool) { return nil, false }
func (s syncRegistry) Routing(ref string) (api.RoutingAdapter, bool) {
	return s.routing, ref == "rt_traefik"
}

type syncApps struct {
	apps []planner.InventoryApp
	err  error
}

func (s syncApps) LiveApps(context.Context, planner.InventoryFilter) ([]planner.InventoryApp, error) {
	return s.apps, s.err
}

func routed(appID, ref string, mode spec.RoutingMode) planner.InventoryApp {
	return planner.InventoryApp{AppID: appID, Spec: &spec.AppSpec{Routing: spec.Routing{
		AdapterRef: ref, Mode: mode, Hostname: appID + ".example.com",
	}}}
}

// TestR417_EveryControlHostWritesEveryRouteFromTheDatabase asserts R-417's
// edge: on a cluster, each host writes every routed app's route itself, each
// pointing at its own Pando (R-023), and skips what has no route to write.
func TestR417_EveryControlHostWritesEveryRouteFromTheDatabase(t *testing.T) {
	ctx := context.Background()
	routing := &syncRouting{fail: "app_broken"}
	two := func(context.Context) (int, error) { return 2, nil }
	s := &RouteSync{
		Apps: syncApps{apps: []planner.InventoryApp{
			routed("app_a", "rt_traefik", spec.RoutingSubdomain),
			routed("app_port", "rt_traefik", spec.RoutingPort),
			routed("app_none", "", spec.RoutingSubdomain),
			routed("app_other", "rt_gone", spec.RoutingSubdomain),
			routed("app_broken", "rt_traefik", spec.RoutingSubdomain),
			{AppID: "app_nospec"},
		}},
		Registry: syncRegistry{routing}, ProxyUpstream: "http://pando-2:8080", Hosts: two,
		Logger: zap.NewNop(),
	}
	n, err := s.Once(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, routing.got, 1)
	require.Equal(t, "app_a", routing.got[0].AppID)
	require.Equal(t, "http://pando-2:8080", routing.got[0].ProxyUpstream, "a route points at Pando, never a workload")

	one := &RouteSync{Apps: s.Apps, Registry: s.Registry, Hosts: func(context.Context) (int, error) { return 1, nil }}
	n, err = one.Once(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "one host's routes are the reconciler's")

	broken := &RouteSync{Apps: syncApps{err: errors.New("database away")}, Registry: s.Registry}
	_, err = broken.Once(ctx)
	require.Error(t, err)
	unread := &RouteSync{Apps: s.Apps, Registry: s.Registry, Hosts: func(context.Context) (int, error) { return 0, errors.New("x") }}
	_, err = unread.Once(ctx)
	require.Error(t, err)
}

func TestRouteSyncRunsUntilItsContextEnds(t *testing.T) {
	routing := &syncRouting{}
	fake := clock.NewFake(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))
	s := &RouteSync{
		Apps:     syncApps{apps: []planner.InventoryApp{routed("app_a", "rt_traefik", spec.RoutingSubdomain)}},
		Registry: syncRegistry{routing}, Clock: fake, Logger: zap.NewNop(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	require.Eventually(t, func() bool {
		routing.mu.Lock()
		defer routing.mu.Unlock()
		return len(routing.got) == 1
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done

	failing := &RouteSync{Apps: syncApps{err: errors.New("away")}, Registry: s.Registry, Clock: fake, Logger: zap.NewNop()}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	failing.Run(ctx)
}
