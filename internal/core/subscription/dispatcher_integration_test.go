//go:build integration

package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

func outbox(t *testing.T) (context.Context, *state.DB, string) {
	t.Helper()
	ctx := context.Background()
	db, _ := statetest.Connect(t)
	first, err := bootstrap.Run(ctx, state.NewUsers(db), state.NewGrants(db), db, audit.New(db.Pool),
		secret.New("a-first-password-123"))
	require.NoError(t, err)
	return ctx, db, first.User.ID
}

func subscribe(t *testing.T, ctx context.Context, db *state.DB, owner string) string {
	t.Helper()
	sub, err := state.NewSubscriptions(db).Create(ctx, state.Subscription{
		ID: id.New(id.Subscription), OwnerID: owner, Events: []string{"deploy.failed"},
		Destination: state.DestinationWebhook, URL: "https://example.test/hook", CreatedBy: owner,
	})
	require.NoError(t, err)
	return sub.ID
}

func deliveriesOf(t *testing.T, ctx context.Context, db *state.DB, eventID string) []string {
	t.Helper()
	rows, err := db.Query(ctx, `SELECT subscription_id FROM event_deliveries WHERE event_id = $1 ORDER BY subscription_id`, eventID)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

// TestR367_ACachedSubscriptionListNeverMissesALaterSubscription asserts that
// caching the subscriptions for routing (issue #72) does not cost a
// subscription the events that happen after it is made (R-367): an event
// newer than the cached list waits for the next list rather than being routed
// against one that cannot know about a subscription made in between.
func TestR367_ACachedSubscriptionListNeverMissesALaterSubscription(t *testing.T) {
	t.Parallel()
	ctx, db, owner := outbox(t)
	d := &Dispatcher{
		Events: state.NewEvents(db), Subscriptions: state.NewSubscriptions(db),
		Deliveries: state.NewDeliveries(db), SubscriptionCache: time.Hour,
	}

	first := subscribe(t, ctx, db, owner)
	_, err := d.route(ctx) // reads, and caches, the list
	require.NoError(t, err)

	second := subscribe(t, ctx, db, owner)
	eventID, err := d.Events.Emit(ctx, state.Event{Name: "deploy.failed"})
	require.NoError(t, err)

	routed, err := d.route(ctx)
	require.NoError(t, err)
	require.Empty(t, routed, "newer than the cached list, so not routed against it")
	require.Empty(t, deliveriesOf(t, ctx, db, eventID))

	d.forget() // the list's time is up
	time.Sleep(cacheMargin + 100*time.Millisecond)
	_, err = d.route(ctx)
	require.NoError(t, err)
	want := []string{first, second}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	require.Equal(t, want, deliveriesOf(t, ctx, db, eventID), "both subscriptions hear it")
}

// TestR367_ASubscriptionMadeOnAnotherReplicaIsHeardWithoutWaitingOutTheCache
// asserts the dispatcher's list of subscriptions under migration 61 (issue
// #72): kept for as long as nothing changes — the same list on every pass, not
// read again every few seconds — and dropped on every replica the moment a
// subscription is made on any of them, so the new one hears the next event
// (R-367) without anybody waiting out a timer.
func TestR367_ASubscriptionMadeOnAnotherReplicaIsHeardWithoutWaitingOutTheCache(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ownerURL, password := statetest.Database(t)
	connect := func() *state.DB {
		db, err := state.ConnectCopy(ctx, ownerURL, password)
		require.NoError(t, err)
		t.Cleanup(db.Close)
		return db
	}
	here, there := connect(), connect()
	first, err := bootstrap.Run(ctx, state.NewUsers(here), state.NewGrants(here), here, audit.New(here.Pool),
		secret.New("a-first-password-123"))
	require.NoError(t, err)
	owner := first.User.ID

	d := &Dispatcher{
		Events: state.NewEvents(here), Subscriptions: state.NewSubscriptions(here),
		Deliveries: state.NewDeliveries(here),
	}
	go d.watch(ctx)
	require.Eventually(t, d.watching.Load, 10*time.Second, 20*time.Millisecond, "listening")

	before := subscribe(t, ctx, here, owner)
	require.Eventually(t, func() bool {
		_, err := d.route(ctx)
		require.NoError(t, err)
		d.cacheMu.Lock()
		defer d.cacheMu.Unlock()
		return d.current(d.cache) && len(d.cache.subs) == 1
	}, 10*time.Second, 50*time.Millisecond, "the list read after the first subscription was told")
	d.cacheMu.Lock()
	kept := d.cache
	d.cacheMu.Unlock()
	require.True(t, kept.watched)
	require.Equal(t, WatchedSubscriptionCache, kept.expires.Sub(kept.readAt), "kept for long while watched")

	// Passes with nothing changed reuse the list.
	for range 3 {
		_, err := d.route(ctx)
		require.NoError(t, err)
	}
	d.cacheMu.Lock()
	require.Same(t, kept, d.cache, "nothing changed, so nothing was read again")
	d.cacheMu.Unlock()

	// Another replica makes a subscription.
	gen := d.changes.Load()
	after := subscribe(t, ctx, there, owner)
	require.Eventually(t, func() bool { return d.changes.Load() > gen }, 10*time.Second, 20*time.Millisecond,
		"told of the change made elsewhere")

	eventID, err := d.Events.Emit(ctx, state.Event{Name: "deploy.failed"})
	require.NoError(t, err)
	want := []string{before, after}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	require.Eventually(t, func() bool {
		_, err := d.route(ctx)
		require.NoError(t, err)
		return len(deliveriesOf(t, ctx, here, eventID)) == 2
	}, 10*time.Second, 100*time.Millisecond, "the event reaches both, without the list's time running out")
	require.Equal(t, want, deliveriesOf(t, ctx, here, eventID))

	// A lost listener is a change too: it may have missed one.
	gen = d.changes.Load()
	_, err = there.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE query LIKE 'LISTEN%' AND pid <> pg_backend_pid()`)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return d.changes.Load() > gen }, 10*time.Second, 20*time.Millisecond)
	require.Eventually(t, d.watching.Load, 15*time.Second, 50*time.Millisecond, "and it listens again")
}

// TestR366_PruningTheOutboxIsBatchedAndKeepsWhatIsStillOwed asserts the
// outbox prune the retention job runs (issue #72): a batch at a time, old
// events whose deliveries are done go with their deliveries, and an event
// with a delivery still pending stays however old (R-366).
func TestR366_PruningTheOutboxIsBatchedAndKeepsWhatIsStillOwed(t *testing.T) {
	t.Parallel()
	ctx, db, owner := outbox(t)
	events := state.NewEvents(db)
	sub := subscribe(t, ctx, db, owner)

	var done []string
	for i := 0; i < 5; i++ {
		eventID, err := events.Emit(ctx, state.Event{Name: "deploy.failed"})
		require.NoError(t, err)
		done = append(done, eventID)
	}
	owed, err := events.Emit(ctx, state.Event{Name: "deploy.failed"})
	require.NoError(t, err)
	_, err = events.Route(ctx, 100, time.Time{}, func(_ context.Context, e state.Event) ([]string, error) {
		if e.Name != "deploy.failed" {
			return nil, nil // what setting up the install emitted
		}
		return []string{sub}, nil
	})
	require.NoError(t, err)
	_, err = db.Exec(ctx, `UPDATE event_deliveries SET status = 'succeeded' WHERE event_id = ANY($1)`, done)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `UPDATE events SET occurred_at = now() - interval '60 days'`)
	require.NoError(t, err)

	before := time.Now().Add(-30 * 24 * time.Hour)
	n, err := events.Prune(ctx, before, 2)
	require.NoError(t, err)
	require.EqualValues(t, 2, n, "one batch")
	for n == 2 {
		n, err = events.Prune(ctx, before, 2)
		require.NoError(t, err)
	}

	var left []string
	rows, err := db.Query(ctx, `SELECT id FROM events`)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		left = append(left, s)
	}
	rows.Close()
	require.Equal(t, []string{owed}, left, "only the event still owed a delivery is kept")
	require.Equal(t, []string{sub}, deliveriesOf(t, ctx, db, owed))
	var orphans int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM event_deliveries WHERE event_id <> $1`, owed).Scan(&orphans))
	require.Zero(t, orphans, "the pruned events' deliveries went with them")
}
