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
		Registry: syncRegistry{routing}, ProxyUpstream: "http://pando-2:8080", ControlHosts: two,
		Logger: zap.NewNop(),
	}
	n, err := s.Once(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, routing.got, 1)
	require.Equal(t, "app_a", routing.got[0].AppID)
	require.Equal(t, "http://pando-2:8080", routing.got[0].ProxyUpstream, "a route points at Pando, never a workload")

	one := &RouteSync{Apps: s.Apps, Registry: s.Registry, ControlHosts: func(context.Context) (int, error) { return 1, nil }}
	n, err = one.Once(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "one host's routes are the reconciler's")

	broken := &RouteSync{Apps: syncApps{err: errors.New("database away")}, Registry: s.Registry}
	_, err = broken.Once(ctx)
	require.Error(t, err)
	unread := &RouteSync{Apps: s.Apps, Registry: s.Registry, ControlHosts: func(context.Context) (int, error) { return 0, errors.New("x") }}
	_, err = unread.Once(ctx)
	require.Error(t, err)
}

// TestR169_RouteSyncKeepsTLSOnEveryAppsRoute asserts that the route each
// control host writes every minute asks for TLS as the deploy's did. It used
// to leave TLS out, so once a cluster had two hosts every app's Traefik route
// lost its certificate on the next pass and the leader stopped ordering app
// certificates (R-169, R-174, R-417).
func TestR169_RouteSyncKeepsTLSOnEveryAppsRoute(t *testing.T) {
	routing := &syncRouting{}
	s := &RouteSync{
		Apps: syncApps{apps: []planner.InventoryApp{
			routed("app_a", "rt_traefik", spec.RoutingSubdomain),
			routed("app_b", "rt_traefik", spec.RoutingPath),
		}},
		Registry: syncRegistry{routing}, ProxyUpstream: "http://pando-2:8080",
		ControlHosts: func(context.Context) (int, error) { return 2, nil },
	}
	n, err := s.Once(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n)
	for _, got := range routing.got {
		require.Equal(t, api.TLSRequest{Enabled: true, Hostname: got.AppID + ".example.com"}, got.TLS, got.AppID)
	}
}

func controlHosts(n int) func(context.Context) (int, error) {
	return func(context.Context) (int, error) { return n, nil }
}

// TestR417_RouteSyncWritesNothingWhenRoutesAreCurrent asserts that route
// sync writes a route only when it differs from the one this host last
// wrote: it rewrote every route every minute on every cluster of two hosts,
// and its adapter, a file Traefik reloads or a provider's API, saw every
// write. A route that changes is written, and one whose write failed is
// tried again; a route not written for RouteSyncRefresh is written again, in
// case the edge lost it.
func TestR417_RouteSyncWritesNothingWhenRoutesAreCurrent(t *testing.T) {
	ctx := context.Background()
	routing := &syncRouting{fail: "app_b"}
	fake := clock.NewFake(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))
	apps := &syncApps{apps: []planner.InventoryApp{
		routed("app_a", "rt_traefik", spec.RoutingSubdomain),
		routed("app_b", "rt_traefik", spec.RoutingSubdomain),
	}}
	s := &RouteSync{Apps: apps, Registry: syncRegistry{routing}, ProxyUpstream: "http://pando-2:8080",
		ControlHosts: controlHosts(2), Clock: fake, Logger: zap.NewNop()}

	n, err := s.Once(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = s.Once(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "nothing changed, nothing is written")
	require.Len(t, routing.got, 1)

	routing.fail = ""
	apps.apps[0].Spec.Routing.Hostname = "renamed.example.com"
	n, err = s.Once(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n, "the changed route, and the one that failed")
	require.Equal(t, "renamed.example.com", routing.got[1].Hostname)

	fake.Advance(RouteSyncRefresh)
	n, err = s.Once(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n, "written again once it is old")
}

// TestR417_RouteSyncRunsOnlyWithMoreThanOneControlHost asserts that route
// sync, which exists so each control host's own edge has every route, runs
// only when more than one host holds the control role — not on a cluster of
// one control host and app hosts, whose routes are the reconciler's — and on
// one replica of a host at a time.
func TestR417_RouteSyncRunsOnlyWithMoreThanOneControlHost(t *testing.T) {
	ctx := context.Background()
	routing := &syncRouting{}
	apps := syncApps{apps: []planner.InventoryApp{routed("app_a", "rt_traefik", spec.RoutingSubdomain)}}
	one := &RouteSync{Apps: apps, Registry: syncRegistry{routing}, ControlHosts: controlHosts(1)}
	n, err := one.Once(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, routing.got, "one control host: its routes are the reconciler's")

	other := &RouteSync{Apps: apps, Registry: syncRegistry{routing}, ControlHosts: controlHosts(2),
		Exclusive: func(context.Context, func(context.Context)) (bool, error) { return false, nil }}
	n, err = other.Once(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, routing.got, "another replica of this host is writing them")

	first := &RouteSync{Apps: apps, Registry: syncRegistry{routing}, ControlHosts: controlHosts(2),
		Exclusive: func(ctx context.Context, pass func(context.Context)) (bool, error) { pass(ctx); return true, nil }}
	n, err = first.Once(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	unlocked := &RouteSync{Apps: apps, Registry: syncRegistry{routing}, ControlHosts: controlHosts(2),
		Exclusive: func(context.Context, func(context.Context)) (bool, error) { return false, errors.New("database away") }}
	_, err = unlocked.Once(ctx)
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
