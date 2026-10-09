//go:build integration

package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
)

// closedReader is a reader whose database connection has gone.
func closedReader(t *testing.T) *audit.Reader {
	t.Helper()
	db, _ := statetest.Connect(t)
	db.Close()
	return audit.NewReader(db.Pool)
}

// requireInternal asserts err is an internal failure with a message a person
// can read, not a driver's.
func requireInternal(t *testing.T, err error, message string) {
	t.Helper()
	e := errs.As(err)
	require.NotNil(t, e, "%v", err)
	assert.Equal(t, errs.Internal, e.Code)
	assert.Equal(t, message, e.Message)
}

// exportActions is every action Export writes for q, natively.
func exportActions(t *testing.T, r *audit.Reader, q audit.StreamQuery) []string {
	t.Helper()
	var got []string
	_, err := r.Export(context.Background(), q, nil, func(line []byte) error {
		var l audit.Line
		require.NoError(t, json.Unmarshal(line, &l))
		got = append(got, l.Action)
		return nil
	})
	require.NoError(t, err)
	return got
}

// TestR381_EveryStreamReadFailsReadablyWithoutADatabase asserts each read of
// the stream reports a lost database as an internal failure in words, never a
// partial page passed off as the end of the log.
func TestR381_EveryStreamReadFailsReadablyWithoutADatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := closedReader(t)

	_, err := r.Stream(ctx, audit.StreamQuery{})
	requireInternal(t, err, "Could not read the audit stream.")

	_, err = r.Now(ctx)
	requireInternal(t, err, "Could not read the audit stream.")

	_, err = r.ResolveCursor(ctx, audit.CursorNow)
	requireInternal(t, err, "Could not read the audit stream.")

	_, err = r.Wait(ctx, audit.StreamQuery{}, time.Second, nil)
	requireInternal(t, err, "Could not read the audit stream.")

	n, err := r.Export(ctx, audit.StreamQuery{}, nil, func([]byte) error { return nil })
	requireInternal(t, err, "Could not read the audit stream.")
	assert.Zero(t, n)

	_, _, err = r.Oldest(ctx)
	requireInternal(t, err, "Could not read the audit log.")
}

// TestR381_AStreamReadThatCannotFinishIsAnError asserts a read the database
// does not answer in time is an error, not an empty, caught-up page.
func TestR381_AStreamReadThatCannotFinishIsAnError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)

	tx, err := r.owner.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `LOCK TABLE audit_events IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	page, err := audit.NewReader(r.app).Stream(short, audit.StreamQuery{})
	requireInternal(t, err, "Could not read the audit stream.")
	assert.False(t, page.CaughtUp)
}

// TestR387_AnEmptyLogHasNoOldestEventAndNowIsTheStart asserts that on an empty
// live log "now" is the start of the stream and there is no oldest event.
func TestR387_AnEmptyLogHasNoOldestEventAndNowIsTheStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	require.Zero(t, r.count(t, "true"), "a fresh database has no audit events")
	reader := audit.NewReader(r.app)

	now, err := reader.ResolveCursor(ctx, audit.CursorNow)
	require.NoError(t, err)
	assert.True(t, now.IsZero())

	_, ok, err := reader.Oldest(ctx)
	require.NoError(t, err)
	assert.False(t, ok)

	parsed, err := reader.ResolveCursor(ctx, "c1.5.6")
	require.NoError(t, err)
	assert.Equal(t, audit.Cursor{TxID: 5, ID: 6}, parsed, "anything but now is parsed")
}

// TestR387_OldestIsTheEarliestEventInTheLiveLog asserts R-387's bound: Oldest
// is when the earliest event in the live log happened, in UTC.
func TestR387_OldestIsTheEarliestEventInTheLiveLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	first := time.Date(2025, 3, 10, 12, 30, 0, 0, time.UTC)
	r.plant(t, time.Date(2025, 4, 2, 8, 0, 0, 0, time.UTC), "app.deploy")
	r.plant(t, first, "app.create")

	at, ok, err := audit.NewReader(r.app).Oldest(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, first, at)
	assert.Equal(t, time.UTC, at.Location())
}

// TestR387_AnExportOfARangeReadsOnlyThatRange asserts R-387's range: Since is
// inclusive, Until exclusive, and an unbounded side reads to that end.
func TestR387_AnExportOfARangeReadsOnlyThatRange(t *testing.T) {
	t.Parallel()
	r := newRetention(t)
	r.plant(t, time.Date(2025, 3, 31, 23, 59, 59, 0, time.UTC), "test.march")
	r.plant(t, time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC), "test.april")
	r.plant(t, time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC), "test.may")
	reader := audit.NewReader(r.app)

	require.Eventually(t, func() bool {
		return len(exportActions(t, reader, audit.StreamQuery{})) == 3
	}, 30*time.Second, 100*time.Millisecond, "every planted event settles")

	april, may := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, []string{"test.april"}, exportActions(t, reader, audit.StreamQuery{Since: april, Until: may}))
	assert.ElementsMatch(t, []string{"test.april", "test.may"}, exportActions(t, reader, audit.StreamQuery{Since: april}))
	assert.ElementsMatch(t, []string{"test.march", "test.april"}, exportActions(t, reader, audit.StreamQuery{Until: may}))

	page, err := reader.Stream(context.Background(), audit.StreamQuery{Since: may})
	require.NoError(t, err)
	assert.Equal(t, []string{"test.may"}, actionsOf(t, page), "Stream applies the same bounds")
}

// TestR387_AnExportLongerThanAPageIsReadWhole asserts Export pages from its
// cursor rather than stopping at one page.
func TestR387_AnExportLongerThanAPageIsReadWhole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	_, err := r.owner.Exec(ctx, `
		INSERT INTO audit_events (occurred_at, principal_kind, principal_id, action)
		SELECT now() - interval '1 day', 'system', 'system', 'test.bulk' FROM generate_series(1, 1001)`)
	require.NoError(t, err)
	reader := audit.NewReader(r.app)

	var n int
	require.Eventually(t, func() bool {
		lines := 0
		n, err = reader.Export(ctx, audit.StreamQuery{Actions: []string{"test.bulk"}}, nil, func([]byte) error { lines++; return nil })
		require.NoError(t, err)
		return n == 1001 && lines == 1001
	}, 30*time.Second, 100*time.Millisecond)
	assert.Equal(t, 1001, n, "more than one 1000-event page")
}

// TestR387_AnExportStopsAtTheFirstEncoderOrWriteFailure asserts an export
// whose encoder or writer fails returns that failure and how many lines were
// written before it, and that an encoder's output is what is written.
func TestR387_AnExportStopsAtTheFirstEncoderOrWriteFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	r.plant(t, time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC), "test.one")
	r.plant(t, time.Date(2025, 3, 11, 12, 0, 0, 0, time.UTC), "test.two")
	reader := audit.NewReader(r.app)
	require.Eventually(t, func() bool {
		return len(exportActions(t, reader, audit.StreamQuery{})) == 2
	}, 30*time.Second, 100*time.Millisecond)

	var written []string
	n, err := reader.Export(ctx, audit.StreamQuery{},
		func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{"encoded":true}`), nil },
		func(line []byte) error { written = append(written, string(line)); return nil })
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, []string{`{"encoded":true}`, `{"encoded":true}`}, written)

	encErr := errors.New("cannot encode")
	n, err = reader.Export(ctx, audit.StreamQuery{},
		func(json.RawMessage) (json.RawMessage, error) { return nil, encErr },
		func([]byte) error { t.Fatal("nothing is written after the encoder fails"); return nil })
	require.ErrorIs(t, err, encErr)
	assert.Zero(t, n)

	writeErr := errors.New("disk full")
	calls := 0
	n, err = reader.Export(ctx, audit.StreamQuery{}, nil, func([]byte) error {
		calls++
		if calls == 2 {
			return writeErr
		}
		return nil
	})
	require.ErrorIs(t, err, writeErr)
	assert.Equal(t, 1, n, "the line before the failure was written")
}

// TestR381_AWaitingReadEndsWithItsContext asserts a long poll returns, without
// an error and with nothing read, as soon as its caller goes away, and that a
// nil clock is the system clock.
func TestR381_AWaitingReadEndsWithItsContext(t *testing.T) {
	t.Parallel()
	r := newRetention(t)
	reader := audit.NewReader(r.app)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	began := time.Now()
	page, err := reader.Wait(ctx, audit.StreamQuery{Actions: []string{"nothing."}}, 30*time.Second, nil)
	require.NoError(t, err)
	assert.Empty(t, page.Events)
	assert.Less(t, time.Since(began), 10*time.Second, "it did not wait out the 30 seconds")
}
