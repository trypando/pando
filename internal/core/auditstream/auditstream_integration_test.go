//go:build integration

package auditstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/auditstream"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
)

// fakeSink records what it was sent, and fails while fail is set.
type fakeSink struct {
	mu      sync.Mutex
	got     []string
	fail    error
	exclude []string
}

func (f *fakeSink) Kind() string                                     { return "fake" }
func (f *fakeSink) Category() api.Category                           { return api.CategoryAuditSink }
func (f *fakeSink) Configure(context.Context, json.RawMessage) error { return nil }
func (f *fakeSink) HealthCheck(context.Context) error                { return nil }
func (f *fakeSink) AuditSinkCapabilities() api.AuditSinkCapabilities {
	return api.AuditSinkCapabilities{MaxBatch: 2, Format: api.AuditFormatNative, Transport: "https", Endpoint: "siem.example.com:443", Exclude: f.exclude}
}
func (f *fakeSink) Send(_ context.Context, b api.AuditBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, b.Actions...)
	return nil
}
func (f *fakeSink) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

type fixture struct {
	db       *state.DB
	ownerURL string
	owner    *pgx.Conn
	writer   *audit.Writer
	clock    *clock.Fake
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	owner, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(ctx) })
	return &fixture{db: db, ownerURL: ownerURL, owner: owner, writer: audit.New(db.Pool), clock: clock.NewFake(time.Now().UTC())}
}

func (f *fixture) service(declared []auditstream.Declared, newSink func(string) api.AuditSinkAdapter) *auditstream.Service {
	return &auditstream.Service{
		Configs:  state.NewAdapters(f.db),
		Declared: declared,
		New:      newSink,
		States:   state.NewAuditSinks(f.db),
		Reader:   audit.NewReader(f.db.Pool),
		Audit:    func(ctx context.Context, e audit.Event) { _ = f.writer.Write(ctx, e) },
		Clock:    f.clock,
		Logger:   zap.NewNop(),
	}
}

func (f *fixture) write(t *testing.T, actions ...string) {
	t.Helper()
	for _, a := range actions {
		require.NoError(t, f.writer.Write(context.Background(), audit.Event{PrincipalKind: audit.KindSystem, PrincipalID: "system", Action: a}))
	}
}

func (f *fixture) count(t *testing.T, action string) int {
	t.Helper()
	var n int
	require.NoError(t, f.owner.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = $1`, action).Scan(&n))
	return n
}

// drain passes until every enabled sink has been sent everything. A pass
// reads only up to the horizon, the oldest transaction running anywhere in the
// cluster (design 12 §2), which other tests sharing it hold back; so "caught
// up" is no backlog, not one quiet pass.
func drain(t *testing.T, s *auditstream.Service) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, err := s.Pass(context.Background())
		require.NoError(t, err)
		statuses, err := s.Statuses(context.Background())
		require.NoError(t, err)
		done := true
		for _, st := range statuses {
			if st.Enabled && st.DisabledAt == nil && st.Backlog > 0 {
				done = false
			}
		}
		if done {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the stream never caught up")
}

// TestR382_ASinkReceivesEveryEventInOrderAndResumes asserts R-382 and R-381
// through a sink: every event, in order, in batches no larger than the sink
// asks for, and a new process carries on from the stored cursor.
func TestR382_ASinkReceivesEveryEventInOrderAndResumes(t *testing.T) {
	f := newFixture(t)
	sk := &fakeSink{exclude: []string{"app.use"}}
	f.write(t, "test.one", "app.use", "test.two", "test.three")

	s := f.service([]auditstream.Declared{{ID: "as_siem", Adapter: sk}}, nil)
	drain(t, s)
	got := sk.received()
	require.GreaterOrEqual(t, len(got), 3)
	assert.Equal(t, []string{"test.one", "test.two", "test.three"}, got[:3], "in order, app.use excluded")

	// Another process, as after a restart, sends only what is new.
	f.write(t, "test.four")
	again := f.service([]auditstream.Declared{{ID: "as_siem", Adapter: sk}}, nil)
	drain(t, again)
	got = sk.received()
	assert.Equal(t, "test.four", got[len(got)-1])
	assert.Equal(t, 1, countOf(got, "test.one"), "nothing was sent twice")

	statuses, err := again.Statuses(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Zero(t, statuses[0].Backlog)
	assert.NotNil(t, statuses[0].DeliveredAt)
	assert.Equal(t, "Every audit event except app.use is sent to siem.example.com:443 over HTTPS.", auditstream.Describe(statuses[0]))
}

// memStore keeps archives in memory.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

type memWriter struct {
	bytes.Buffer
	store *memStore
	name  string
}

func (w *memWriter) Close() error {
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	w.store.objects[w.name] = w.Bytes()
	return nil
}

func (m *memStore) Writer(_ context.Context, name string) (io.WriteCloser, error) {
	return &memWriter{store: m, name: name}, nil
}

func (m *memStore) Reader(_ context.Context, name string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[name]
	if !ok {
		return nil, errors.New("no object " + name)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func countOf(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return n
}

// TestR383_AFailingSinkIsRetriedAuditedOnceAndTurnedOff asserts R-383.
func TestR383_AFailingSinkIsRetriedAuditedOnceAndTurnedOff(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sk := &fakeSink{fail: errors.New("the collector answered 503")}
	require.NoError(t, state.NewAdapters(f.db).Upsert(ctx, state.AdapterConfig{
		ID: "as_down", Category: "audit_sink", Kind: "fake", Name: "Down SIEM", Config: json.RawMessage(`{}`), Enabled: true,
	}))
	f.write(t, "test.one")
	s := f.service(nil, func(string) api.AuditSinkAdapter { return sk })

	_, err := s.Pass(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(t, "audit.sink.fail"), "the first failure is audited")

	// Retried on the schedule, not every pass; each failure after the first
	// is not audited again.
	_, err = s.Pass(ctx)
	require.NoError(t, err)
	st, err := state.NewAuditSinks(f.db).Get(ctx, "as_down", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, st.Attempts, "not due yet")

	for i := 0; i < 6; i++ {
		f.clock.Advance(5 * time.Hour)
		_, err = s.Pass(ctx)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, f.count(t, "audit.sink.fail"))
	assert.Equal(t, 1, f.count(t, "audit.sink.disable"), "turned off once, after a day and five attempts")

	var enabled bool
	require.NoError(t, f.owner.QueryRow(ctx, `SELECT enabled FROM adapter_configs WHERE id = 'as_down'`).Scan(&enabled))
	assert.False(t, enabled, "its adapter is off, so it stops holding archival")

	// Turned back on and working, it resumes and its recovery is audited.
	sk.fail = nil
	_, err = f.owner.Exec(ctx, `UPDATE adapter_configs SET enabled = true WHERE id = 'as_down'`)
	require.NoError(t, err)
	drain(t, s)
	assert.Contains(t, sk.received(), "test.one")
}

// TestR386_ArchivingWaitsForAnEnabledSink asserts R-386: a month an enabled
// sink has not been sent stays in the live log, unarchived, and the hold is
// audited; once sent, it is archived; a sink Pando turned off does not hold.
func TestR386_ArchivingWaitsForAnEnabledSink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, -6, 0)
	month := time.Date(old.Year(), old.Month(), 1, 0, 0, 0, 0, time.UTC)
	_, err := f.owner.Exec(ctx, `INSERT INTO audit_events (occurred_at, principal_kind, principal_id, action)
		VALUES ($1, 'system', 'system', 'test.old')`, month.Add(48*time.Hour))
	require.NoError(t, err)

	sk := &fakeSink{fail: errors.New("down")}
	s := f.service([]auditstream.Declared{{ID: "as_hold", Adapter: sk}}, nil)

	holders, err := s.Holding(ctx, month, month.AddDate(0, 1, 0))
	require.NoError(t, err)
	assert.Equal(t, []string{"as_hold"}, holders, "a sink that has not been sent the month holds it")

	archiver := &audit.Archiver{
		Pool:      statetest.Archiver(t, f.ownerURL),
		Stores:    audit.Stores{Kept: newMemStore()},
		Retention: func(context.Context) (audit.Retention, error) { return audit.Retention{}, nil },
		Clock:     clock.System{},
		Logger:    zap.NewNop(),
		Holds:     s.Holding,
		Audit:     func(ctx context.Context, e audit.Event) { _ = f.writer.Write(ctx, e) },
	}
	recs, err := archiver.Pass(ctx)
	require.NoError(t, err)
	assert.Empty(t, recs, "nothing archived while held")
	assert.Equal(t, 1, f.count(t, "test.old"), "the month is still in the live log")
	assert.Equal(t, 1, f.count(t, "audit.archive.held"))

	_, err = archiver.Pass(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(t, "audit.archive.held"), "the hold is audited once per month")

	sk.fail = nil
	drain(t, s)
	holders, err = s.Holding(ctx, month, month.AddDate(0, 1, 0))
	require.NoError(t, err)
	assert.Empty(t, holders, "once sent, the month is not held")
	recs, err = archiver.Pass(ctx)
	require.NoError(t, err)
	assert.Len(t, recs, 1, "and it is archived")
}
