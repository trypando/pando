//go:build integration

package state_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// A detection that fails in the background records why, unless it already
// recorded an outcome of its own, which is the one that stands.
func TestADetectionFailureIsRecordedOnlyWhileItIsStillRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedUser(t, db, "fail-if-running")
	apps := state.NewApps(db)
	detections := state.NewDetections(db)

	src := spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"}
	structured, err := apps.Create(ctx, "structured", id.New(id.App), owner.ID, owner.ID, src)
	require.NoError(t, err)
	plain, err := apps.Create(ctx, "plain", id.New(id.App), owner.ID, owner.ID, src)
	require.NoError(t, err)
	finished, err := apps.Create(ctx, "finished", id.New(id.App), owner.ID, owner.ID, src)
	require.NoError(t, err)

	for _, app := range []state.App{structured, plain, finished} {
		require.NoError(t, detections.Start(ctx, app.ID))
	}
	require.NoError(t, detections.Save(ctx, finished.ID, state.DetectionReady, map[string]any{"ok": true}, "abc123"))

	require.NoError(t, detections.FailIfRunning(ctx, structured.ID,
		errs.New(errs.AdapterUnavailable, "The builder could not be reached.")))
	require.NoError(t, detections.FailIfRunning(ctx, plain.ID, errors.New("the clone was cut off")))
	require.NoError(t, detections.FailIfRunning(ctx, finished.ID, errors.New("too late")))

	got, err := detections.Get(ctx, structured.ID)
	require.NoError(t, err)
	require.Equal(t, state.DetectionFailed, got.Status)
	require.Contains(t, string(got.Body), string(errs.AdapterUnavailable), "a structured error keeps its code")
	require.Contains(t, string(got.Body), "The builder could not be reached.")

	got, err = detections.Get(ctx, plain.ID)
	require.NoError(t, err)
	require.Equal(t, state.DetectionFailed, got.Status)
	require.Contains(t, string(got.Body), "the clone was cut off")

	got, err = detections.Get(ctx, finished.ID)
	require.NoError(t, err)
	require.Equal(t, state.DetectionReady, got.Status, "an outcome already recorded is not overwritten")
	require.NotContains(t, string(got.Body), "too late")
}

// Known is how startup tells this install's app networks from another
// install's on the same Docker host, so a deleted app is still known; Live is
// asked just before acting on an app read a while ago, so it is not (issue
// #55).
func TestADeletedAppIsKnownButNotLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedUser(t, db, "known-live")
	apps := state.NewApps(db)

	app, err := apps.Create(ctx, "known", id.New(id.App), owner.ID, owner.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)

	known, err := apps.Known(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, known)
	live, err := apps.Live(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, live)

	require.NoError(t, apps.Archive(ctx, app.ID))

	known, err = apps.Known(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, known, "a deleted app's network is still this install's")
	live, err = apps.Live(ctx, app.ID)
	require.NoError(t, err)
	require.False(t, live)

	known, err = apps.Known(ctx, id.New(id.App))
	require.NoError(t, err)
	require.False(t, known, "another install's app")
}

// TestR224_ATeardownNamesTheBuilderHoldingTheAppsCache asserts R-224. A
// deleted app's build cache stayed on disk because the teardown did not know
// which builder held it (issue #55).
func TestR224_ATeardownNamesTheBuilderHoldingTheAppsCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedUser(t, db, "teardown-builder")
	apps := state.NewApps(db)

	src := spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"}
	app, err := apps.Create(ctx, "torn-down", id.New(id.App), owner.ID, owner.ID, src)
	require.NoError(t, err)
	rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion, AppID: app.ID, Source: src,
		Build:   spec.Build{Strategy: spec.BuildDockerfile, AdapterRef: "bld_test"},
		Runtime: spec.RuntimeRef{AdapterRef: "rt_test"},
		Routing: spec.Routing{AdapterRef: "rtg_test"},
	}, spec.OriginManual, owner.ID)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, "running", owner.ID))

	never, err := apps.Create(ctx, "never-deployed", id.New(id.App), owner.ID, owner.ID, src)
	require.NoError(t, err)

	// R-204: this delete settled the app's storage; the other one had none.
	require.NoError(t, apps.DiscardStorage(ctx, app.ID))
	require.NoError(t, apps.Archive(ctx, app.ID))
	require.NoError(t, apps.Archive(ctx, never.ID))

	targets, err := apps.AwaitingTeardown(ctx, 100)
	require.NoError(t, err)
	byApp := map[string]state.TeardownTarget{}
	for _, target := range targets {
		byApp[target.AppID] = target
	}
	require.Equal(t, state.TeardownTarget{AppID: app.ID, RuntimeRef: "rt_test", RoutingRef: "rtg_test", BuilderRef: "bld_test",
		DiscardStorage: true}, byApp[app.ID])
	require.Equal(t, state.TeardownTarget{AppID: never.ID}, byApp[never.ID], "an app never deployed has nothing to name")

	require.NoError(t, apps.MarkBundleDestroyed(ctx, app.ID))
	targets, err = apps.AwaitingTeardown(ctx, 100)
	require.NoError(t, err)
	for _, target := range targets {
		require.NotEqual(t, app.ID, target.AppID, "torn down once")
	}
}

// A store that cannot be reached is reported as such, with a sentence, rather
// than taken for an answer.
func TestStartupRecoveryReportsAStoreItCannotReach(t *testing.T) {
	t.Parallel()
	db := connected(t)
	apps := state.NewApps(db)
	detections := state.NewDetections(db)
	deployments := state.NewDeployments(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	internal := func(err error) {
		t.Helper()
		require.Error(t, err)
		require.Equal(t, errs.Internal, errs.As(err).Code)
	}

	_, err := detections.RecoverRunning(ctx)
	internal(err)
	internal(detections.FailIfRunning(ctx, id.New(id.App), errors.New("x")))
	_, err = deployments.RecoverInFlight(ctx)
	internal(err)
	_, err = apps.Known(ctx, id.New(id.App))
	internal(err)
	_, err = apps.Live(ctx, id.New(id.App))
	internal(err)
	_, err = apps.AwaitingTeardown(ctx, 10)
	internal(err)
}
