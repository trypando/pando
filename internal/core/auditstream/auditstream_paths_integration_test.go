//go:build integration

package auditstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/auditstream"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// rows is a ConfigStore in memory, or one that cannot be read.
type rows struct {
	list []state.AdapterConfig
	err  error
}

func (r rows) List(context.Context) ([]state.AdapterConfig, error) { return r.list, r.err }

func sinkRow(id, kind string, config string) state.AdapterConfig {
	return state.AdapterConfig{ID: id, Category: string(api.CategoryAuditSink), Kind: kind, Name: "Name of " + id,
		Config: json.RawMessage(config), Enabled: true}
}

type holders struct {
	people []string
	err    error
}

func (h holders) InstallVerbHolders(_ context.Context, verb authz.Verb) ([]string, error) {
	if verb != authz.InstallAuditExport {
		return nil, errors.New("asked about the wrong verb: " + string(verb))
	}
	return h.people, h.err
}

type notifier struct {
	mu   sync.Mutex
	sent []api.Notification
}

func (n *notifier) Notify(_ context.Context, m api.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, m)
	return nil
}

func (n *notifier) all() []api.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]api.Notification(nil), n.sent...)
}

// settle writes events and waits until every one is past the stream's
// horizon (design 12 §2), so a pass is certain to read them.
func (f *fixture) settle(t *testing.T, actions ...string) {
	t.Helper()
	f.write(t, actions...)
	var newest int64
	require.NoError(t, f.owner.QueryRow(context.Background(), `SELECT max(id) FROM audit_events`).Scan(&newest))
	r := audit.NewReader(f.db.Pool)
	require.Eventually(t, func() bool {
		now, err := r.Now(context.Background())
		return err == nil && now.ID >= newest
	}, 30*time.Second, 50*time.Millisecond, "the events settled")
}

func (f *fixture) state(t *testing.T, id string) state.AuditSinkState {
	t.Helper()
	st, err := state.NewAuditSinks(f.db).Get(context.Background(), id, nil)
	require.NoError(t, err)
	return st
}

// lockState holds a sink's delivery-state row until the test ends or the
// returned func runs, so a write to it waits.
func (f *fixture) lockState(t *testing.T, id string) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := f.owner.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT 1 FROM audit_sink_state WHERE adapter_id = $1 FOR UPDATE`, id)
	require.NoError(t, err)
	release := func() { _ = tx.Rollback(ctx) }
	t.Cleanup(release)
	return release
}

// lockLog holds the audit log so nothing can read it until the returned func
// runs.
func (f *fixture) lockLog(t *testing.T) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := f.owner.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `LOCK TABLE audit_events IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	release := func() { _ = tx.Rollback(ctx) }
	t.Cleanup(release)
	return release
}

func shortCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func observed(level zapcore.Level) (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(level)
	return zap.New(core), logs
}

func closedDB(t *testing.T) *state.DB {
	t.Helper()
	db, _ := statetest.Connect(t)
	db.Close()
	return db
}

// TestR383_ASinkThatCannotBeBuiltIsShownAndRetriedAsFailing asserts R-383 for
// a sink whose stored settings no longer work: it is not skipped silently but
// recorded as failing with why, in words that hold no credential, and the
// status lists it as unusable beside the ones that work.
func TestR383_ASinkThatCannotBeBuiltIsShownAndRetriedAsFailing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	good := &probeSink{caps: api.AuditSinkCapabilities{Transport: "syslog", Endpoint: "logs.example.com:6514"}}
	refusesEnvelope := &probeSink{configErr: errs.New(errs.ValidInvalid, "The collector address is missing.")}
	refusesPlain := &probeSink{configErr: errors.New("port 99999 is out of range")}
	declared := &probeSink{}

	off := sinkRow("as_off", "good", `{}`)
	off.Enabled = false
	other := sinkRow("ntf_mail", "good", `{}`)
	other.Category = string(api.CategoryNotify)
	s := f.service([]auditstream.Declared{{ID: "as_declared", Adapter: declared}}, newOf(map[string]*probeSink{
		"good": good, "envelope": refusesEnvelope, "plain": refusesPlain,
	}))
	s.Configs = rows{list: []state.AdapterConfig{
		sinkRow("as_good", "good", `{"url":"syslog://logs.example.com:6514"}`),
		sinkRow("as_nokind", "splunk", `{}`),
		sinkRow("as_sealed", "good", `{}`),
		sinkRow("as_unreadable", "good", `["not an object"]`),
		sinkRow("as_envelope", "envelope", `{}`),
		sinkRow("as_plain", "plain", `{}`),
		sinkRow("as_declared", "good", `{}`), // the declared one wins
		off, other,
	}}
	s.Credentials = credStore{
		creds:   map[string]map[string]secret.Value{"as_good": {"token": secret.New("hec-token")}},
		failing: map[string]bool{"as_sealed": true},
	}

	_, err := s.Pass(ctx)
	require.NoError(t, err)

	assert.Equal(t, map[string]any{"token": "hec-token"}, good.config(t)["credentials"], "configured with its sealed credentials")
	assert.Equal(t, "syslog://logs.example.com:6514", good.config(t)["url"])

	unusable := map[string]string{
		"as_nokind":     `The audit sink "as_nokind" cannot be used. This build of Pando has no audit sink of kind splunk.`,
		"as_sealed":     `The audit sink "as_sealed" cannot be used. Its credentials could not be opened. Enter them again in its settings.`,
		"as_unreadable": `The audit sink "as_unreadable" cannot be used. Its settings could not be read.`,
		"as_envelope":   `The audit sink "as_envelope" cannot be used. The collector address is missing.`,
		"as_plain":      `The audit sink "as_plain" cannot be used. Its settings were refused: port 99999 is out of range`,
	}
	for id, msg := range unusable {
		st := f.state(t, id)
		assert.Equal(t, msg, st.LastError, id)
		assert.Equal(t, 1, st.Attempts, id)
		assert.NotNil(t, st.NextAttemptAt, "%s is retried on the schedule", id)
		assert.NotContains(t, st.LastError, "hec-token")
	}
	assert.Equal(t, len(unusable), f.count(t, "audit.sink.fail"), "each one's first failure is audited")

	statuses, err := s.Statuses(ctx)
	require.NoError(t, err)
	byID := map[string]auditstream.Status{}
	var ids []string
	for _, st := range statuses {
		byID[st.ID] = st
		ids = append(ids, st.ID)
	}
	assert.Equal(t, []string{"as_declared", "as_envelope", "as_good", "as_nokind", "as_off", "as_plain", "as_sealed", "as_unreadable"}, ids,
		"every audit sink, enabled or not, sorted, and nothing of another category")
	for id, msg := range unusable {
		assert.Equal(t, msg, byID[id].Unusable, id)
		assert.Equal(t, msg, byID[id].LastError, id)
	}
	assert.Empty(t, byID["as_good"].Unusable)
	assert.Equal(t, "syslog", byID["as_good"].Transport)
	assert.Equal(t, "logs.example.com:6514", byID["as_good"].Endpoint)
	assert.Equal(t, api.AuditFormatNative, byID["as_good"].Format, "no format is native")
	assert.True(t, byID["as_declared"].Declared)
	assert.Equal(t, "as_declared", byID["as_declared"].Name)
	assert.False(t, byID["as_off"].Enabled)
	assert.Equal(t, "as_off", byID["as_off"].AdapterID, "a sink never delivered to still names itself")
	assert.Equal(t, "Name of as_good", byID["as_good"].Name)
}

// TestR383_AFailureStoresTheMessageNotTheCodeAndAtMost500Characters asserts
// what a failed send records: an error envelope's message without its code,
// and any error cut to 500 characters.
func TestR383_AFailureStoresTheMessageNotTheCodeAndAtMost500Characters(t *testing.T) {
	f := newFixture(t)
	f.settle(t, "test.one")
	envelope := &probeSink{sendFn: func(context.Context, api.AuditBatch) error {
		return errs.Wrap(errs.AdapterFailed, "The collector refused the batch: 401 Unauthorized.", errors.New("tls internals"))
	}}
	long := &probeSink{sendFn: func(context.Context, api.AuditBatch) error {
		return errors.New(strings.Repeat("x", 600))
	}}
	s := f.service([]auditstream.Declared{{ID: "as_envelope", Adapter: envelope}, {ID: "as_long", Adapter: long}}, nil)
	s.Audit = nil // a service with nowhere to audit still records the failure

	_, err := s.Pass(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "The collector refused the batch: 401 Unauthorized.", f.state(t, "as_envelope").LastError)
	assert.Equal(t, strings.Repeat("x", 500), f.state(t, "as_long").LastError)
	assert.Zero(t, f.count(t, "audit.sink.fail"))
}

// TestR384_EventsAreEncodedInTheFormatTheSinkAsksFor asserts R-384's formats:
// a sink asking for a format with an encoder is sent encoded events, with the
// native ids and actions beside them; an encoder's failure, or a format this
// Pando cannot write, is a failed send that names the problem.
func TestR384_EventsAreEncodedInTheFormatTheSinkAsksFor(t *testing.T) {
	f := newFixture(t)
	f.settle(t, "test.one", "test.two")
	ocsf := &probeSink{caps: api.AuditSinkCapabilities{Format: api.AuditFormatOCSF}}
	broken := &probeSink{caps: api.AuditSinkCapabilities{Format: "broken"}}
	cef := &probeSink{caps: api.AuditSinkCapabilities{Format: "cef"}}
	s := f.service([]auditstream.Declared{
		{ID: "as_ocsf", Adapter: ocsf}, {ID: "as_broken", Adapter: broken}, {ID: "as_cef", Adapter: cef},
	}, nil)
	s.Encoders = map[string]audit.Encoder{
		api.AuditFormatOCSF: func(line json.RawMessage) (json.RawMessage, error) {
			var l audit.Line
			if err := json.Unmarshal(line, &l); err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"class_uid": 6003, "id": l.ID})
		},
		"broken": func(json.RawMessage) (json.RawMessage, error) {
			return nil, errors.New("this event has no OCSF class")
		},
	}

	_, err := s.Pass(context.Background())
	require.NoError(t, err)

	sent := ocsf.sent()
	require.NotEmpty(t, sent)
	b := sent[0]
	require.Len(t, b.Events, len(b.IDs))
	require.Len(t, b.Actions, len(b.IDs))
	for i, ev := range b.Events {
		var got map[string]any
		require.NoError(t, json.Unmarshal(ev, &got))
		assert.EqualValues(t, 6003, got["class_uid"])
		assert.EqualValues(t, b.IDs[i], got["id"], "ids line up with the encoded events")
	}
	assert.Contains(t, b.Actions, "test.one", "actions are read from the native line")

	assert.Empty(t, broken.sent())
	assert.Equal(t, "this event has no OCSF class", f.state(t, "as_broken").LastError)
	assert.Empty(t, cef.sent())
	assert.Equal(t, `The audit sink asks for events as "cef", which this Pando cannot write. Valid answers: native, ocsf.`,
		f.state(t, "as_cef").LastError)
}

// TestR382_ASinkThatStartsNowIsSentOnlyWhatFollows asserts R-382's start: now:
// a sink that asks to start now is positioned after the newest event when it
// is first seen.
func TestR382_ASinkThatStartsNowIsSentOnlyWhatFollows(t *testing.T) {
	f := newFixture(t)
	f.settle(t, "test.before")
	sk := &probeSink{caps: api.AuditSinkCapabilities{StartAtNow: true}}
	s := f.service([]auditstream.Declared{{ID: "as_now", Adapter: sk}}, nil)
	_, err := s.Pass(context.Background())
	require.NoError(t, err)
	assert.True(t, f.state(t, "as_now").HasCursor, "positioned when first seen")

	f.write(t, "test.after")
	drain(t, s)
	assert.Equal(t, []string{"test.after"}, sk.actions())
}

// TestR384_ASinkWhoseFilterDropsEverythingStillMovesOn asserts R-384's cursor
// rule at the sink: events a sink's filter drops are passed over, recorded as
// its position, and never counted as delivered.
func TestR384_ASinkWhoseFilterDropsEverythingStillMovesOn(t *testing.T) {
	f := newFixture(t)
	f.settle(t, "test.one", "test.two")
	sk := &probeSink{caps: api.AuditSinkCapabilities{Actions: []string{"nothing."}}}
	s := f.service([]auditstream.Declared{{ID: "as_none", Adapter: sk}}, nil)

	_, err := s.Pass(context.Background())
	require.NoError(t, err)
	st := f.state(t, "as_none")
	assert.True(t, st.HasCursor, "the cursor moved past what the filter dropped")
	assert.Zero(t, st.DeliveredCount)
	assert.Nil(t, st.DeliveredAt)
	assert.Empty(t, sk.sent())
}

// TestR383_OneSinksBookkeepingFailingDoesNotStopTheOthers asserts a sink whose
// state cannot be recorded is logged and passed over, and the rest are still
// delivered to in the same pass.
func TestR383_OneSinksBookkeepingFailingDoesNotStopTheOthers(t *testing.T) {
	f := newFixture(t)
	f.settle(t, "test.one")
	bad, good := &probeSink{}, &probeSink{}
	logger, logs := observed(zapcore.ErrorLevel)
	// Postgres refuses a NUL in text, so this sink's state can never be read.
	s := f.service([]auditstream.Declared{{ID: "as_\x00bad", Adapter: bad}, {ID: "as_good", Adapter: good}}, nil)
	s.Logger = logger

	_, err := s.Pass(context.Background())
	require.NoError(t, err)
	assert.Empty(t, bad.sent())
	assert.Contains(t, good.actions(), "test.one", "the next sink was still sent its events")
	failed := logs.FilterMessage("audit sink pass failed").All()
	require.Len(t, failed, 1)
	assert.Equal(t, "as_\x00bad", failed[0].ContextMap()["audit_sink"])

	_, err = s.Holding(context.Background(), time.Now().AddDate(0, -1, 0), time.Now())
	require.Error(t, err, "a hold that cannot be read is not reported as no hold")
}

// TestR383_BookkeepingThatCannotFinishIsAnErrorNotADelivery asserts that when
// a sink's state cannot be written in time — a delivery, a skip, a failure, a
// resume — the pass reports it as that sink's error, logged with the sink,
// rather than carrying on as if it had been recorded. (Whether the write that
// timed out lands later is the driver's business: pgx stops waiting but does
// not cancel the statement, so this does not assert the state afterwards.)
func TestR383_BookkeepingThatCannotFinishIsAnErrorNotADelivery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.settle(t, "test.one")
	sinks := state.NewAuditSinks(f.db)

	delivers := &probeSink{}
	skips := &probeSink{caps: api.AuditSinkCapabilities{Actions: []string{"nothing."}}}
	fails := &probeSink{sendFn: func(context.Context, api.AuditBatch) error { return errors.New("503") }}
	resumes := &probeSink{}
	for _, id := range []string{"as_delivers", "as_skips", "as_fails", "as_resumes"} {
		_, err := sinks.Get(ctx, id, nil)
		require.NoError(t, err)
	}
	_, err := sinks.Disable(ctx, "as_resumes", "off", f.clock.Now().Add(-time.Hour))
	require.NoError(t, err)

	for id, sk := range map[string]*probeSink{"as_delivers": delivers, "as_skips": skips, "as_fails": fails, "as_resumes": resumes} {
		release := f.lockState(t, id)
		logger, logs := observed(zapcore.ErrorLevel)
		var s *auditstream.Service
		if id == "as_resumes" {
			// A sink with a row, turned back on: it resumes on the next pass.
			s = f.service(nil, newOf(map[string]*probeSink{"probe": sk}))
			s.Configs = rows{list: []state.AdapterConfig{sinkRow(id, "probe", `{}`)}}
		} else {
			s = f.service([]auditstream.Declared{{ID: id, Adapter: sk}}, nil)
		}
		s.Logger = logger

		_, err := s.Pass(shortCtx(t))
		require.NoError(t, err, id)
		failed := logs.FilterMessage("audit sink pass failed").All()
		release()
		require.Len(t, failed, 1, id)
		assert.Equal(t, id, failed[0].ContextMap()["audit_sink"])
		assert.Contains(t, failed[0].ContextMap()["error"], "audit sink", id)
	}
	assert.NotEmpty(t, delivers.sent(), "the batch was sent; only its record failed")
	assert.Empty(t, resumes.sent(), "a sink whose resume failed is not sent anything")
}

// TestR383_ASendCutShortByShutdownIsNotAFailure asserts a send that ends
// because Pando is stopping is not counted against the sink.
func TestR383_ASendCutShortByShutdownIsNotAFailure(t *testing.T) {
	f := newFixture(t)
	f.settle(t, "test.one")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sk := &probeSink{sendFn: func(sendCtx context.Context, _ api.AuditBatch) error {
		cancel()
		<-sendCtx.Done()
		return sendCtx.Err()
	}}
	logger, logs := observed(zapcore.ErrorLevel)
	s := f.service([]auditstream.Declared{{ID: "as_stop", Adapter: sk}}, nil)
	s.Logger = logger

	_, err := s.Pass(ctx)
	require.NoError(t, err)
	st := f.state(t, "as_stop")
	assert.Zero(t, st.Attempts)
	assert.Empty(t, st.LastError)
	assert.Zero(t, f.count(t, "audit.sink.fail"))
	require.Equal(t, 1, logs.FilterMessage("audit sink pass failed").Len())
	assert.ErrorIs(t, logs.All()[0].Context[1].Interface.(error), context.Canceled)
}

// TestR383_AStreamThatCannotBeReadIsLoggedNotRecordedAgainstTheSink asserts a
// read of the log failing — Pando's problem, not the destination's — is
// logged and not counted as the sink failing, whether it fails reading where
// to start or reading what to send.
func TestR383_AStreamThatCannotBeReadIsLoggedNotRecordedAgainstTheSink(t *testing.T) {
	f := newFixture(t)
	logger, logs := observed(zapcore.ErrorLevel)
	s := f.service([]auditstream.Declared{
		{ID: "as_now", Adapter: &probeSink{caps: api.AuditSinkCapabilities{StartAtNow: true}}},
		{ID: "as_old", Adapter: &probeSink{}},
	}, nil)
	s.Reader = audit.NewReader(closedDB(t).Pool)
	s.Logger = logger

	_, err := s.Pass(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, logs.FilterMessage("audit sink pass failed").Len())
	assert.Zero(t, f.state(t, "as_old").Attempts)
}

// TestR383_ATurnedOffSinkTellsEveryoneWhoMayExportTheLog asserts R-383's
// notice: when Pando turns a sink off, everyone holding install.audit.export
// is told which sink, why, and what to do; nobody is told when nobody holds
// it or the holders cannot be read.
func TestR383_ATurnedOffSinkTellsEveryoneWhoMayExportTheLog(t *testing.T) {
	for _, tc := range []struct {
		name    string
		holders holders
		want    []api.Recipient
	}{
		{"holders", holders{people: []string{"usr_ada", "usr_bob"}}, []api.Recipient{{UserID: "usr_ada"}, {UserID: "usr_bob"}}},
		{"nobody", holders{}, nil},
		{"unreadable", holders{err: errors.New("database gone")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			n := &notifier{}
			// A sink that cannot be built fails without reading the log.
			s := f.service(nil, newOf(nil))
			s.Configs = rows{list: []state.AdapterConfig{sinkRow("as_dead", "gone", `{}`)}}
			s.Holders, s.Notifier = tc.holders, n

			_, err := s.Pass(ctx)
			require.NoError(t, err)
			for i := 0; i < 4; i++ {
				f.clock.Advance(7 * time.Hour)
				_, err = s.Pass(ctx)
				require.NoError(t, err)
			}
			st := f.state(t, "as_dead")
			require.NotNil(t, st.DisabledAt, "turned off after a day and five attempts")
			assert.Contains(t, st.DisabledReason, "5 attempts in a row")
			assert.Contains(t, st.DisabledReason, "Fix the destination, then turn the adapter back on")
			assert.Equal(t, 1, f.count(t, "audit.sink.disable"))

			got := n.all()
			if tc.want == nil {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			assert.Equal(t, api.NotifyAuditSinkDisabled, got[0].Kind)
			assert.Equal(t, tc.want, got[0].Recipients)
			assert.Equal(t, "Pando turned off the audit sink Name of as_dead", got[0].Subject)
			assert.Equal(t, st.DisabledReason, got[0].Body)
		})
	}
}

// TestR386_ASinkTurnedOffDoesNotHoldArchival asserts R-386: a declared sink
// Pando turned off is not asked to hold a month, and holds failing to be read
// are an error rather than no hold.
func TestR386_ASinkTurnedOffDoesNotHoldArchival(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.write(t, "test.one")
	lo, hi := time.Now().AddDate(0, -1, 0), time.Now().AddDate(0, 1, 0)

	s := f.service([]auditstream.Declared{{ID: "as_off", Adapter: &probeSink{}}, {ID: "as_on", Adapter: &probeSink{}}}, nil)
	_, err := state.NewAuditSinks(f.db).Get(ctx, "as_off", nil)
	require.NoError(t, err)
	_, err = state.NewAuditSinks(f.db).Disable(ctx, "as_off", "off", f.clock.Now())
	require.NoError(t, err)

	held, err := s.Holding(ctx, lo, hi)
	require.NoError(t, err)
	assert.Equal(t, []string{"as_on"}, held)

	release := f.lockLog(t)
	_, err = s.Holding(shortCtx(t), lo, hi)
	require.Error(t, err, "a hold that cannot be read is an error")
	release()

	s.Configs = rows{err: errors.New("adapters unreadable")}
	_, err = s.Holding(ctx, lo, hi)
	require.EqualError(t, err, "adapters unreadable")
}

// TestR383_AStatusThatCannotBeReadIsAnError asserts Statuses reports a failed
// read rather than a partial list.
func TestR383_AStatusThatCannotBeReadIsAnError(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.service([]auditstream.Declared{{ID: "as_x", Adapter: &probeSink{}}}, nil)
	_, err := state.NewAuditSinks(f.db).Get(ctx, "as_x", nil)
	require.NoError(t, err)

	_, err = state.NewAuditSinks(f.db).Get(ctx, "as_row", nil)
	require.NoError(t, err)
	release := f.lockLog(t)
	_, err = s.Statuses(shortCtx(t))
	require.Error(t, err, "the backlog cannot be counted")
	s.Declared = nil
	s.Configs = rows{list: []state.AdapterConfig{sinkRow("as_row", "probe", `{}`)}}
	s.New = newOf(map[string]*probeSink{"probe": {}})
	_, err = s.Statuses(shortCtx(t))
	require.Error(t, err, "nor a stored sink's")
	release()

	s.Configs = rows{err: errors.New("adapters unreadable")}
	_, err = s.Statuses(ctx)
	require.EqualError(t, err, "adapters unreadable")
	_, err = s.Pass(ctx)
	require.EqualError(t, err, "adapters unreadable")

	s.States = state.NewAuditSinks(closedDB(t))
	_, err = s.Statuses(ctx)
	require.Error(t, err)

	none := &auditstream.Service{Declared: []auditstream.Declared{{ID: "as_x", Adapter: &probeSink{}}}}
	statuses, err := none.Statuses(ctx)
	require.NoError(t, err, "no state store, no state")
	require.Len(t, statuses, 1)
	assert.Zero(t, statuses[0].Backlog)
}

// TestR383_ABacklogIsCountedUpToItsCap asserts design 12 §5.2's bound: a
// backlog of 100,000 or more is shown as capped, not counted to the end.
func TestR383_ABacklogIsCountedUpToItsCap(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.owner.Exec(ctx, `
		INSERT INTO audit_events (principal_kind, principal_id, action)
		SELECT 'system', 'system', 'test.bulk' FROM generate_series(1, 100001)`)
	require.NoError(t, err)
	s := f.service([]auditstream.Declared{{ID: "as_behind", Adapter: &probeSink{}}}, nil)
	_, err = state.NewAuditSinks(f.db).Get(ctx, "as_behind", nil)
	require.NoError(t, err)

	statuses, err := s.Statuses(ctx)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, 100_000, statuses[0].Backlog)
	assert.True(t, statuses[0].BacklogCapped)
}

// TestR383_RunPassesBackToBackWhileBehindAndStopsWithItsContext asserts the
// loop: while a sink is behind, passes run without waiting for the interval;
// when its context ends it returns.
func TestR383_RunPassesBackToBackWhileBehindAndStopsWithItsContext(t *testing.T) {
	f := newFixture(t)
	f.settle(t, "test.1", "test.2", "test.3", "test.4", "test.5")
	sk := &probeSink{caps: api.AuditSinkCapabilities{MaxBatch: 1, Actions: []string{"test."}}}
	s := f.service([]auditstream.Declared{{ID: "as_run", Adapter: sk}}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	// The fake clock never fires the interval, so all five arrive only if
	// passes ran back to back.
	require.Eventually(t, func() bool { return len(sk.actions()) == 5 }, 30*time.Second, 50*time.Millisecond)
	for _, b := range sk.sent() {
		assert.Len(t, b.Events, 1, "batches no larger than the sink asks for")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	assert.Equal(t, []string{"test.1", "test.2", "test.3", "test.4", "test.5"}, sk.actions())
}

// TestR383_RunLogsAPassThatFailsAndTriesAgain asserts a pass that cannot list
// the sinks is logged and the loop carries on, waiting the interval.
func TestR383_RunLogsAPassThatFailsAndTriesAgain(t *testing.T) {
	f := newFixture(t)
	logger, logs := observed(zapcore.ErrorLevel)
	s := f.service(nil, nil)
	s.Configs = rows{err: errors.New("adapters unreadable")}
	s.Logger = logger
	s.Interval = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	require.Eventually(t, func() bool { return logs.FilterMessage("audit stream pass did not finish").Len() == 1 }, 10*time.Second, 10*time.Millisecond)
	assert.Equal(t, 1, logs.FilterMessage("audit stream pass did not finish").Len(), "not again before the interval")
	require.Eventually(t, func() bool {
		f.clock.Advance(time.Minute) // until Run is waiting on it
		return logs.FilterMessage("audit stream pass did not finish").Len() >= 2
	}, 10*time.Second, 10*time.Millisecond, "tried again after the interval")
	cancel()
	<-done
}

// TestR383_RunWithAnEndedContextReturnsAtOnce asserts a loop started as Pando
// stops returns after one pass that records nothing. It runs with no clock and
// no logger, which default.
func TestR383_RunWithAnEndedContextReturnsAtOnce(t *testing.T) {
	f := newFixture(t)
	s := &auditstream.Service{
		Declared: []auditstream.Declared{{ID: "as_x", Adapter: &probeSink{}}},
		States:   state.NewAuditSinks(f.db),
		Reader:   audit.NewReader(f.db.Pool),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	list, err := state.NewAuditSinks(f.db).List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list, "nothing recorded")
}

// TestR383_ADeclaredSinkPandoTurnedOffResumesAfterARestart asserts R-383 for a
// sink in the configuration file, which has no row to turn back on: it stays
// off in the process that turned it off, and a process started after resumes
// it from its cursor.
func TestR383_ADeclaredSinkPandoTurnedOffResumesAfterARestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.settle(t, "test.one")
	sk := &probeSink{}
	_, err := state.NewAuditSinks(f.db).Get(ctx, "as_cfg", nil)
	require.NoError(t, err)
	_, err = state.NewAuditSinks(f.db).Disable(ctx, "as_cfg", "it kept failing", f.clock.Now())
	require.NoError(t, err)

	s := f.service([]auditstream.Declared{{ID: "as_cfg", Adapter: sk}}, nil)
	_, err = s.Pass(ctx)
	require.NoError(t, err)
	assert.Empty(t, sk.sent(), "still off in this process")
	assert.NotNil(t, f.state(t, "as_cfg").DisabledAt)

	f.clock.Advance(time.Minute)
	restarted := f.service([]auditstream.Declared{{ID: "as_cfg", Adapter: sk}}, nil)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { restarted.Run(runCtx); close(done) }()
	require.Eventually(t, func() bool { return len(sk.actions()) > 0 }, 30*time.Second, 50*time.Millisecond)
	cancel()
	<-done
	st := f.state(t, "as_cfg")
	assert.Nil(t, st.DisabledAt, "resumed after the restart")
	assert.Empty(t, st.DisabledReason)
	assert.Contains(t, sk.actions(), "test.one")
}

// TestR386_ASinkTurnedBackOnRecordsTheMonthsArchivedWhileItWasOff asserts
// R-386's gap: a sink Pando turned off stops holding archival, so a month it
// was never sent is archived and removed; turned back on, it resumes and the
// range it missed is recorded and logged, with that month's archive as the
// backfill.
func TestR386_ASinkTurnedBackOnRecordsTheMonthsArchivedWhileItWasOff(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, -6, 0)
	at := time.Date(old.Year(), old.Month(), 3, 10, 0, 0, 0, time.UTC)
	_, err := f.owner.Exec(ctx, `INSERT INTO audit_events (occurred_at, principal_kind, principal_id, action)
		VALUES ($1, 'system', 'system', 'test.old')`, at)
	require.NoError(t, err)

	sk := &probeSink{}
	adapters := state.NewAdapters(f.db)
	require.NoError(t, adapters.Upsert(ctx, state.AdapterConfig{
		ID: "as_gap", Category: "audit_sink", Kind: "probe", Name: "SIEM", Config: json.RawMessage(`{}`), Enabled: true,
	}))
	logger, logs := observed(zapcore.WarnLevel)
	s := f.service(nil, newOf(map[string]*probeSink{"probe": sk}))
	s.Logger = logger
	sinks := state.NewAuditSinks(f.db)
	_, err = sinks.Get(ctx, "as_gap", nil)
	require.NoError(t, err)
	changed, err := sinks.Disable(ctx, "as_gap", "it kept failing", f.clock.Now())
	require.NoError(t, err)
	require.True(t, changed)

	archiver := &audit.Archiver{
		Pool:      statetest.Archiver(t, f.ownerURL),
		Stores:    audit.Stores{Kept: newMemStore()},
		Retention: func(context.Context) (audit.Retention, error) { return audit.Retention{}, nil },
		Clock:     clock.System{},
		Logger:    zap.NewNop(),
		Holds:     s.Holding,
	}
	recs, err := archiver.Pass(ctx)
	require.NoError(t, err)
	require.Len(t, recs, 1, "a sink that is off does not hold the month")
	assert.Zero(t, f.count(t, "test.old"), "and it left the live log")

	_, err = f.owner.Exec(ctx, `UPDATE adapter_configs SET enabled = true WHERE id = 'as_gap'`)
	require.NoError(t, err)
	_, err = s.Pass(ctx)
	require.NoError(t, err)

	st := f.state(t, "as_gap")
	assert.Nil(t, st.DisabledAt, "turned back on, it resumes")
	require.NotNil(t, st.GapFrom)
	require.NotNil(t, st.GapTo)
	assert.True(t, st.GapFrom.Equal(at), "the gap is the archived month's events: %v", st.GapFrom)
	assert.True(t, st.GapTo.Equal(at))
	assert.NotContains(t, sk.actions(), "test.old")
	warned := logs.FilterMessage("an audit sink that was turned off missed events the archiver has since removed").All()
	require.Len(t, warned, 1)
	assert.Equal(t, "as_gap", warned[0].ContextMap()["audit_sink"])
}

// TestR383_ARecoveryIsAuditedOnce asserts R-383's other edge: a failing sink
// that is sent a batch on a retry has its recovery audited, with how long and
// how often it failed, once.
func TestR383_ARecoveryIsAuditedOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.settle(t, "test.one")
	var down = true
	sk := &probeSink{sendFn: func(context.Context, api.AuditBatch) error {
		if down {
			return errors.New("the collector answered 503")
		}
		return nil
	}}
	s := f.service([]auditstream.Declared{{ID: "as_flaky", Adapter: sk}}, nil)

	_, err := s.Pass(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, f.state(t, "as_flaky").Attempts)

	down = false
	f.clock.Advance(2 * time.Minute)
	_, err = s.Pass(ctx)
	require.NoError(t, err)
	assert.Contains(t, sk.actions(), "test.one")
	st := f.state(t, "as_flaky")
	assert.Zero(t, st.Attempts)
	assert.Nil(t, st.FailingSince)

	_, err = s.Pass(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(t, "audit.sink.recover"))
	var detail map[string]any
	require.NoError(t, f.owner.QueryRow(ctx, `SELECT detail FROM audit_events WHERE action = 'audit.sink.recover'`).Scan(&detail))
	assert.EqualValues(t, 1, detail["attempts"])
	assert.NotEmpty(t, detail["failing_since"])
	assert.Equal(t, "as_flaky", detail["adapter"])
	assert.Equal(t, "probe", detail["kind"])
}

// sent and actions are what a probe was sent; only these integration tests
// read them.
func (p *probeSink) sent() []api.AuditBatch {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]api.AuditBatch(nil), p.batches...)
}

func (p *probeSink) actions() []string {
	var out []string
	for _, b := range p.sent() {
		out = append(out, b.Actions...)
	}
	return out
}
