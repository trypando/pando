//go:build integration

package reconciler_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// noAdapters resolves nothing: the apps here were never deployed, so their
// teardown has nothing to destroy.
type noAdapters struct{}

func (noAdapters) Runtime(string) (api.RuntimeAdapter, bool) { return nil, false }
func (noAdapters) Routing(string) (api.RoutingAdapter, bool) { return nil, false }

func awaitingTeardown(t *testing.T, apps *state.Apps, appID string) bool {
	t.Helper()
	targets, err := apps.AwaitingTeardown(context.Background(), 100)
	require.NoError(t, err)
	for _, target := range targets {
		if target.AppID == appID {
			return true
		}
	}
	return false
}

// teardownSignal reports each app whose bundle the GC records destroying.
type teardownSignal struct{ apps chan string }

func (s teardownSignal) Write(_ context.Context, e reconciler.AuditEvent) error {
	if e.Action == "app.bundle.destroy" {
		s.apps <- e.AppID
	}
	return nil
}

// TestR256_AnAppDeletedOnAnotherReplicaIsTornDownByTheLeader asserts that
// the GC, which runs on whichever replica leads, finds a delete made on
// another replica — whose TeardownNow signal never reaches it — within its
// teardown poll rather than at its next hourly pass (R-256, issue #72).
func TestR256_AnAppDeletedOnAnotherReplicaIsTornDownByTheLeader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)

	// An app deleted before the GC starts is torn down by its startup pass,
	// which lists what awaits teardown before the loop begins. Seeing it torn
	// down is how this test knows that listing is behind it.
	before, err := apps.Create(ctx, "before-"+id.New(id.App), id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	require.NoError(t, apps.Archive(ctx, before.ID))
	torn := teardownSignal{apps: make(chan string, 4)}

	gc := &reconciler.GC{
		Apps: apps, Registry: noAdapters{}, Logger: zap.NewNop(), Auditor: torn,
		Interval: time.Hour, TeardownPoll: 20 * time.Millisecond,
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { gc.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case got := <-torn.apps:
		require.Equal(t, before.ID, got)
	case <-time.After(10 * time.Second):
		t.Fatal("the GC's startup pass did not tear down an app deleted before it")
	}

	// Deleted after the GC's startup pass, with no signal to it.
	app, err := apps.Create(ctx, "elsewhere-"+id.New(id.App), id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	require.NoError(t, apps.Archive(ctx, app.ID))

	require.Eventually(t, func() bool { return !awaitingTeardown(t, apps, app.ID) }, 10*time.Second, 20*time.Millisecond,
		"the leader's GC finds the delete by polling")
}

// TestR256_AGCLeftAtItsDefaultPollStillAnswersADeleteAtOnce asserts that a GC
// with no TeardownPoll set — the slow default, ten seconds — still tears a
// delete down as soon as the replica that took it signals, and stops with its
// context.
func TestR256_AGCLeftAtItsDefaultPollStillAnswersADeleteAtOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)

	now := make(chan struct{}, 1)
	torn := teardownSignal{apps: make(chan string, 4)}
	gc := &reconciler.GC{
		Apps: apps, Registry: noAdapters{}, Logger: zap.NewNop(), Auditor: torn,
		Interval: time.Hour, TeardownNow: now,
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { gc.Run(runCtx); close(done) }()

	app, err := apps.Create(ctx, "signaled-"+id.New(id.App), id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	require.NoError(t, apps.Archive(ctx, app.ID))
	now <- struct{}{}

	select {
	case got := <-torn.apps:
		require.Equal(t, app.ID, got)
	case <-time.After(5 * time.Second):
		t.Fatal("a signaled delete waited for the poll")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end with its context")
	}
}

// TestR256_TheReplicaThatTookADeleteTearsItDownItself asserts that the
// replica a delete reached can tear the app down at once, whether or not it
// runs the GC loop (R-256, issue #72).
func TestR256_TheReplicaThatTookADeleteTearsItDownItself(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)

	app, err := apps.Create(ctx, "here-"+id.New(id.App), id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	require.NoError(t, apps.Archive(ctx, app.ID))
	require.True(t, awaitingTeardown(t, apps, app.ID))

	gc := &reconciler.GC{Apps: apps, Registry: noAdapters{}, Logger: zap.NewNop()}
	gc.TearDownDeleted(ctx)
	require.False(t, awaitingTeardown(t, apps, app.ID))
}
