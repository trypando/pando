//go:build integration

package reconciler_test

import (
	"context"
	"sync"
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

// perAppRuntime observes each app separately: healthy unless told an app's
// container is gone, and recording which apps were looked at.
type perAppRuntime struct {
	fakeRuntime

	mu       sync.Mutex
	missing  map[string]bool
	observed map[string]int
	applied  map[string]int

	// inFlight counts reconciliations of one app running at once, and
	// overlapped records any app that ever had two.
	inFlight   map[string]int
	overlapped map[string]bool
	hold       time.Duration
}

func newPerAppRuntime() *perAppRuntime {
	return &perAppRuntime{
		missing: map[string]bool{}, observed: map[string]int{}, applied: map[string]int{},
		inFlight: map[string]int{}, overlapped: map[string]bool{},
	}
}

func (f *perAppRuntime) Observe(_ context.Context, ref api.BundleRef) (api.ObservedBundle, error) {
	f.mu.Lock()
	f.observed[ref.BundleID]++
	f.inFlight[ref.BundleID]++
	if f.inFlight[ref.BundleID] > 1 {
		f.overlapped[ref.BundleID] = true
	}
	missing := f.missing[ref.BundleID]
	hold := f.hold
	f.mu.Unlock()

	time.Sleep(hold)

	f.mu.Lock()
	f.inFlight[ref.BundleID]--
	f.mu.Unlock()
	if missing {
		return api.ObservedBundle{Exists: true}, nil
	}
	return healthy(), nil
}

func (f *perAppRuntime) Apply(_ context.Context, plan api.BundlePlan) (api.BundleHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied[plan.BundleID]++
	return api.BundleHandle{}, nil
}

func (f *perAppRuntime) seen() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.observed))
	for k, v := range f.observed {
		out[k] = v
	}
	return out
}

type perAppRegistry struct{ runtime *perAppRuntime }

func (r perAppRegistry) Runtime(string) (api.RuntimeAdapter, bool) { return r.runtime, true }
func (r perAppRegistry) Routing(string) (api.RoutingAdapter, bool) { return nil, false }

// seedRunningApps creates n running apps, each pinned to a spec of its own.
func seedRunningApps(t *testing.T, db *state.DB, n int) []string {
	t.Helper()
	ctx := context.Background()
	apps := state.NewApps(db)
	owner := seedOwner(t, db)

	ids := make([]string, 0, n)
	for range n {
		app, err := apps.Create(ctx, "rec-"+id.New(id.App), id.New(id.App), owner, owner,
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

func leaseReconciler(db *state.DB, runtime *perAppRuntime) *reconciler.Reconciler {
	return &reconciler.Reconciler{
		Apps:       state.NewApps(db),
		Reconciles: state.NewReconciles(db),
		Secrets:    state.NewSecrets(db, nil, ""),
		Volumes:    state.NewVolumes(db),
		Registry:   perAppRegistry{runtime: runtime},
		Auditor:    &recordingAuditor{},
		Logger:     zap.NewNop(),
	}
}

// TestR148_EveryAppIsVisitedPastTheFirstTwoHundred asserts R-148 holds for
// every app on an install, not only the first 200.
//
// The loop read the 200 due apps with the oldest updated_at, and a healthy
// pass writes nothing to the row — so the same 200 were visited on every tick
// and the rest never were.
func TestR148_EveryAppIsVisitedPastTheFirstTwoHundred(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 230)
	runtime := newPerAppRuntime()
	rec := leaseReconciler(db, runtime)

	rec.Tick(context.Background())
	seen := runtime.seen()
	for _, appID := range ids {
		require.Equal(t, 1, seen[appID], "every app is visited once in a pass, and only once")
	}

	// And the next pass visits them all again: a healthy app is not skipped
	// for being healthy.
	rec.Tick(context.Background())
	seen = runtime.seen()
	for _, appID := range ids {
		require.Equal(t, 2, seen[appID])
	}
}

// TestR148_AKilledAppBeyondTheFirstTwoHundredIsRestored is the failure the
// starvation caused: the 201st app's container was killed and nothing ever
// noticed.
func TestR148_AKilledAppBeyondTheFirstTwoHundredIsRestored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	ids := seedRunningApps(t, db, 220)
	runtime := newPerAppRuntime()
	rec := leaseReconciler(db, runtime)

	// Every app visited once and settled, so the order is now the one a
	// long-lived install has.
	rec.Tick(ctx)

	// The app the old query would never reach: updated_at the newest.
	last := ids[len(ids)-1]
	_, err := db.Exec(ctx, `UPDATE apps SET updated_at = now() + interval '1 hour' WHERE id = $1`, last)
	require.NoError(t, err)
	runtime.mu.Lock()
	runtime.missing[last] = true
	runtime.mu.Unlock()

	rec.Tick(ctx)

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	require.Equal(t, 1, runtime.applied[last], "the killed app is restored")
	require.Len(t, runtime.applied, 1, "and nothing else is touched")
}

// TestR256_ReplicasShareTheAppsRatherThanEachVisitingEvery asserts that two
// replicas' passes running at once take disjoint sets of apps, and that no
// app is ever reconciled by two of them at the same time.
func TestR256_ReplicasShareTheAppsRatherThanEachVisitingEvery(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 120)

	// One runtime, as on one Docker host, so an overlap is visible to it.
	runtime := newPerAppRuntime()
	runtime.hold = 5 * time.Millisecond

	a, b := leaseReconciler(db, runtime), leaseReconciler(db, runtime)
	a.MinRevisit, b.MinRevisit = reconciler.DefaultMinRevisit, reconciler.DefaultMinRevisit

	var wg sync.WaitGroup
	for _, r := range []*reconciler.Reconciler{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); r.Tick(context.Background()) }()
	}
	wg.Wait()

	seen := runtime.seen()
	for _, appID := range ids {
		require.Equal(t, 1, seen[appID], "each app is visited by one replica, once")
	}
	require.Empty(t, runtime.overlapped, "no app is reconciled twice at once")

	var held int
	require.NoError(t, db.QueryRow(context.Background(),
		`SELECT count(*) FROM apps WHERE reconcile_lease_holder IS NOT NULL`).Scan(&held))
	require.Zero(t, held, "every lease is released when its pass ends")
}

// TestR148_AnAppHeldByADeadReplicaIsVisitedOnceItsLeaseRunsOut asserts that a
// lease left by a process that died does not take the app out of the loop.
func TestR148_AnAppHeldByADeadReplicaIsVisitedOnceItsLeaseRunsOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	ids := seedRunningApps(t, db, 1)
	runtime := newPerAppRuntime()
	rec := leaseReconciler(db, runtime)

	// Held by a pass on a replica that is gone.
	_, err := db.Exec(ctx, `UPDATE apps SET reconcile_lease_holder = 'rep_gone:1',
		reconcile_lease_until = now() + interval '1 hour' WHERE id = $1`, ids[0])
	require.NoError(t, err)
	rec.Tick(ctx)
	require.Zero(t, runtime.seen()[ids[0]], "a held app is left to its holder")

	_, err = db.Exec(ctx, `UPDATE apps SET reconcile_lease_until = now() - interval '1 second' WHERE id = $1`, ids[0])
	require.NoError(t, err)
	rec.Tick(ctx)
	require.Equal(t, 1, runtime.seen()[ids[0]], "and visited once the lease has run out")
}

// TestR148_ALostLeaseStopsTheWork asserts the lease is as strong as the lock it
// replaced: work on an app whose lease another pass has taken is canceled
// rather than left running alongside the new holder's.
func TestR148_ALostLeaseStopsTheWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	ids := seedRunningApps(t, db, 1)

	stolen := make(chan struct{})
	canceled := make(chan struct{})
	runtime := &blockingRuntime{started: make(chan struct{}), canceled: canceled}
	rec := &reconciler.Reconciler{
		Apps:          state.NewApps(db),
		Reconciles:    state.NewReconciles(db),
		Secrets:       state.NewSecrets(db, nil, ""),
		Volumes:       state.NewVolumes(db),
		Registry:      blockingRegistry{runtime},
		Auditor:       &recordingAuditor{},
		Logger:        zap.NewNop(),
		LeaseDuration: 300 * time.Millisecond,
	}

	go func() {
		<-runtime.started
		// Another pass takes the app over, as it would once a lease ran out.
		_, err := db.Exec(ctx, `UPDATE apps SET reconcile_lease_holder = 'rep_other:1' WHERE id = $1`, ids[0])
		require.NoError(t, err)
		close(stolen)
	}()

	done := make(chan struct{})
	go func() { rec.Tick(ctx); close(done) }()

	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("the work on an app whose lease was lost kept running")
	}
	<-stolen
	<-done

	var holder string
	require.NoError(t, db.QueryRow(ctx, `SELECT coalesce(reconcile_lease_holder, '') FROM apps WHERE id = $1`,
		ids[0]).Scan(&holder))
	require.Equal(t, "rep_other:1", holder, "releasing does not take the app back from its new holder")
}

// blockingRuntime's Observe waits until its context is canceled.
type blockingRuntime struct {
	fakeRuntime
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func (f *blockingRuntime) Observe(ctx context.Context, _ api.BundleRef) (api.ObservedBundle, error) {
	f.once.Do(func() { close(f.started) })
	<-ctx.Done()
	close(f.canceled)
	return api.ObservedBundle{}, ctx.Err()
}

type blockingRegistry struct{ runtime *blockingRuntime }

func (r blockingRegistry) Runtime(string) (api.RuntimeAdapter, bool) { return r.runtime, true }
func (r blockingRegistry) Routing(string) (api.RoutingAdapter, bool) { return nil, false }

// startCountingRuntime's Observe counts the apps whose reconciliation has
// started, says when limit have, and waits until its context is canceled.
type startCountingRuntime struct {
	fakeRuntime
	mu      sync.Mutex
	started map[string]bool
	limit   int
	full    chan struct{}
}

func (f *startCountingRuntime) Observe(ctx context.Context, ref api.BundleRef) (api.ObservedBundle, error) {
	f.mu.Lock()
	f.started[ref.BundleID] = true
	if len(f.started) == f.limit {
		close(f.full)
	}
	f.mu.Unlock()
	<-ctx.Done()
	return api.ObservedBundle{}, ctx.Err()
}

type startCountingRegistry struct{ runtime *startCountingRuntime }

func (r startCountingRegistry) Runtime(string) (api.RuntimeAdapter, bool) { return r.runtime, true }
func (r startCountingRegistry) Routing(string) (api.RoutingAdapter, bool) { return nil, false }

// TestR148_AppsClaimedButNotStartedGoBackToTheFrontOnShutdown asserts that a
// pass stopped while apps wait their turn releases them at once, unvisited,
// rather than leaving them held until their leases run out: they are the
// first the next pass takes.
func TestR148_AppsClaimedButNotStartedGoBackToTheFrontOnShutdown(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 3*reconciler.Concurrency)

	runtime := &startCountingRuntime{started: map[string]bool{}, limit: reconciler.Concurrency, full: make(chan struct{})}
	rec := &reconciler.Reconciler{
		Apps:       state.NewApps(db),
		Reconciles: state.NewReconciles(db),
		Secrets:    state.NewSecrets(db, nil, ""),
		Volumes:    state.NewVolumes(db),
		Registry:   startCountingRegistry{runtime},
		Auditor:    &recordingAuditor{},
		Logger:     zap.NewNop(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rec.Tick(ctx); close(done) }()
	select {
	case <-runtime.full:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass never filled its slots")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass did not end when it was stopped")
	}

	var held, unvisited int
	require.NoError(t, db.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE reconcile_lease_holder IS NOT NULL),
		       count(*) FILTER (WHERE reconcile_lease_until IS NULL)
		FROM apps WHERE id = ANY($1)`, ids).Scan(&held, &unvisited))
	require.Zero(t, held, "nothing is left held by a pass that has ended")
	require.Equal(t, len(ids)-reconciler.Concurrency, unvisited,
		"every app that never started is back at the front of the queue")
	runtime.mu.Lock()
	require.Len(t, runtime.started, reconciler.Concurrency, "and none was started after the stop")
	runtime.mu.Unlock()
}
