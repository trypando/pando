//go:build integration

package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/id"
)

// leasedApps creates n running apps the loop would reconcile.
func leasedApps(t *testing.T, db *state.DB, n int) []string {
	t.Helper()
	ctx := context.Background()
	users := state.NewUsers(db)
	require.NoError(t, users.EnsureLocalAdapter(ctx))
	owner, err := users.Create(ctx, state.LocalAdapterID, "lease-owner", "lease-owner@example.test", "Lease Owner", "", false)
	require.NoError(t, err)

	apps := state.NewApps(db)
	ids := make([]string, 0, n)
	for range n {
		app, err := apps.Create(ctx, "lease-"+id.New(id.App), id.New(id.App), owner.ID, owner.ID,
			spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
		require.NoError(t, err)
		rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion,
			AppID:         app.ID,
			Source:        spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"},
			Build:         spec.Build{Strategy: spec.BuildPrebuilt},
			Workloads:     []spec.Workload{{Name: "web", Image: "example/app:1", Primary: true, Exposed: true}},
			Routing:       spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
			Runtime:       spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
			Deploy:        spec.Deploy{Strategy: spec.DeployRecreate},
		}, spec.OriginDetected, "")
		require.NoError(t, err)
		require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, ""))
		require.NoError(t, apps.SetDesiredState(ctx, app.ID, "running"))
		ids = append(ids, app.ID)
	}
	return ids
}

// claimAll claims every due app for k, as a pass's first claim does.
func claimAll(t *testing.T, k *leaseKeeper, r *state.Reconciles, lease *state.Lease, d time.Duration) []state.Reconcilable {
	t.Helper()
	ctx := context.Background()
	cutoff, err := r.Cutoff(ctx, 0)
	require.NoError(t, err)
	batch, err := k.claim(func() ([]state.Reconcilable, error) {
		return lease.Claim(ctx, time.Now(), cutoff.Add(time.Second), 100, d)
	})
	require.NoError(t, err)
	return batch
}

func observedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.WarnLevel)
	return zap.New(core), logs
}

// TestR148_AnAppTakenWhileWaitingIsNotStarted asserts the lease covers an
// app claimed but not yet started: if another pass has it by the time its
// turn comes, this pass leaves it alone, and the apps it still holds carry on.
func TestR148_AnAppTakenWhileWaitingIsNotStarted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := statetest.Connect(t)
	ids := leasedApps(t, db, 2)

	reconciles := state.NewReconciles(db)
	lease := reconciles.Lease()
	require.True(t, strings.HasPrefix(lease.Holder(), db.Replica()+":"), "a pass is named for its replica")
	logger, logs := observedLogger()
	k := newLeaseKeeper(lease, 300*time.Millisecond, logger)
	require.Len(t, claimAll(t, k, reconciles, lease, 300*time.Millisecond), 2)

	running, ok := k.start(ctx, ids[0])
	require.True(t, ok)

	// The second app, still waiting its turn, is taken by another pass.
	_, err := db.Exec(ctx, `UPDATE apps SET reconcile_lease_holder = 'rep_other:1' WHERE id = $1`, ids[1])
	require.NoError(t, err)

	keeperCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); k.run(keeperCtx) }()
	defer func() { stop(); <-done }()

	require.Eventually(t, func() bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		return k.lost[ids[1]]
	}, 5*time.Second, 20*time.Millisecond, "an extension finds the waiting app gone")
	_, ok = k.start(ctx, ids[1])
	require.False(t, ok, "and it is never started")

	// Extended past its first lease, the running app is still this pass's.
	time.Sleep(400 * time.Millisecond)
	require.NoError(t, running.Err(), "an app whose lease is kept keeps running")
	// Compared on the database's clock, which is the one leases are kept by.
	var current bool
	require.NoError(t, db.QueryRow(ctx, `SELECT reconcile_lease_until > now() FROM apps WHERE id = $1`, ids[0]).Scan(&current))
	require.True(t, current, "its lease was extended")

	// Then it is taken too, and its work is stopped.
	_, err = db.Exec(ctx, `UPDATE apps SET reconcile_lease_holder = 'rep_other:1' WHERE id = $1`, ids[0])
	require.NoError(t, err)
	select {
	case <-running.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the work on an app whose lease was taken kept running")
	}
	require.NotZero(t, logs.FilterMessage("lost the reconciliation lease on an app; stopping its work").Len())
	k.finish(ids[0])
	k.finish(ids[0]) // finishing twice is harmless
}

// TestR148_ALeaseThatCannotBeExtendedStopsEverything asserts that when the
// database cannot be reached to extend the lease, every app's work stops once
// the last extension has run out: none of them can be assumed to be this
// pass's any more, and another pass may already have them.
func TestR148_ALeaseThatCannotBeExtendedStopsEverything(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := statetest.Connect(t)
	ids := leasedApps(t, db, 2)

	reconciles := state.NewReconciles(db)
	lease := reconciles.Lease()
	logger, logs := observedLogger()
	k := newLeaseKeeper(lease, 300*time.Millisecond, logger)
	require.Len(t, claimAll(t, k, reconciles, lease, 300*time.Millisecond), 2)
	running, ok := k.start(ctx, ids[0])
	require.True(t, ok)

	// The database goes away.
	db.Close()
	_, err := lease.Extend(ctx, time.Second)
	require.Error(t, err)
	require.Error(t, lease.Release(ctx, ids[0], nil))
	_, err = reconciles.Cutoff(ctx, time.Minute)
	require.Error(t, err)

	keeperCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); k.run(keeperCtx) }()
	defer func() { stop(); <-done }()

	select {
	case <-running.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("work went on after the lease could no longer be kept")
	}
	require.NotZero(t, logs.FilterMessage("lost the reconciliation lease; stopping this pass's work").Len())
	_, ok = k.start(ctx, ids[1])
	require.False(t, ok, "an app still waiting is not started under a lease that ran out")
}

func TestALeaseKeeperClaimThatFailsHoldsNothing(t *testing.T) {
	k := newLeaseKeeper(nil, time.Second, zap.NewNop())
	_, err := k.claim(func() ([]state.Reconcilable, error) { return nil, errors.New("database unreachable") })
	require.EqualError(t, err, "database unreachable")
	require.Empty(t, k.waiting)
}
