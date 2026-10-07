//go:build integration

package deploy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// revisionsFunc is a Revisions whose answer the test decides.
type revisionsFunc func(ctx context.Context, specID string) (state.Revision, bool, error)

func (f revisionsFunc) RevisionByID(ctx context.Context, specID string) (state.Revision, bool, error) {
	return f(ctx, specID)
}

// queuedDeploy is a database with a registered replica and one deploy per
// name queued on it, keyed by name.
func queuedDeploys(t *testing.T, names ...string) (*state.DB, map[string]state.Deployment) {
	t.Helper()
	ctx := context.Background()
	db, _ := statetest.Connect(t)
	require.NoError(t, state.NewReplicas(db).Register(ctx, state.Replica{
		ID: db.Replica(), Hostname: "test", AssertionKID: "kid", AssertionKey: make([]byte, 32),
	}))
	first, err := bootstrap.Run(ctx, state.NewUsers(db), state.NewGrants(db), db, audit.New(db.Pool),
		secret.New("a-first-password-123"))
	require.NoError(t, err)
	owner := first.User.ID
	apps := state.NewApps(db)
	out := map[string]state.Deployment{}
	for _, name := range names {
		src := spec.Source{Type: spec.SourceGit, URL: "https://example.test/" + name, Ref: "main"}
		app, err := apps.Create(ctx, name, id.New(id.App), owner, owner, src)
		require.NoError(t, err)
		rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion, AppID: app.ID, Source: src,
			Build: spec.Build{Strategy: spec.BuildDockerfile},
		}, spec.OriginManual, owner)
		require.NoError(t, err)
		dep, err := state.NewDeployments(db).Create(ctx, app.ID, rev.ID, state.TriggerManual, owner)
		require.NoError(t, err)
		out[name] = dep
	}
	return db, out
}

// TestR105_AQueuedDeployWhoseRevisionCannotBeReadFailsSayingWhy asserts that a
// deploy the queue cannot start — its revision gone, or unreadable — is
// recorded as failed with a message a person can act on, rather than left
// pending for good. An internal error's own text is not shown; anything else
// is.
func TestR105_AQueuedDeployWhoseRevisionCannotBeReadFailsSayingWhy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, deps := queuedDeploys(t, "gone-rev", "broken-rev", "refused-rev")
	bySpec := map[string]string{}
	for name, dep := range deps {
		bySpec[dep.SpecID] = name
	}

	deployments := state.NewDeployments(db)
	q := &Queue{
		Deployments: deployments,
		Revisions: revisionsFunc(func(_ context.Context, specID string) (state.Revision, bool, error) {
			switch bySpec[specID] {
			case "gone-rev":
				return state.Revision{}, false, nil
			case "broken-rev":
				return state.Revision{}, false, errors.New("pq: relation does not exist")
			default:
				return state.Revision{}, false, errs.New(errs.StateInvalid, "This revision was made for a different app.")
			}
		}),
		Limit: 3,
		Poll:  20 * time.Millisecond,
	}
	qctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go q.Serve(qctx)

	read := func(name string) state.Deployment {
		dep, _, err := deployments.ByID(ctx, deps[name].ID)
		require.NoError(t, err)
		return dep
	}
	require.Eventually(t, func() bool {
		for name := range deps {
			if read(name).Status != state.DeployFailed {
				return false
			}
		}
		return true
	}, 30*time.Second, 20*time.Millisecond, "every deploy that could not start is recorded as failed")

	gone := read("gone-rev")
	require.Equal(t, string(errs.NotFound), gone.ErrorCode)
	require.Equal(t, "The spec revision this deploy was for no longer exists.", gone.ErrorDetail)

	broken := read("broken-rev")
	require.Equal(t, string(errs.Internal), broken.ErrorCode)
	require.Equal(t, "Pando could not read the spec revision this deploy was for.", broken.ErrorDetail,
		"a driver's message is not a person's")

	refused := read("refused-rev")
	require.Equal(t, string(errs.StateInvalid), refused.ErrorCode)
	require.Equal(t, "This revision was made for a different app.", refused.ErrorDetail)
}

// TestR256_AStoppingQueueHandsItsDeployBackUnfailed asserts that a deploy
// running when its replica stops goes back in the queue, not counted as an
// attempt and not recorded as failed, so another replica takes it at once —
// and that Start, the approval service's hook, runs a queued deploy without
// waiting for a poll.
func TestR256_AStoppingQueueHandsItsDeployBackUnfailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, deps := queuedDeploys(t, "stopping")
	dep := deps["stopping"]
	deployments := state.NewDeployments(db)

	started := make(chan struct{}, 1)
	q := &Queue{
		Deployments: deployments,
		Revisions: revisionsFunc(func(ctx context.Context, _ string) (state.Revision, bool, error) {
			started <- struct{}{}
			<-ctx.Done()
			return state.Revision{}, false, ctx.Err()
		}),
		Poll: time.Hour, // only Start can make it look
	}
	require.GreaterOrEqual(t, DefaultConcurrency(), 2, "a queue with no limit set runs at least two")

	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan struct{})
	go func() {
		q.Serve(qctx)
		close(served)
	}()
	// The first claim happens as Serve starts; Start must still be harmless.
	q.Start(ctx, dep, state.Revision{})
	q.Kick()

	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the queued deploy never started")
	}
	require.Equal(t, 1, q.Running())
	waiting, err := deployments.Waiting(ctx, dep.ID)
	require.NoError(t, err)
	require.False(t, waiting, "a running deploy is claimed")

	cancel()
	select {
	case <-served:
	case <-time.After(30 * time.Second):
		t.Fatal("the queue did not stop")
	}

	waiting, err = deployments.Waiting(ctx, dep.ID)
	require.NoError(t, err)
	require.True(t, waiting, "the stopped deploy is back in the queue for another replica")
	got, _, err := deployments.ByID(ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, state.DeployPending, got.Status)
	require.Empty(t, got.ErrorCode, "stopping is not the deploy's failure")
	var attempts int
	require.NoError(t, db.QueryRow(ctx, `SELECT attempts FROM deployments WHERE id = $1`, dep.ID).Scan(&attempts))
	require.Zero(t, attempts, "a rolling restart is not an attempt")
}
