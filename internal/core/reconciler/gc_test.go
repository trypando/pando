package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// In-package, because teardown is the one part of the janitor that destroys
// things and the order it does them in is the whole of it.

// fakeRegistry resolves only what a test puts in it.
type fakeRegistry struct {
	runtime map[string]api.RuntimeAdapter
	routing map[string]api.RoutingAdapter
}

func (f fakeRegistry) Runtime(ref string) (api.RuntimeAdapter, bool) {
	rt, ok := f.runtime[ref]
	return rt, ok
}

func (f fakeRegistry) Routing(ref string) (api.RoutingAdapter, bool) {
	rte, ok := f.routing[ref]
	return rte, ok
}

// recordingRuntime notes what it was asked to destroy.
type recordingRuntime struct {
	api.RuntimeAdapter
	destroyed []api.BundleRef
	opts      []api.DestroyOptions
	err       error
}

func (r *recordingRuntime) Destroy(_ context.Context, ref api.BundleRef, opts api.DestroyOptions) error {
	r.destroyed = append(r.destroyed, ref)
	r.opts = append(r.opts, opts)
	return r.err
}

// recordingRouting notes what routes it was asked to remove, and in what order
// relative to the runtime.
type recordingRouting struct {
	api.RoutingAdapter
	removed []string
	err     error
	order   *[]string
}

func (r *recordingRouting) Remove(_ context.Context, h api.RouteHandle) error {
	r.removed = append(r.removed, h.AppID)
	if r.order != nil {
		*r.order = append(*r.order, "route")
	}
	return r.err
}

// R-204: a volume outlives the app, so teardown keeps them. Destroying the
// bundle and its storage together is the failure that looks exactly like
// success — the app is gone and so is the data somebody expected to restore.
func TestR204_TearingDownAnAppKeepsItsVolumes(t *testing.T) {
	runtime := &recordingRuntime{}
	g := &GC{
		Logger:   zap.NewNop(),
		Registry: fakeRegistry{runtime: map[string]api.RuntimeAdapter{"rt_docker": runtime}},
	}

	require.NoError(t, g.tearDown(context.Background(), state.TeardownTarget{
		AppID: "app_01HQ8", RuntimeRef: "rt_docker",
	}))

	require.Len(t, runtime.destroyed, 1)
	require.Equal(t, "app_01HQ8", runtime.destroyed[0].BundleID)
	require.True(t, runtime.opts[0].KeepVolumes, "the data survives the app")
}

// TestR204_ADeleteThatSettledTheStorageTakesTheVolumes asserts R-204: a delete
// says to discard the storage or back it up first, and either way it was left
// on disk with no row to reach it by (issue #55). Only that recorded decision
// lets the teardown take the volumes.
func TestR204_ADeleteThatSettledTheStorageTakesTheVolumes(t *testing.T) {
	runtime := &recordingRuntime{}
	g := &GC{
		Logger:   zap.NewNop(),
		Registry: fakeRegistry{runtime: map[string]api.RuntimeAdapter{"rt_docker": runtime}},
	}

	require.NoError(t, g.tearDown(context.Background(), state.TeardownTarget{
		AppID: "app_01HQ8", RuntimeRef: "rt_docker", DiscardStorage: true,
	}))
	require.Len(t, runtime.opts, 1)
	require.False(t, runtime.opts[0].KeepVolumes, "the delete said to discard it")
}

// The route first. A route outliving its app points at a Pando that will answer
// 404 for it, and on a file-provider edge it is a file that accumulates one per
// deleted app.
func TestTheRouteIsRemovedBeforeTheContainers(t *testing.T) {
	var order []string
	routing := &recordingRouting{order: &order}
	runtime := &recordingRuntime{}

	g := &GC{
		Logger: zap.NewNop(),
		Registry: fakeRegistry{
			runtime: map[string]api.RuntimeAdapter{"rt_docker": runtime},
			routing: map[string]api.RoutingAdapter{"rte_traefik": routing},
		},
	}

	require.NoError(t, g.tearDown(context.Background(), state.TeardownTarget{
		AppID: "app_01HQ8", RuntimeRef: "rt_docker", RoutingRef: "rte_traefik",
	}))

	require.Equal(t, []string{"app_01HQ8"}, routing.removed)
	require.Len(t, runtime.destroyed, 1)
	require.Equal(t, []string{"route"}, order, "the route went first")
}

// Never deployed, so there is nothing to destroy and the teardown is complete
// by definition.
func TestAnAppThatWasNeverDeployedTearsDownWithNothingToDo(t *testing.T) {
	g := &GC{Logger: zap.NewNop(), Registry: fakeRegistry{}}

	require.NoError(t, g.tearDown(context.Background(), state.TeardownTarget{AppID: "app_01HQ8"}))
}

// recordingCaches notes which apps' build caches it was asked to forget.
type recordingCaches struct {
	forgot []string
	err    error
}

func (r *recordingCaches) Forget(_ context.Context, builderRef, appID string) error {
	r.forgot = append(r.forgot, builderRef+":"+appID)
	return r.err
}

// TestR224_TearingDownAnAppRemovesItsBuildCacheAndUpload asserts R-224. Both
// stayed on disk after the app was deleted (issue #55): hundreds of megabytes
// of cache for an ordinary app, and every archive anyone uploaded.
func TestR224_TearingDownAnAppRemovesItsBuildCacheAndUpload(t *testing.T) {
	runtime := &recordingRuntime{}
	caches := &recordingCaches{}
	var discarded []string
	g := &GC{
		Logger:        zap.NewNop(),
		Registry:      fakeRegistry{runtime: map[string]api.RuntimeAdapter{"rt_docker": runtime}},
		BuildCaches:   caches,
		DiscardUpload: func(appID string) error { discarded = append(discarded, appID); return nil },
	}

	require.NoError(t, g.tearDown(context.Background(), state.TeardownTarget{
		AppID: "app_01HQ8", RuntimeRef: "rt_docker", BuilderRef: "bld_buildkit",
	}))
	require.Len(t, runtime.destroyed, 1)
	require.Equal(t, []string{"bld_buildkit:app_01HQ8"}, caches.forgot)
	require.Equal(t, []string{"app_01HQ8"}, discarded)

	// An upload is stored before the first deploy, so an app that never ran
	// still has one.
	require.NoError(t, g.tearDown(context.Background(), state.TeardownTarget{AppID: "app_01NEVER"}))
	require.Equal(t, []string{"app_01HQ8", "app_01NEVER"}, discarded)
	require.Len(t, caches.forgot, 1, "no builder, no cache")
}

// A cache that cannot be removed leaves the teardown for the next pass rather
// than recording it done and forgetting the files for good.
func TestABuildCacheThatCannotBeRemovedIsRetried(t *testing.T) {
	g := &GC{
		Logger:        zap.NewNop(),
		Registry:      fakeRegistry{},
		BuildCaches:   &recordingCaches{err: errors.New("read-only file system")},
		DiscardUpload: func(string) error { return nil },
	}
	require.Error(t, g.tearDown(context.Background(), state.TeardownTarget{
		AppID: "app_01HQ8", BuilderRef: "bld_buildkit",
	}))

	g.BuildCaches = &recordingCaches{}
	g.DiscardUpload = func(string) error { return errors.New("permission denied") }
	require.Error(t, g.tearDown(context.Background(), state.TeardownTarget{AppID: "app_01HQ8"}))
}

// The adapter that held it is no longer configured. Reported rather than marked
// done: something is still running and Pando can no longer reach it, which an
// operator needs to know.
func TestAnAppHeldByAnAdapterThatIsGoneIsReportedRatherThanForgotten(t *testing.T) {
	g := &GC{Logger: zap.NewNop(), Registry: fakeRegistry{}}

	err := g.tearDown(context.Background(), state.TeardownTarget{
		AppID: "app_01HQ8", RuntimeRef: "rt_removed",
	})
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "rt_removed",
		"an operator needs to know which runtime still holds it")
}

// A routing adapter that is no longer configured is not a reason to leave the
// containers running: the route is gone with the adapter either way.
func TestARoutingAdapterThatIsGoneDoesNotStopTheTeardown(t *testing.T) {
	runtime := &recordingRuntime{}
	g := &GC{
		Logger:   zap.NewNop(),
		Registry: fakeRegistry{runtime: map[string]api.RuntimeAdapter{"rt_docker": runtime}},
	}

	require.NoError(t, g.tearDown(context.Background(), state.TeardownTarget{
		AppID: "app_01HQ8", RuntimeRef: "rt_docker", RoutingRef: "rte_removed",
	}))
	require.Len(t, runtime.destroyed, 1)
}

// A route that cannot be removed stops the teardown, rather than destroying the
// containers and leaving traffic pointed at nothing.
func TestARouteThatCannotBeRemovedStopsTheTeardown(t *testing.T) {
	runtime := &recordingRuntime{}
	routing := &recordingRouting{err: errors.New("the edge is unreachable")}

	g := &GC{
		Logger: zap.NewNop(),
		Registry: fakeRegistry{
			runtime: map[string]api.RuntimeAdapter{"rt_docker": runtime},
			routing: map[string]api.RoutingAdapter{"rte_traefik": routing},
		},
	}

	require.Error(t, g.tearDown(context.Background(), state.TeardownTarget{
		AppID: "app_01HQ8", RuntimeRef: "rt_docker", RoutingRef: "rte_traefik",
	}))
	require.Empty(t, runtime.destroyed, "nothing was destroyed behind a route that still points at it")
}

func TestARuntimeThatCannotDestroyIsReported(t *testing.T) {
	runtime := &recordingRuntime{err: errors.New("the daemon is not running")}
	g := &GC{
		Logger:   zap.NewNop(),
		Registry: fakeRegistry{runtime: map[string]api.RuntimeAdapter{"rt_docker": runtime}},
	}

	require.Error(t, g.tearDown(context.Background(), state.TeardownTarget{
		AppID: "app_01HQ8", RuntimeRef: "rt_docker",
	}))
}

// Retention is testable without waiting a day, which is why the clock is a
// field rather than a call to time.Now.
func TestTheJanitorsClockIsInjectableAndDefaultsToTheWallClock(t *testing.T) {
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	require.Equal(t, at, (&GC{Clock: clock.NewFake(at)}).now())
	require.WithinDuration(t, time.Now().UTC(), (&GC{}).now(), time.Second)
}

// R-141's two triggers, and the default when a spec names neither. An unknown
// trigger records the common one rather than an empty string, because the
// deployment row is what someone reads to find out why their app redeployed.
func TestAnAutoDeployIsRecordedWithTheTriggerThatCausedIt(t *testing.T) {
	triggerFor := func(t spec.AutoDeployTrigger) string { return string(TriggerOf(spec.AutoDeploy{Trigger: t})) }
	require.Equal(t, "release_tagged", triggerFor(spec.TriggerReleaseTagged))
	require.Equal(t, "branch_updated", triggerFor(spec.TriggerBranchUpdated))
	require.Equal(t, "branch_updated", triggerFor(""))
	require.Equal(t, "branch_updated", triggerFor(spec.AutoDeployTrigger("something-else")))
}
