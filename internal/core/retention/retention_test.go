package retention

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/core/clock"
)

// fakeStore records what each delete was asked for and answers from a script:
// each call to a table takes the next count from its list, and zero once the
// list runs out.
type fakeStore struct {
	mu      sync.Mutex
	counts  map[string][]int64
	fail    map[string]error
	calls   map[string]int
	limits  map[string]int
	befores map[string]time.Time
	keep    int
	onCall  func(table string)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		counts: map[string][]int64{}, fail: map[string]error{}, calls: map[string]int{},
		limits: map[string]int{}, befores: map[string]time.Time{},
	}
}

func (f *fakeStore) next(table string, before time.Time, limit int) (int64, error) {
	f.mu.Lock()
	f.calls[table]++
	f.limits[table] = limit
	f.befores[table] = before
	var n int64
	if q := f.counts[table]; len(q) > 0 {
		n, f.counts[table] = q[0], q[1:]
	}
	err := f.fail[table]
	hook := f.onCall
	f.mu.Unlock()
	if hook != nil {
		hook(table)
	}
	return n, err
}

func (f *fakeStore) Deployments(_ context.Context, keepPerApp, limit int) (int64, error) {
	f.mu.Lock()
	f.keep = keepPerApp
	f.mu.Unlock()
	return f.next("deployments", time.Time{}, limit)
}
func (f *fakeStore) DeletedApps(_ context.Context, before time.Time, limit int) (int64, error) {
	return f.next("deleted_apps", before, limit)
}
func (f *fakeStore) Scans(_ context.Context, before time.Time, limit int) (int64, error) {
	return f.next("scans", before, limit)
}
func (f *fakeStore) Sessions(_ context.Context, before time.Time, limit int) (int64, error) {
	return f.next("sessions", before, limit)
}
func (f *fakeStore) Notifications(_ context.Context, now time.Time, limit int) (int64, error) {
	return f.next("notifications", now, limit)
}
func (f *fakeStore) IdempotencyKeys(_ context.Context, before time.Time, limit int) (int64, error) {
	return f.next("idempotency_keys", before, limit)
}
func (f *fakeStore) SSOFlows(_ context.Context, before, _ time.Time, limit int) (int64, error) {
	return f.next("sso_flows", before, limit)
}

// outbox is the event outbox's prune, as a fakeStore table.
type outbox struct{ *fakeStore }

func (o outbox) Prune(_ context.Context, before time.Time, limit int) (int64, error) {
	return o.next("events", before, limit)
}

// TestR224_RetentionDrainsABacklogABatchAtATimeWithItsDocumentedWindows
// asserts the job's defaults and its batching: each table is deleted from in
// batches until one comes back short, and each window is the documented
// default measured back from now.
func TestR224_RetentionDrainsABacklogABatchAtATimeWithItsDocumentedWindows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.counts["deployments"] = []int64{1000, 1000, 7}
	store.counts["events"] = []int64{1000, 0}
	store.counts["sessions"] = []int64{4}

	core, logs := observer.New(zap.InfoLevel)
	job := &Job{Store: store, Outbox: outbox{store}, Clock: clock.NewFake(now), Logger: zap.New(core)}
	removed := job.Pass(context.Background())

	require.Equal(t, int64(2007), removed["deployments"])
	require.Equal(t, 3, store.calls["deployments"], "batches until one comes back short")
	require.Equal(t, int64(1000), removed["events"])
	require.Equal(t, int64(4), removed["sessions"])
	require.Zero(t, removed["scans"])
	require.Len(t, removed, 8, "every table, the outbox included")

	day := 24 * time.Hour
	require.Equal(t, 50, store.keep)
	require.Equal(t, 1000, store.limits["scans"])
	require.Equal(t, now.Add(-90*day), store.befores["scans"])
	require.Equal(t, now.Add(-30*day), store.befores["sessions"])
	require.Equal(t, now, store.befores["notifications"], "a notification carries its own retain_until")
	require.Equal(t, now.Add(-day), store.befores["idempotency_keys"])
	require.Equal(t, now.Add(-day), store.befores["sso_flows"])
	require.Equal(t, now.Add(-30*day), store.befores["events"])
	require.Equal(t, now.Add(-30*day), store.befores["deleted_apps"])

	require.Equal(t, 3, logs.FilterMessage("removed old rows").Len(), "one line per table that lost rows")
}

// TestR224_ATableThatCannotBeClearedDoesNotStopTheOthers asserts that a
// failing delete is logged with the table it was for, and the pass goes on to
// the next table.
func TestR224_ATableThatCannotBeClearedDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.fail["scans"] = errors.New("database is gone")
	store.counts["scans"] = []int64{1000, 1000}
	store.counts["sso_flows"] = []int64{2}

	core, logs := observer.New(zap.WarnLevel)
	job := &Job{Store: store, Logger: zap.New(core), Settings: Settings{Batch: 1000}}
	removed := job.Pass(context.Background())

	require.Equal(t, int64(1000), removed["scans"], "what went before the failure is counted")
	require.Equal(t, 1, store.calls["scans"], "a failing table is not retried in the same pass")
	require.Equal(t, int64(2), removed["sso_flows"], "the tables after it still ran")
	_, hasEvents := removed["events"]
	require.False(t, hasEvents, "no outbox, no events step")

	warned := logs.FilterMessage("could not remove old rows").All()
	require.Len(t, warned, 1)
	require.Equal(t, "scans", warned[0].ContextMap()["table"])
}

// TestR224_AStoppingPassStopsBetweenBatches asserts that a pass whose
// context ends stops deleting rather than draining every table first.
func TestR224_AStoppingPassStopsBetweenBatches(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	store := newFakeStore()
	store.counts["deployments"] = []int64{5, 5, 5, 5}
	store.onCall = func(string) { cancel() }

	job := &Job{Store: store, Settings: Settings{Batch: 5}}
	removed := job.Pass(ctx)

	require.Equal(t, int64(5), removed["deployments"])
	require.Equal(t, 1, store.calls["deployments"])
	require.Zero(t, store.calls["scans"], "nothing after the table it was on")
}

// TestR224_RetentionRunsAtOnceAndThenOnItsInterval asserts Run's schedule:
// a pass at start, another each interval, and none after its context ends.
func TestR224_RetentionRunsAtOnceAndThenOnItsInterval(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	job := &Job{Store: store, Every: 5 * time.Millisecond}
	go func() {
		job.Run(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.calls["deployments"] >= 3
	}, 10*time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}
