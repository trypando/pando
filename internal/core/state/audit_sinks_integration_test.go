//go:build integration

package state_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
)

type sinkFixture struct {
	db    *state.DB
	owner *pgx.Conn
	sinks *state.AuditSinks
}

func newSinkFixture(t *testing.T) *sinkFixture {
	t.Helper()
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	owner, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(ctx) })
	return &sinkFixture{db: db, owner: owner, sinks: state.NewAuditSinks(db)}
}

// event plants an audit event at a time and returns its stream position.
func (f *sinkFixture) event(t *testing.T, at time.Time, action string) state.AuditCursor {
	t.Helper()
	var txid string
	var id int64
	require.NoError(t, f.owner.QueryRow(context.Background(), `
		INSERT INTO audit_events (occurred_at, principal_kind, principal_id, action)
		VALUES ($1, 'system', 'system', $2) RETURNING txid::text, id`, at, action).Scan(&txid, &id))
	tx, err := strconv.ParseUint(txid, 10, 64)
	require.NoError(t, err)
	return state.AuditCursor{TxID: tx, ID: id}
}

func (f *sinkFixture) sinkRow(t *testing.T, id string) {
	t.Helper()
	require.NoError(t, state.NewAdapters(f.db).Upsert(context.Background(), state.AdapterConfig{
		ID: id, Category: "audit_sink", Kind: "fake", Name: "SIEM", Config: json.RawMessage(`{}`), Enabled: true,
	}))
}

func (f *sinkFixture) enabled(t *testing.T, id string) bool {
	t.Helper()
	var on bool
	require.NoError(t, f.owner.QueryRow(context.Background(), `SELECT enabled FROM adapter_configs WHERE id = $1`, id).Scan(&on))
	return on
}

func requireInternalErr(t *testing.T, err error, message string) {
	t.Helper()
	e := errs.As(err)
	require.NotNil(t, e, "%v", err)
	assert.Equal(t, errs.Internal, e.Code)
	assert.Equal(t, message, e.Message)
}

// TestR382_ASinkStartsAtTheOldestEventOrWhereItIsToldTo asserts R-382's start:
// a new sink with no start has no cursor, so it starts at the oldest event in
// the live log; one given a start is positioned there; and a second Get
// returns the stored state without moving it.
func TestR382_ASinkStartsAtTheOldestEventOrWhereItIsToldTo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSinkFixture(t)

	st, err := f.sinks.Get(ctx, "as_oldest", nil)
	require.NoError(t, err)
	assert.Equal(t, "as_oldest", st.AdapterID)
	assert.False(t, st.HasCursor)

	at := f.event(t, time.Now().UTC(), "test.one")
	st, err = f.sinks.Get(ctx, "as_now", &at)
	require.NoError(t, err)
	assert.True(t, st.HasCursor)
	assert.Equal(t, at.TxID, st.CursorTxID)
	assert.Equal(t, at.ID, st.CursorID)

	again, err := f.sinks.Get(ctx, "as_now", &state.AuditCursor{TxID: 1, ID: 1})
	require.NoError(t, err)
	assert.Equal(t, at.ID, again.CursorID, "a start is read once, when the sink is first seen")

	list, err := f.sinks.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "as_now", list[0].AdapterID, "ordered by adapter")
	assert.Equal(t, "as_oldest", list[1].AdapterID)
}

// TestR383_DeliveryFailureAndRecoveryAreRecorded asserts R-383's bookkeeping: a
// failure counts attempts from when the run began, a delivery clears it and
// reports that it had been failing, and a skip moves the cursor without
// counting a delivery.
func TestR383_DeliveryFailureAndRecoveryAreRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSinkFixture(t)
	t0 := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	e1 := f.event(t, t0.Add(-time.Hour), "test.one")
	e2 := f.event(t, t0.Add(-time.Minute), "test.two")
	_, err := f.sinks.Get(ctx, "as_x", nil)
	require.NoError(t, err)

	st, err := f.sinks.Failed(ctx, "as_x", "the collector answered 503", t0, t0.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, st.Attempts)
	assert.Equal(t, "the collector answered 503", st.LastError)
	require.NotNil(t, st.FailingSince)
	assert.True(t, st.FailingSince.Equal(t0))
	require.NotNil(t, st.NextAttemptAt)
	assert.True(t, st.NextAttemptAt.Equal(t0.Add(time.Minute)))

	st, err = f.sinks.Failed(ctx, "as_x", "still 503", t0.Add(time.Minute), t0.Add(6*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 2, st.Attempts)
	assert.True(t, st.FailingSince.Equal(t0), "a run of failures keeps when it began")
	assert.True(t, st.LastErrorAt.Equal(t0.Add(time.Minute)))

	wasFailing, err := f.sinks.Delivered(ctx, "as_x", e1.TxID, e1.ID, 1, t0.Add(2*time.Minute))
	require.NoError(t, err)
	assert.True(t, wasFailing, "the recovery is reported, so it is audited")

	st, err = f.sinks.Get(ctx, "as_x", nil)
	require.NoError(t, err)
	assert.Zero(t, st.Attempts)
	assert.Empty(t, st.LastError)
	assert.Nil(t, st.FailingSince)
	assert.Nil(t, st.NextAttemptAt)
	assert.EqualValues(t, 1, st.DeliveredCount)
	assert.Equal(t, e1.ID, st.CursorID)
	require.NotNil(t, st.CursorAt)
	assert.True(t, st.CursorAt.Equal(t0.Add(-time.Hour)), "the cursor's time is its event's")

	wasFailing, err = f.sinks.Delivered(ctx, "as_x", e1.TxID, e1.ID, 0, t0.Add(3*time.Minute))
	require.NoError(t, err)
	assert.False(t, wasFailing)

	require.NoError(t, f.sinks.Skipped(ctx, "as_x", e2.TxID, e2.ID, t0.Add(4*time.Minute)))
	st, err = f.sinks.Get(ctx, "as_x", nil)
	require.NoError(t, err)
	assert.Equal(t, e2.ID, st.CursorID, "a skip moves the cursor")
	assert.True(t, st.CursorAt.Equal(t0.Add(-time.Minute)))
	assert.EqualValues(t, 1, st.DeliveredCount, "and delivers nothing")
	assert.True(t, st.DeliveredAt.Equal(t0.Add(3*time.Minute)))
}

// TestR383_DisablingASinkTurnsItsAdapterOffOnce asserts R-383: Pando turning a
// sink off records why, turns its adapter row off, and reports a change only
// the first time; a declared sink with no row is turned off in its state.
func TestR383_DisablingASinkTurnsItsAdapterOffOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSinkFixture(t)
	at := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)
	f.sinkRow(t, "as_row")
	_, err := f.sinks.Get(ctx, "as_row", nil)
	require.NoError(t, err)
	_, err = f.sinks.Failed(ctx, "as_row", "down", at, at.Add(time.Hour))
	require.NoError(t, err)

	changed, err := f.sinks.Disable(ctx, "as_row", "it kept failing", at)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.False(t, f.enabled(t, "as_row"))
	st, err := f.sinks.Get(ctx, "as_row", nil)
	require.NoError(t, err)
	require.NotNil(t, st.DisabledAt)
	assert.True(t, st.DisabledAt.Equal(at))
	assert.Equal(t, "it kept failing", st.DisabledReason)
	assert.Nil(t, st.NextAttemptAt, "a sink that is off is not due")

	changed, err = f.sinks.Disable(ctx, "as_row", "again", at.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, changed, "turned off once")
	st, err = f.sinks.Get(ctx, "as_row", nil)
	require.NoError(t, err)
	assert.Equal(t, "it kept failing", st.DisabledReason)

	_, err = f.sinks.Get(ctx, "as_declared", nil)
	require.NoError(t, err)
	changed, err = f.sinks.Disable(ctx, "as_declared", "it kept failing", at)
	require.NoError(t, err)
	assert.True(t, changed, "a declared sink has no row, and its state records it")

	changed, err = f.sinks.Disable(ctx, "as_unknown", "no state", at)
	require.NoError(t, err)
	assert.False(t, changed, "a sink with no state has nothing to turn off")
}

// archive records an archive of [first, last] whose rows have left the live
// log, as the archiver leaves one after dropping its month.
func (f *sinkFixture) archive(t *testing.T, id string, first, last time.Time, lastID int64) {
	t.Helper()
	_, err := f.owner.Exec(context.Background(), `
		INSERT INTO audit_archives (id, month, object_name, row_count, first_id, last_id, first_at, last_at, size_bytes, sha256)
		VALUES ($1, date_trunc('month', $2::timestamptz AT TIME ZONE 'UTC')::date, $1, 1, $4, $4, $2, $3, 1, repeat('a', 64))`,
		id, first, last, lastID)
	require.NoError(t, err)
}

// TestR386_ResumingRecordsWhatWasArchivedAwayWhileOff asserts R-386: a sink
// turned back on is cleared, and the range it missed — archived months past
// its cursor whose rows are gone — is recorded as its gap, starting no earlier
// than its cursor. An archive whose rows are still live is not a gap.
func TestR386_ResumingRecordsWhatWasArchivedAwayWhileOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSinkFixture(t)
	at := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	jan := time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2025, 2, 20, 0, 0, 0, 0, time.UTC)

	// No cursor: everything archived away is missed.
	_, err := f.sinks.Get(ctx, "as_fresh", nil)
	require.NoError(t, err)
	_, err = f.sinks.Disable(ctx, "as_fresh", "off", at)
	require.NoError(t, err)
	f.archive(t, "aar_jan", jan, jan.Add(48*time.Hour), 1_000_001)
	f.archive(t, "aar_feb", feb, feb.Add(48*time.Hour), 1_000_002)
	live := f.event(t, time.Date(2025, 3, 3, 0, 0, 0, 0, time.UTC), "test.live")
	f.archive(t, "aar_live", time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 3, 3, 0, 0, 0, 0, time.UTC), live.ID)

	st, err := f.sinks.Resume(ctx, "as_fresh", at)
	require.NoError(t, err)
	assert.Nil(t, st.DisabledAt)
	assert.Empty(t, st.DisabledReason)
	assert.Zero(t, st.Attempts)
	require.NotNil(t, st.GapFrom)
	require.NotNil(t, st.GapTo)
	assert.True(t, st.GapFrom.Equal(jan), "from the first archived event")
	assert.True(t, st.GapTo.Equal(feb.Add(48*time.Hour)), "to the last one gone; the live archive is not a gap")

	// A cursor inside January: the gap starts at the cursor.
	mid := f.event(t, jan.Add(24*time.Hour), "test.mid")
	_, err = f.sinks.Get(ctx, "as_mid", &mid)
	require.NoError(t, err)
	_, err = f.sinks.Delivered(ctx, "as_mid", mid.TxID, mid.ID, 1, at)
	require.NoError(t, err)
	_, err = f.sinks.Disable(ctx, "as_mid", "off", at)
	require.NoError(t, err)
	st, err = f.sinks.Resume(ctx, "as_mid", at)
	require.NoError(t, err)
	require.NotNil(t, st.GapFrom)
	assert.True(t, st.GapFrom.Equal(jan.Add(24*time.Hour)), "nothing before the cursor was missed")

	// A cursor past every archive: no gap, and none invented.
	late := f.event(t, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), "test.late")
	_, err = f.sinks.Get(ctx, "as_late", &late)
	require.NoError(t, err)
	_, err = f.sinks.Delivered(ctx, "as_late", late.TxID, late.ID, 1, at)
	require.NoError(t, err)
	_, err = f.sinks.Disable(ctx, "as_late", "off", at)
	require.NoError(t, err)
	st, err = f.sinks.Resume(ctx, "as_late", at)
	require.NoError(t, err)
	assert.Nil(t, st.GapFrom)
	assert.Nil(t, st.GapTo)
	assert.Nil(t, st.DisabledAt)
}

// TestR386_ASinkHoldsOnlyMonthsWithEventsItHasNotBeenSent asserts R-386's
// question, and that the backlog counts what is past the cursor up to a cap.
func TestR386_ASinkHoldsOnlyMonthsWithEventsItHasNotBeenSent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSinkFixture(t)
	mar := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	apr := mar.AddDate(0, 1, 0)
	first := f.event(t, mar.Add(time.Hour), "test.one")
	f.event(t, mar.Add(2*time.Hour), "test.two")
	f.event(t, mar.Add(3*time.Hour), "test.three")

	fresh, err := f.sinks.Get(ctx, "as_fresh", nil)
	require.NoError(t, err)
	held, err := f.sinks.Holds(ctx, fresh, mar, apr)
	require.NoError(t, err)
	assert.True(t, held, "a sink with no cursor holds every month with events")
	held, err = f.sinks.Holds(ctx, fresh, apr, apr.AddDate(0, 1, 0))
	require.NoError(t, err)
	assert.False(t, held, "but not an empty one")

	n, err := f.sinks.Backlog(ctx, fresh, 100)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	n, err = f.sinks.Backlog(ctx, fresh, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "counted up to the cap")

	part, err := f.sinks.Get(ctx, "as_part", &first)
	require.NoError(t, err)
	n, err = f.sinks.Backlog(ctx, part, 100)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	held, err = f.sinks.Holds(ctx, part, mar, apr)
	require.NoError(t, err)
	assert.True(t, held)

	// Sent the whole month, it holds nothing.
	var last state.AuditCursor
	var txid string
	require.NoError(t, f.owner.QueryRow(ctx, `SELECT txid::text, id FROM audit_events ORDER BY txid DESC, id DESC LIMIT 1`).Scan(&txid, &last.ID))
	last.TxID, err = strconv.ParseUint(txid, 10, 64)
	require.NoError(t, err)
	done, err := f.sinks.Get(ctx, "as_done", &last)
	require.NoError(t, err)
	held, err = f.sinks.Holds(ctx, done, mar, apr)
	require.NoError(t, err)
	assert.False(t, held)
	n, err = f.sinks.Backlog(ctx, done, 100)
	require.NoError(t, err)
	assert.Zero(t, n)
}

// TestR383_SinkStateFailsReadablyWithoutADatabase asserts every read and write
// of delivery state reports a lost database as an internal failure in words.
func TestR383_SinkStateFailsReadablyWithoutADatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := statetest.Connect(t)
	db.Close()
	s := state.NewAuditSinks(db)
	at := time.Now().UTC()

	_, err := s.List(ctx)
	requireInternalErr(t, err, "Could not read the audit sinks' delivery state.")
	_, err = s.Get(ctx, "as_x", nil)
	requireInternalErr(t, err, "Could not record an audit sink's delivery state.")
	_, err = s.Delivered(ctx, "as_x", 1, 1, 1, at)
	requireInternalErr(t, err, "Could not record an audit sink's delivery.")
	err = s.Skipped(ctx, "as_x", 1, 1, at)
	requireInternalErr(t, err, "Could not record an audit sink's position.")
	_, err = s.Failed(ctx, "as_x", "down", at, at)
	requireInternalErr(t, err, "Could not record an audit sink's failure.")
	_, err = s.Disable(ctx, "as_x", "off", at)
	requireInternalErr(t, err, "Could not turn off an audit sink.")
	_, err = s.Resume(ctx, "as_x", at)
	requireInternalErr(t, err, "Could not turn an audit sink back on.")
	_, err = s.Backlog(ctx, state.AuditSinkState{}, 10)
	requireInternalErr(t, err, "Could not count an audit sink's backlog.")
	_, err = s.Holds(ctx, state.AuditSinkState{}, at, at)
	requireInternalErr(t, err, "Could not read what an audit sink has been sent.")
}

// TestR383_ADisableThatCannotFinishChangesNothing asserts turning a sink off
// is one transaction: when either of its writes cannot finish, neither its
// state nor its adapter row is changed.
func TestR383_ADisableThatCannotFinishChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSinkFixture(t)
	at := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	f.sinkRow(t, "as_row")
	_, err := f.sinks.Get(ctx, "as_row", nil)
	require.NoError(t, err)

	for _, lock := range []string{
		`SELECT 1 FROM audit_sink_state WHERE adapter_id = 'as_row' FOR UPDATE`, // the first write waits
		`SELECT 1 FROM adapter_configs WHERE id = 'as_row' FOR UPDATE`,          // the second write waits
	} {
		tx, err := f.owner.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, lock)
		require.NoError(t, err)

		short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		changed, err := f.sinks.Disable(short, "as_row", "off", at)
		cancel()
		requireInternalErr(t, err, "Could not turn off an audit sink.")
		assert.False(t, changed)
		require.NoError(t, tx.Rollback(ctx))

		st, err := f.sinks.Get(ctx, "as_row", nil)
		require.NoError(t, err)
		assert.Nil(t, st.DisabledAt, lock)
		assert.True(t, f.enabled(t, "as_row"), lock)
	}
}
