//go:build integration

package reconciler_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/secret"
)

// eventRuntime is a runtime that reports bundle events (O-52): each
// WatchBundles call opens a stream, says Watching, and passes on whatever the
// test sends until the test drops it.
type eventRuntime struct {
	*perAppRuntime

	events chan api.BundleEvent

	mu       sync.Mutex
	connects int
	drop     chan struct{}
	// refuse fails every attempt to open a stream.
	refuse bool
}

func newEventRuntime() *eventRuntime {
	return &eventRuntime{perAppRuntime: newPerAppRuntime(), events: make(chan api.BundleEvent, 16), drop: make(chan struct{})}
}

func (f *eventRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{SupportsPrivateNetwork: true, SupportsBundleEvents: true}, nil
}

func (f *eventRuntime) WatchBundles(ctx context.Context, sink func(api.BundleEvent)) error {
	f.mu.Lock()
	if f.refuse {
		f.mu.Unlock()
		return errors.New("connection refused")
	}
	f.connects++
	drop := f.drop
	f.mu.Unlock()

	sink(api.BundleEvent{Kind: api.BundleEventWatching})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-drop:
			return errors.New("the stream ended")
		case e := <-f.events:
			sink(e)
		}
	}
}

// dropStream ends the open stream; the next one stays open.
func (f *eventRuntime) dropStream() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.drop)
	f.drop = make(chan struct{})
}

func (f *eventRuntime) connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connects
}

func (f *eventRuntime) setMissing(appID string, missing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.perAppRuntime.mu.Lock()
	defer f.perAppRuntime.mu.Unlock()
	f.missing[appID] = missing
}

type eventRegistry struct{ runtime api.RuntimeAdapter }

func (r eventRegistry) Runtime(string) (api.RuntimeAdapter, bool) { return r.runtime, true }
func (r eventRegistry) Routing(string) (api.RoutingAdapter, bool) { return nil, false }

// eventReconciler is a reconciler following runtime's events under a fake
// clock, so waits on it — reconnecting, retrying a nudge — happen only when a
// test advances it.
func eventReconciler(db *state.DB, runtime api.RuntimeAdapter, clk clock.Clock) *reconciler.Reconciler {
	rec := leaseReconciler(db, nil)
	rec.Registry = eventRegistry{runtime: runtime}
	rec.Clock = clk
	rec.Runtimes = func() []string { return []string{"rt_fake"} }
	return rec
}

// leaseUntil is how far from the database's now an app's next visit is:
// positive while it waits for the slow sweep. It waits out a pass that holds
// the app.
func leaseUntil(t *testing.T, db *state.DB, appID string) time.Duration {
	t.Helper()
	var left time.Duration
	require.Eventually(t, func() bool {
		var ok bool
		left, ok = released(db, appID)
		return ok
	}, 10*time.Second, 10*time.Millisecond)
	return left
}

// released is leaseUntil once, and false while a pass holds the app.
func released(db *state.DB, appID string) (time.Duration, bool) {
	var secs float64
	if err := db.QueryRow(context.Background(), `
		SELECT coalesce(extract(epoch FROM reconcile_lease_until - now()), -1)::float8
		FROM apps WHERE id = $1 AND reconcile_lease_holder IS NULL`, appID).Scan(&secs); err != nil {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

// settledAll waits for every app to have been visited and left for the slow
// sweep.
func settledAll(t *testing.T, db *state.DB, ids ...string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, id := range ids {
			if left, ok := released(db, id); !ok || left < time.Minute {
				return false
			}
		}
		return true
	}, 10*time.Second, 20*time.Millisecond, "every settled app waits for the slow sweep")
}

// runLoop runs rec until the test ends, or until the returned func is called.
func runLoop(t *testing.T, rec *reconciler.Reconciler) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rec.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

// TestR148_AnExitedWorkloadIsReconciledWithoutWaitingForTheSweep asserts R-148
// on a runtime that reports events (O-52): an app whose workload exits is
// restored when the runtime says so, not when the slow sweep comes round to
// it, and the apps nothing happened to are left alone.
func TestR148_AnExitedWorkloadIsReconciledWithoutWaitingForTheSweep(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 3)
	runtime := newEventRuntime()
	rec := eventReconciler(db, runtime, clock.NewFake(time.Now()))
	runLoop(t, rec)

	settledAll(t, db, ids...)
	before := runtime.seen()

	runtime.setMissing(ids[0], true)
	runtime.events <- api.BundleEvent{Kind: api.BundleEventExited, BundleID: ids[0], Workload: "web"}

	require.Eventually(t, func() bool {
		runtime.perAppRuntime.mu.Lock()
		defer runtime.perAppRuntime.mu.Unlock()
		return runtime.applied[ids[0]] == 1
	}, 10*time.Second, 20*time.Millisecond, "the exited workload is restored at once")

	after := runtime.seen()
	require.Greater(t, after[ids[0]], before[ids[0]])
	require.Equal(t, before[ids[1]], after[ids[1]], "an app nothing happened to is not looked at")
	require.Equal(t, before[ids[2]], after[ids[2]])

	// Corrected, not settled: it is looked at again on the fast cadence.
	require.Less(t, leaseUntil(t, db, ids[0]), time.Minute)
}

// TestASettledAppWaitsForTheSlowSweep asserts the cadence O-52 chose: a
// settled app on a runtime whose events are followed is next due
// SettledRevisit after it was visited, not on the next pass — and it is due
// once that time has passed.
func TestASettledAppWaitsForTheSlowSweep(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 2)
	runtime := newEventRuntime()
	rec := eventReconciler(db, runtime, clock.NewFake(time.Now()))
	runLoop(t, rec)

	settledAll(t, db, ids...)
	left := leaseUntil(t, db, ids[0])
	require.InDelta(t, reconciler.DefaultSettledRevisit.Seconds(), left.Seconds(), 30,
		"due again after the slow sweep's interval")

	before := runtime.seen()
	rec.Tick(context.Background())
	require.Equal(t, before, runtime.seen(), "a pass before then passes over it")

	// The sweep's interval passes.
	_, err := db.Exec(context.Background(), `
		UPDATE apps SET reconcile_lease_until = reconcile_lease_until - interval '5 minutes 1 second'
		WHERE id = $1`, ids[0])
	require.NoError(t, err)
	rec.Tick(context.Background())
	after := runtime.seen()
	require.Equal(t, before[ids[0]]+1, after[ids[0]], "due once the interval has passed")
	require.Equal(t, before[ids[1]], after[ids[1]])
}

// TestADroppedEventStreamReconnectsAndCatchesUp asserts that a stream that
// drops is opened again after its backoff, and that every app waiting for the
// sweep is then looked at, since whatever happened to it in between was not
// reported (O-52).
func TestADroppedEventStreamReconnectsAndCatchesUp(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 2)
	runtime := newEventRuntime()
	clk := clock.NewFake(time.Now())
	rec := eventReconciler(db, runtime, clk)
	runLoop(t, rec)

	settledAll(t, db, ids...)
	require.Equal(t, 1, runtime.connections())
	before := runtime.seen()

	// The workload goes while nobody is listening.
	runtime.setMissing(ids[1], true)
	runtime.dropStream()

	// Not reopened until the backoff has passed on the reconciler's clock.
	require.Never(t, func() bool { return runtime.connections() > 1 }, 200*time.Millisecond, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		clk.Advance(time.Second)
		return runtime.connections() == 2
	}, 10*time.Second, 20*time.Millisecond, "the stream is opened again")

	require.Eventually(t, func() bool {
		seen := runtime.seen()
		return seen[ids[0]] > before[ids[0]] && seen[ids[1]] > before[ids[1]]
	}, 10*time.Second, 20*time.Millisecond, "every waiting app is looked at again")
	require.Eventually(t, func() bool {
		runtime.perAppRuntime.mu.Lock()
		defer runtime.perAppRuntime.mu.Unlock()
		return runtime.applied[ids[1]] == 1
	}, 10*time.Second, 20*time.Millisecond, "what was missed is restored")
}

// TestAnEventForAnAppAnotherPassHoldsIsNotLost asserts that an event for an
// app another replica is reconciling — which may have looked at it before the
// event — gets the app looked at again once that replica lets go (O-52).
func TestAnEventForAnAppAnotherPassHoldsIsNotLost(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 1)
	runtime := newEventRuntime()
	clk := clock.NewFake(time.Now())
	rec := eventReconciler(db, runtime, clk)
	runLoop(t, rec)
	settledAll(t, db, ids...)
	ctx := context.Background()

	_, err := db.Exec(ctx, `UPDATE apps SET reconcile_lease_holder = 'rep_other:1',
		reconcile_lease_until = now() + interval '1 hour' WHERE id = $1`, ids[0])
	require.NoError(t, err)
	before := runtime.seen()[ids[0]]

	runtime.events <- api.BundleEvent{Kind: api.BundleEventExited, BundleID: ids[0]}
	require.Never(t, func() bool { return runtime.seen()[ids[0]] > before }, 300*time.Millisecond, 20*time.Millisecond,
		"another replica holds it")

	// The other replica finds it settled and lets go, leaving it for the sweep.
	_, err = db.Exec(ctx, `UPDATE apps SET reconcile_lease_holder = NULL,
		reconcile_lease_until = now() + interval '4 minutes' WHERE id = $1`, ids[0])
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		clk.Advance(3 * time.Second)
		return runtime.seen()[ids[0]] > before
	}, 10*time.Second, 20*time.Millisecond, "looked at once the other replica lets go")
}

// TestR151_AnEventForAFailedAppTouchesNothing asserts R-151 holds for events
// (O-52): a failed app's runtime reporting on it does not make the reconciler
// look at it, and its row's place in the queue is not moved.
func TestR151_AnEventForAFailedAppTouchesNothing(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 2)
	ctx := context.Background()
	require.NoError(t, state.NewApps(db).SetState(ctx, ids[0], state.StateFailed))

	runtime := newEventRuntime()
	rec := eventReconciler(db, runtime, clock.NewFake(time.Now()))
	runLoop(t, rec)
	settledAll(t, db, ids[1])

	got, err := state.NewReconciles(db).Nudge(ctx, ids[0])
	require.NoError(t, err)
	require.Equal(t, state.Nudged{}, got, "a nudge is a no-op for a failed app")

	for _, kind := range []api.BundleEventKind{api.BundleEventExited, api.BundleEventStarted, api.BundleEventRemoved} {
		runtime.events <- api.BundleEvent{Kind: kind, BundleID: ids[0]}
	}
	runtime.events <- api.BundleEvent{Kind: api.BundleEventMissed}
	// The marker: an event for the other app, handled after the failed one's.
	runtime.events <- api.BundleEvent{Kind: api.BundleEventExited, BundleID: ids[1]}
	before := runtime.seen()[ids[1]]
	require.Eventually(t, func() bool { return runtime.seen()[ids[1]] > before }, 10*time.Second, 20*time.Millisecond)

	require.Zero(t, runtime.seen()[ids[0]], "a failed app is not even observed")
	var appState string
	require.NoError(t, db.QueryRow(ctx, `SELECT state FROM apps WHERE id = $1`, ids[0]).Scan(&appState))
	require.Equal(t, state.StateFailed, appState)
}

// TestARuntimeWithoutEventsKeepsTheFastCadence asserts that a runtime that
// does not report events has its apps visited on every pass, as before O-52.
func TestARuntimeWithoutEventsKeepsTheFastCadence(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 1)
	runtime := newPerAppRuntime()
	rec := eventReconciler(db, runtime, clock.NewFake(time.Now()))
	runLoop(t, rec)

	require.Eventually(t, func() bool { return runtime.seen()[ids[0]] > 0 }, 10*time.Second, 20*time.Millisecond)
	rec.Tick(context.Background())
	require.Less(t, leaseUntil(t, db, ids[0]), time.Second, "due on the next pass")
	before := runtime.seen()[ids[0]]
	rec.Tick(context.Background())
	require.Equal(t, before+1, runtime.seen()[ids[0]])
}

// TestAStreamThatCannotOpenKeepsTheFastCadence asserts that apps on a runtime
// whose stream is not open are not left for the slow sweep.
func TestAStreamThatCannotOpenKeepsTheFastCadence(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 1)
	runtime := newEventRuntime()
	runtime.refuse = true
	rec := eventReconciler(db, runtime, clock.NewFake(time.Now()))
	runLoop(t, rec)

	require.Eventually(t, func() bool { return runtime.seen()[ids[0]] > 0 }, 10*time.Second, 20*time.Millisecond)
	rec.Tick(context.Background())
	require.Less(t, leaseUntil(t, db, ids[0]), time.Second)
}

// TestR193_ChangesBringASettledAppForward asserts that a settled app waiting
// for the slow sweep is due at once when something that changes what it
// should run is written: a rotated secret (R-193), a desired state, a state.
func TestR193_ChangesBringASettledAppForward(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 3)
	runtime := newEventRuntime()
	rec := eventReconciler(db, runtime, clock.NewFake(time.Now()))
	stop := runLoop(t, rec)
	settledAll(t, db, ids...)
	// Stopped, so no pass takes the apps between the writes and the check.
	stop()
	ctx := context.Background()

	secrets := state.NewSecrets(db, fakeSecretsAdapter{}, "sek_fake")
	require.NoError(t, secrets.Put(ctx, ids[0], "API_KEY", secret.New("rotated")))
	require.NoError(t, state.NewApps(db).SetDesiredState(ctx, ids[1], "stopped"))
	require.NoError(t, state.NewApps(db).SetState(ctx, ids[2], state.StateDegraded))

	for _, id := range ids {
		var due bool
		require.NoError(t, db.QueryRow(ctx,
			`SELECT reconcile_lease_until IS NULL OR reconcile_lease_until <= now() FROM apps WHERE id = $1`, id).Scan(&due))
		require.True(t, due, "due on the next pass")
	}
	require.NoError(t, secrets.Delete(ctx, ids[0], "API_KEY"))
}

// TestASettledReleaseOfAnAppThatChangedMeanwhileIsNotDeferred asserts that an
// app written to while a pass held it is released as due, not left for the
// sweep: the pass may have looked at it before the write.
func TestASettledReleaseOfAnAppThatChangedMeanwhileIsNotDeferred(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ids := seedRunningApps(t, db, 1)
	ctx := context.Background()
	reconciles := state.NewReconciles(db)
	lease := reconciles.Lease()
	cutoff, err := reconciles.Cutoff(ctx, 0)
	require.NoError(t, err)
	claimed, err := lease.Claim(ctx, time.Now(), cutoff, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	_, err = db.Exec(ctx, `UPDATE apps SET updated_at = now() WHERE id = $1`, ids[0])
	require.NoError(t, err)
	require.NoError(t, lease.ReleaseSettled(ctx, ids[0], claimed[0].ClaimedAt, 5*time.Minute))
	require.Less(t, leaseUntil(t, db, ids[0]), time.Second)
}

// fakeSecretsAdapter stores nothing anywhere; Put only needs an answer.
type fakeSecretsAdapter struct{ api.SecretsAdapter }

func (fakeSecretsAdapter) Put(context.Context, api.SecretRef, secret.Value) (api.StoredRef, error) {
	return api.StoredRef{Ciphertext: []byte("x")}, nil
}
