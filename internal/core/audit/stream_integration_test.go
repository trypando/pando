//go:build integration

package audit_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/clientaddr"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
)

// collect reads from after until want events have been read, or 30 seconds
// pass. The horizon is the oldest transaction running anywhere in the cluster
// (design 12 §2), which other tests sharing it hold back, so events appear
// when it moves rather than at once.
func collect(t *testing.T, r *audit.Reader, q audit.StreamQuery, want int) ([]string, audit.Cursor) {
	t.Helper()
	var got []string
	deadline := time.Now().Add(30 * time.Second)
	for len(got) < want && time.Now().Before(deadline) {
		page, err := r.Wait(context.Background(), q, time.Second, clock.System{})
		require.NoError(t, err)
		got = append(got, actionsOf(t, page)...)
		q.After = page.Cursor
	}
	return got, q.After
}

func actionsOf(t *testing.T, page audit.StreamPage) []string {
	t.Helper()
	var out []string
	for _, raw := range page.Events {
		var l audit.Line
		require.NoError(t, json.Unmarshal(raw, &l))
		out = append(out, l.Action)
	}
	return out
}

// TestR381_AnEventThatCommitsLateIsNotPassedByTheCursor asserts R-381's
// ordering: an event whose transaction began first but commits after a later
// one is still delivered after a cursor that has read the later one.
func TestR381_AnEventThatCommitsLateIsNotPassedByTheCursor(t *testing.T) {
	r := newRetention(t)
	ctx := context.Background()
	reader := audit.NewReader(r.app)

	// The early writer takes its id and holds its transaction open.
	early, err := r.app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = early.Rollback(ctx) }()
	var earlyID int64
	require.NoError(t, early.QueryRow(ctx, `
		INSERT INTO audit_events (principal_kind, action) VALUES ('anonymous', 'test.early') RETURNING id`).Scan(&earlyID))

	// The late writer takes a higher id and commits first.
	require.NoError(t, audit.New(r.app).Write(ctx, audit.Event{PrincipalKind: audit.KindSystem, PrincipalID: "system", Action: "test.late"}))

	page, err := reader.Stream(ctx, audit.StreamQuery{})
	require.NoError(t, err)
	assert.Empty(t, actionsOf(t, page), "nothing is past the horizon while the early writer is open")
	assert.True(t, page.CaughtUp)

	require.NoError(t, early.Commit(ctx))

	got, cursor := collect(t, reader, audit.StreamQuery{After: page.Cursor, Actions: []string{"test."}}, 2)
	assert.Equal(t, []string{"test.early", "test.late"}, got, "both, the early one first, after a cursor that read neither")
	_ = earlyID

	// Resuming from the cursor reads neither again.
	again, err := reader.Stream(ctx, audit.StreamQuery{After: cursor, Actions: []string{"test."}})
	require.NoError(t, err)
	assert.Empty(t, again.Events)

	parsed, err := audit.ParseCursor(cursor.String())
	require.NoError(t, err)
	assert.Equal(t, cursor, parsed, "the wire form round-trips")
}

// TestR381_AFilteredReaderMovesPastWhatItDrops asserts the cursor advances
// past filtered events, and that "now" starts after the newest settled event.
func TestR381_AFilteredReaderMovesPastWhatItDrops(t *testing.T) {
	r := newRetention(t)
	ctx := context.Background()
	reader := audit.NewReader(r.app)
	w := audit.New(r.app)

	start, err := reader.Now(ctx)
	require.NoError(t, err)
	for _, a := range []string{"app.use", "app.use", "grant.create", "app.use"} {
		require.NoError(t, w.Write(ctx, audit.Event{PrincipalKind: audit.KindAnonymous, Action: a, AppID: "app_x"}))
	}
	got, cursor := collect(t, reader, audit.StreamQuery{After: start, Exclude: []string{"app.use"}}, 1)
	assert.Equal(t, []string{"grant.create"}, got, "app.use excluded")

	// The cursor moved past every event the filtered reads scanned, the
	// app.use events before grant.create among them, so an unfiltered read
	// from it sees none of those: at most the app.use written after, if the
	// horizon had not passed it yet.
	rest, err := reader.Stream(ctx, audit.StreamQuery{After: cursor})
	require.NoError(t, err)
	restActions := actionsOf(t, rest)
	assert.LessOrEqual(t, len(restActions), 1)
	for _, a := range restActions {
		assert.Equal(t, "app.use", a, "nothing the filtered reader already moved past comes back")
	}

	// A waiting read with nothing new returns when its wait runs out.
	began := time.Now()
	waited, err := reader.Wait(ctx, audit.StreamQuery{After: cursor, Actions: []string{"nothing."}}, 1500*time.Millisecond, clock.System{})
	require.NoError(t, err)
	assert.Empty(t, waited.Events)
	assert.GreaterOrEqual(t, time.Since(began), time.Second)

	// Export reads the same events through an encoder, once the horizon has
	// passed them all.
	require.Eventually(t, func() bool {
		lines := 0
		n, err := reader.Export(ctx, audit.StreamQuery{After: start, Actions: []string{"app.use"}}, nil,
			func([]byte) error { lines++; return nil })
		require.NoError(t, err)
		return n == 3 && lines == 3
	}, 30*time.Second, 250*time.Millisecond)
}

// TestR379_EveryEventCarriesActorTargetOutcomeAndSource asserts R-379: the
// writer fills in the target, the outcome, the source and the actor's name,
// so the hundred places an event is built cannot forget them.
func TestR379_EveryEventCarriesActorTargetOutcomeAndSource(t *testing.T) {
	r := newRetention(t)
	ctx := context.Background()
	_, err := r.owner.Exec(ctx, `INSERT INTO identity_adapters (id, kind, name) VALUES ('ida_t', 'local', 'local') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = r.owner.Exec(ctx, `INSERT INTO users (id, adapter_id, external_id, email, display_name)
		VALUES ('usr_ada', 'ida_t', 'ada', 'ada@example.com', 'Ada Lovelace')`)
	require.NoError(t, err)

	reqCtx := clientaddr.With(ctx, clientaddr.Request{SourceIP: "198.51.100.2", PeerIP: "10.0.0.5", UserAgent: "curl/8"})
	w := audit.New(r.app)
	require.NoError(t, w.Write(reqCtx, audit.Event{PrincipalKind: audit.KindToken, PrincipalID: "tok_1", OnBehalfOf: "usr_ada", Action: "app.restart", AppID: "app_1"}))
	require.NoError(t, w.Write(ctx, audit.Event{PrincipalKind: audit.KindUser, PrincipalID: "usr_ada", Action: "session.denied"}))
	require.NoError(t, w.Write(ctx, audit.Event{PrincipalKind: audit.KindSystem, PrincipalID: "system", Action: "deploy.finish", Outcome: audit.OutcomeFailed}))

	recs, err := audit.NewReader(r.app).List(ctx, audit.Query{})
	require.NoError(t, err)
	require.Len(t, recs, 3)
	finish, denied, restart := recs[0], recs[1], recs[2]

	assert.Equal(t, "app", restart.TargetKind)
	assert.Equal(t, "app_1", restart.TargetID, "an event about an app with no narrower target is about the app")
	assert.Equal(t, "success", restart.Outcome)
	assert.Equal(t, "198.51.100.2", restart.SourceIP)
	assert.Equal(t, "10.0.0.5", restart.PeerIP)
	assert.Equal(t, "curl/8", restart.UserAgent)
	assert.Equal(t, "Ada Lovelace", restart.ActorName, "a token acting for someone is that person")
	assert.Equal(t, "ada@example.com", restart.ActorEmail)
	assert.Equal(t, 2, restart.SchemaVersion)

	assert.Equal(t, "denied", denied.Outcome)
	assert.Equal(t, "install", denied.TargetKind, "an event about nothing narrower is about the installation")
	assert.Empty(t, denied.SourceIP, "no request, no source")

	assert.Equal(t, "failed", finish.Outcome, "an explicit outcome wins")
	assert.Empty(t, finish.ActorName)
}
