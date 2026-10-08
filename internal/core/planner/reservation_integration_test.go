//go:build integration

package planner_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/approval"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/secret"
)

// noStart leaves a created deploy pending, as a queue nobody has claimed yet.
type noStart struct{}

func (noStart) Start(context.Context, state.Deployment, state.Revision) {}

// racingAllocations holds the first two answers back, once read, until both
// have been read or a moment has passed: without anything serializing the
// plans, both read what is committed before either deploy exists, which is
// the race. Serialized, the first waits out the moment alone.
type racingAllocations struct {
	planner.Allocations
	calls   atomic.Int32
	arrived sync.WaitGroup
}

func (r *racingAllocations) AllocatedOn(ctx context.Context, ref, exclude string) (planner.Allocation, error) {
	got, err := r.Allocations.AllocatedOn(ctx, ref, exclude)
	if r.calls.Add(1) <= 2 {
		r.arrived.Done()
		both := make(chan struct{})
		go func() { r.arrived.Wait(); close(both) }()
		select {
		case <-both:
		case <-time.After(300 * time.Millisecond):
		}
	}
	return got, err
}

// TestR242_ConcurrentFirstDeploysCannotBothTakeTheLastRoom asserts R-242 for
// deploys in flight (migration 62): a deploy reserves what it asks for from
// the moment it passes the plan-time capacity check, and the check and the
// reservation are taken together per runtime, so two first deploys racing for
// room only one of them fits are not both let through — the other is refused
// at plan time with R-242's message. A deploy that ends without running gives
// its room back, including one a stopped replica left behind, and a redeploy
// of a running app is not counted twice.
func TestR242_ConcurrentFirstDeploysCannotBothTakeTheLastRoom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := statetest.Connect(t)
	users := state.NewUsers(db)
	require.NoError(t, users.EnsureLocalAdapter(ctx))
	digest, err := hash.New(secret.New("correct-password"))
	require.NoError(t, err)
	alice, err := users.Create(ctx, state.LocalAdapterID, "alice", "alice@corp.com", "alice", digest, false)
	require.NoError(t, err)
	require.NoError(t, state.NewReplicas(db).Register(ctx, state.Replica{
		ID: db.Replica(), Hostname: "test", AssertionKID: "kid", AssertionKey: make([]byte, 32),
	}))

	rt := capableRuntime()
	rt.capacity.TotalCPUMillis = 1000
	reg := registry(t, rt, capableRouting(), capableBuilder())
	apps, deployments := state.NewApps(db), state.NewDeployments(db)
	allocations := state.NewAllocations(db)
	racing := &racingAllocations{Allocations: allocations}
	racing.arrived.Add(2)
	p := planner.New(reg, policy.Static(policy.Default()), racing)
	svc := &approval.Service{
		Deployments: deployments, Apps: apps, Policy: state.NewPolicy(db),
		Planner: p, Capacity: allocations, Deployer: noStart{},
	}
	principal := authz.Principal{Kind: authz.KindUser, ID: alice.ID, UserID: alice.ID}

	newApp := func(name string, cpu int) (state.App, state.Revision) {
		t.Helper()
		s := plannableSpec()
		s.Resources = spec.Resources{CPUMillis: cpu, MemoryBytes: 64 << 20}
		s.Routing.Port = 9000 + len(name)*7 + cpu%97
		app, err := apps.Create(ctx, name, name, alice.ID, alice.ID, s.Source)
		require.NoError(t, err)
		s.AppID = app.ID
		rev, err := apps.CreateRevision(ctx, app.ID, s, spec.OriginManual, alice.ID)
		require.NoError(t, err)
		return app, rev
	}
	committed := func() int {
		t.Helper()
		a, err := allocations.AllocatedOn(ctx, "rt_docker", "")
		require.NoError(t, err)
		return a.CPUMillis
	}

	// Two first deploys, 600 each, onto 1000: only one fits.
	first, firstRev := newApp("first", 600)
	second, secondRev := newApp("second", 600)
	var wg sync.WaitGroup
	results := make([]error, 2)
	deps := make([]state.Deployment, 2)
	for i, pair := range []struct {
		app state.App
		rev state.Revision
	}{{first, firstRev}, {second, secondRev}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deps[i], results[i] = svc.Deploy(ctx, principal, pair.app, pair.rev, state.TriggerManual)
		}()
	}
	wg.Wait()

	var refused, passed int
	for i, err := range results {
		if err == nil {
			passed = i
			continue
		}
		refused++
		require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err), "%v", err)
		require.Contains(t, errs.As(err).Message, "There is not enough CPU left on this installation to start this app.")
	}
	require.Equal(t, 1, refused, "exactly one of the two is refused at plan time")
	require.Equal(t, 600, committed(), "the deploy that passed holds its room before it is pinned")

	// The deploy fails before it runs: its room is free again, and the other
	// app now fits.
	require.NoError(t, deployments.Finish(ctx, deps[passed].ID, state.DeployFailed, string(errs.Internal), "the build failed"))
	require.Zero(t, committed(), "a failed deploy holds nothing")
	other := []state.App{first, second}[1-passed]
	otherRev := []state.Revision{firstRev, secondRev}[1-passed]
	dep, err := svc.Deploy(ctx, principal, other, otherRev, state.TriggerManual)
	require.NoError(t, err)
	require.Equal(t, 600, committed())

	// It succeeds and is pinned; redeploying it is the same app, not two.
	require.NoError(t, apps.Pin(ctx, other.ID, otherRev.ID, state.StateRunning, alice.ID))
	require.NoError(t, deployments.Finish(ctx, dep.ID, state.DeploySucceeded, "", ""))
	redeploy, err := svc.Deploy(ctx, principal, other, otherRev, state.TriggerManual)
	require.NoError(t, err, "an app's own reservation is not counted against it")
	require.Equal(t, 600, committed(), "pinned and redeploying, counted once")

	// While that redeploy is in flight another 500 does not fit; 400 does.
	big, bigRev := newApp("big", 500)
	_, err = svc.Deploy(ctx, principal, big, bigRev, state.TriggerManual)
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	small, smallRev := newApp("small", 400)
	smallDep, err := svc.Deploy(ctx, principal, small, smallRev, state.TriggerManual)
	require.NoError(t, err)
	require.Equal(t, 1000, committed())
	require.NoError(t, deployments.Finish(ctx, redeploy.ID, state.DeploySucceeded, "", ""))
	require.Equal(t, 1000, committed(), "the running app keeps its pinned room")

	// A replica claimed the small deploy and stopped for good: once
	// RecoverInFlight gives up on it, its room is free.
	_, err = db.Exec(ctx, `UPDATE deployments SET status = 'building', replica_id = $2, attempts = $3 WHERE id = $1`,
		smallDep.ID, db.Replica(), state.MaxAttempts)
	require.NoError(t, err)
	require.Equal(t, 1000, committed(), "claimed and building, still held")
	require.NoError(t, state.NewReplicas(db).Stop(ctx, db.Replica()))
	n, err := deployments.RecoverInFlight(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.Equal(t, 600, committed(), fmt.Sprintf("a deploy recovered as failed holds nothing (%d)", committed()))
}
