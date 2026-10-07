//go:build integration

package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// TestR256_QueuedDeploysRunOnceOnWhicheverReplicaHasRoom asserts the deploy
// queue end to end against Postgres (O-32): deploys queued through one
// replica are run by two, each exactly once, and each is recorded against the
// replica that ran it.
//
// The deploys fail at their first step — a revision with no pinned commit —
// which is the quickest way through Run that still reaches the store.
func TestR256_QueuedDeploysRunOnceOnWhicheverReplicaHasRoom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, ownerURL := statetest.Connect(t)
	_, password := statetest.Database(t)
	b, err := state.ConnectCopy(ctx, ownerURL, password)
	require.NoError(t, err)
	t.Cleanup(b.Close)
	for _, db := range []*state.DB{a, b} {
		require.NoError(t, state.NewReplicas(db).Register(ctx, state.Replica{
			ID: db.Replica(), Hostname: "test", AssertionKID: "kid", AssertionKey: make([]byte, 32),
		}))
	}

	first, err := bootstrap.Run(ctx, state.NewUsers(a), state.NewGrants(a), a, audit.New(a.Pool),
		secret.New("a-first-password-123"))
	require.NoError(t, err)
	owner := first.User.ID
	apps := state.NewApps(a)

	var queued []string
	for i := 0; i < 12; i++ {
		src := spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"}
		app, err := apps.Create(ctx, "queued-"+id.New(id.App)[4:12], id.New(id.App), owner, owner, src)
		require.NoError(t, err)
		rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion, AppID: app.ID, Source: src,
			Build: spec.Build{Strategy: spec.BuildDockerfile},
		}, spec.OriginManual, owner)
		require.NoError(t, err)
		dep, err := state.NewDeployments(a).Create(ctx, app.ID, rev.ID, state.TriggerManual, owner)
		require.NoError(t, err)
		queued = append(queued, dep.ID)
	}

	serve := func(db *state.DB) {
		deploys := state.NewDeployments(db)
		runner := NewRunner(api.NewRegistry(), nil, state.NewApps(db), deploys, nil, nil, NewLogStore(),
			state.NewVolumes(db), "http://pando:8080")
		q := &Queue{Runner: runner, Deployments: deploys, Revisions: state.NewApps(db), Limit: 2,
			Poll: 20 * time.Millisecond}
		qctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		go q.Serve(qctx)
	}
	serve(a)
	serve(b)

	deployments := state.NewDeployments(a)
	require.Eventually(t, func() bool {
		for _, depID := range queued {
			dep, _, err := deployments.ByID(ctx, depID)
			if err != nil || dep.Status != state.DeployFailed {
				return false
			}
		}
		return true
	}, 30*time.Second, 50*time.Millisecond, "every queued deploy ran")

	for _, depID := range queued {
		dep, _, err := deployments.ByID(ctx, depID)
		require.NoError(t, err)
		require.Contains(t, dep.ErrorDetail, "does not say which commit to build", "it ran, and failed where it should")
		runner, err := deployments.Runner(ctx, depID)
		require.NoError(t, err)
		require.Contains(t, []string{a.Replica(), b.Replica()}, runner)

		var attempts int
		require.NoError(t, a.QueryRow(ctx, `SELECT attempts FROM deployments WHERE id = $1`, depID).Scan(&attempts))
		require.Equal(t, 1, attempts, "claimed once")
	}
}
