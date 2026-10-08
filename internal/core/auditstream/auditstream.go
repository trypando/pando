// Package auditstream pushes the audit log to audit sinks as it is written
// (R-382, R-383, design 12 §5).
//
// It runs on the leader alone (R-256): one delivering replica, so a
// destination never receives two interleaved copies of the stream. Sinks are
// built from their rows on every pass, as core/imageregistry builds
// registries, so a rotated token is what the next pass sends with on
// whichever replica leads, with no restart.
//
// An adapter is handed encoded events and nothing else. This package reads
// the log through audit.Reader, which cannot write it (R-027), and records
// where each sink has got to in audit_sink_state.
package auditstream

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// ConfigStore lists stored adapters (state.Adapters).
type ConfigStore interface {
	List(ctx context.Context) ([]state.AdapterConfig, error)
}

// CredentialStore opens an adapter's sealed credentials
// (state.AdapterCredentials).
type CredentialStore interface {
	Resolve(ctx context.Context, adapterID string) (map[string]secret.Value, error)
}

// VerbHolders lists the people holding an install verb (state.AuthzStore).
type VerbHolders interface {
	InstallVerbHolders(ctx context.Context, verb authz.Verb) ([]string, error)
}

// Notifier sends Pando's own notifications.
type Notifier interface {
	Notify(ctx context.Context, n api.Notification) error
}

// Declared is an audit sink the startup configuration declares, already
// configured.
type Declared struct {
	ID      string
	Adapter api.AuditSinkAdapter
}

// Service delivers the audit log to every enabled audit sink.
type Service struct {
	Configs     ConfigStore
	Credentials CredentialStore
	Declared    []Declared

	// New returns an unconfigured adapter of a kind, or nil.
	New func(kind string) api.AuditSinkAdapter

	States *state.AuditSinks
	Reader *audit.Reader

	// Encoders turn native lines into a sink's format. A format with no
	// encoder — native — is sent as read.
	Encoders map[string]audit.Encoder

	// Audit writes the sink's own events: its first failure, its recovery,
	// and Pando turning it off (R-383).
	Audit func(ctx context.Context, e audit.Event)

	Holders  VerbHolders
	Notifier Notifier

	Clock    clock.Clock
	Logger   *zap.Logger
	Interval time.Duration

	// started is when this process began delivering: a sink declared in the
	// configuration file, which has no row to turn back on, resumes after a
	// restart.
	started time.Time
}

// DefaultInterval is how often a quiet pass runs. While any sink is behind,
// passes run back to back.
const DefaultInterval = 2 * time.Second

// retryAfter is design 11's schedule, reused (design 12 §5.2): the first
// attempt is immediate, and each failure waits the next step, then 12 hours
// for as long as it keeps failing.
var retryAfter = []time.Duration{
	time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour,
}

// Turned off after failing this long and this many times in a row (R-383).
const (
	disableAfter    = 24 * time.Hour
	disableAttempts = 5
)

// sendTimeout bounds one Send.
const sendTimeout = time.Minute

// backlogCap bounds the backlog count (design 12 §5.2).
const backlogCap = 100_000

// DefaultBatch is a batch's size when an adapter does not say.
const DefaultBatch = 500

// sink is one audit sink as a pass sees it.
type sink struct {
	id       string
	kind     string
	name     string
	declared bool
	adapter  api.AuditSinkAdapter
	// broken is why the sink could not be built from its row. It is
	// recorded as a failed send, so a sink with credentials that no longer
	// open is visible and retried, not silently skipped.
	broken error
}

func (s *Service) clock() clock.Clock {
	if s.Clock == nil {
		return clock.System{}
	}
	return s.Clock
}

func (s *Service) logger() *zap.Logger {
	if s.Logger == nil {
		return zap.NewNop()
	}
	return s.Logger
}

// sinks lists every enabled audit sink, declared ones first-class, sorted by
// ID.
func (s *Service) sinks(ctx context.Context) ([]sink, error) {
	var out []sink
	declared := map[string]bool{}
	for _, d := range s.Declared {
		declared[d.ID] = true
		out = append(out, sink{id: d.ID, kind: d.Adapter.Kind(), name: d.ID, declared: true, adapter: d.Adapter})
	}
	if s.Configs != nil {
		rows, err := s.Configs.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.Category != string(api.CategoryAuditSink) || !row.Enabled || declared[row.ID] {
				continue
			}
			a, err := s.build(ctx, row)
			out = append(out, sink{id: row.ID, kind: row.Kind, name: row.Name, adapter: a, broken: err})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// build configures a sink from its row and its sealed credentials. The
// adapter's own refusal is kept, never a credential (R-194).
func (s *Service) build(ctx context.Context, row state.AdapterConfig) (api.AuditSinkAdapter, error) {
	unusable := func(why string) error {
		return errs.Newf(errs.AdapterUnavailable, "The audit sink %q cannot be used. %s", row.ID, why).
			WithDetail("adapter", row.ID).
			WithRemedy("Change its settings under System → Adapters, or with pando adapter add.")
	}
	var a api.AuditSinkAdapter
	if s.New != nil {
		a = s.New(row.Kind)
	}
	if a == nil {
		return nil, unusable("This build of Pando has no audit sink of kind " + row.Kind + ".")
	}
	var creds map[string]secret.Value
	if s.Credentials != nil {
		var err error
		if creds, err = s.Credentials.Resolve(ctx, row.ID); err != nil {
			return nil, unusable("Its credentials could not be opened. Enter them again in its settings.")
		}
	}
	raw, err := withCredentials(row.Config, creds)
	if err != nil {
		return nil, unusable("Its settings could not be read.")
	}
	if err := a.Configure(ctx, raw); err != nil {
		if e := errs.As(err); e != nil {
			return nil, unusable(e.Message)
		}
		return nil, unusable("Its settings were refused: " + err.Error())
	}
	return a, nil
}

// Validate configures a sink as it would be saved — config and the
// credentials given, beside the ones already stored for id that are not
// replaced — and returns the adapter's refusal, so a sink Pando could not send
// with is refused when it is saved rather than at its first send. A
// credential sent empty removes the stored one, as saving does.
func (s *Service) Validate(ctx context.Context, id, kind string, config json.RawMessage, given map[string]secret.Value) error {
	if s == nil || s.New == nil {
		return nil
	}
	a := s.New(kind)
	if a == nil {
		return errs.Newf(errs.ValidInvalid, "This build of Pando has no audit sink of kind %q.", kind)
	}
	creds := map[string]secret.Value{}
	if s.Credentials != nil && id != "" {
		if stored, err := s.Credentials.Resolve(ctx, id); err == nil {
			for k, v := range stored {
				creds[k] = v
			}
		}
	}
	for k, v := range given {
		if v.IsZero() {
			delete(creds, k)
			continue
		}
		creds[k] = v
	}
	raw, err := withCredentials(config, creds)
	if err != nil {
		return errs.New(errs.ValidInvalid, "The audit sink's settings could not be read.")
	}
	if err := a.Configure(ctx, raw); err != nil {
		if errs.As(err) != nil {
			return err
		}
		return errs.New(errs.ValidInvalid, "The audit sink's settings were refused. "+err.Error())
	}
	return nil
}

// withCredentials puts credentials where an adapter's Configure reads them,
// as cmd/pando does for adapters built at startup (O-20).
func withCredentials(raw json.RawMessage, creds map[string]secret.Value) (json.RawMessage, error) {
	cfg := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	if len(creds) > 0 {
		plain := make(map[string]string, len(creds))
		for field, v := range creds {
			plain[field] = v.Reveal()
		}
		body, err := json.Marshal(plain)
		if err != nil {
			return nil, err
		}
		cfg["credentials"] = body
	}
	return json.Marshal(cfg)
}

// Run delivers until ctx ends: a pass every Interval while every sink is
// caught up, and back to back while any is behind.
func (s *Service) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	s.started = s.clock().Now()
	for {
		busy, err := s.Pass(ctx)
		if err != nil && ctx.Err() == nil {
			s.logger().Error("audit stream pass did not finish", zap.Error(err))
		}
		if ctx.Err() != nil {
			return
		}
		if busy && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.clock().After(interval):
		}
	}
}

// Pass sends one batch to every sink that is due. It reports whether any
// sink is still behind.
func (s *Service) Pass(ctx context.Context) (bool, error) {
	all, err := s.sinks(ctx)
	if err != nil {
		return false, err
	}
	busy := false
	for _, sk := range all {
		behind, err := s.deliver(ctx, sk)
		if err != nil {
			// One sink's bookkeeping failing is no reason to skip the rest.
			s.logger().Error("audit sink pass failed", zap.String("audit_sink", sk.id), zap.Error(err))
			continue
		}
		busy = busy || behind
	}
	return busy, nil
}

// ensure returns a sink's state, creating it where the sink starts: at the
// oldest event in the live log, or after the newest for one that starts now.
func (s *Service) ensure(ctx context.Context, sk sink) (state.AuditSinkState, error) {
	var start *state.AuditCursor
	if sk.adapter != nil && sk.adapter.AuditSinkCapabilities().StartAtNow {
		now, err := s.Reader.Now(ctx)
		if err != nil {
			return state.AuditSinkState{}, err
		}
		start = &state.AuditCursor{TxID: now.TxID, ID: now.ID}
	}
	return s.States.Get(ctx, sk.id, start)
}

// deliver sends one batch to one sink, if it is due, and records the
// outcome. It reports whether the sink is still behind.
func (s *Service) deliver(ctx context.Context, sk sink) (bool, error) {
	now := s.clock().Now()
	st, err := s.ensure(ctx, sk)
	if err != nil {
		return false, err
	}

	if st.DisabledAt != nil {
		// Its row is enabled again — sinks lists only enabled ones — so
		// somebody turned it back on. A declared sink has no row; it resumes
		// after a restart.
		if sk.declared && !st.DisabledAt.Before(s.started) {
			return false, nil
		}
		if st, err = s.States.Resume(ctx, sk.id, now); err != nil {
			return false, err
		}
		if st.GapTo != nil {
			s.logger().Warn("an audit sink that was turned off missed events the archiver has since removed",
				zap.String("audit_sink", sk.id), zap.Timep("gap_from", st.GapFrom), zap.Timep("gap_to", st.GapTo))
		}
	}
	if st.NextAttemptAt != nil && now.Before(*st.NextAttemptAt) {
		return false, nil
	}
	if sk.broken != nil {
		return false, s.failed(ctx, sk, st, sk.broken, now)
	}

	caps := sk.adapter.AuditSinkCapabilities()
	batch := caps.MaxBatch
	if batch <= 0 {
		batch = DefaultBatch
	}
	after := audit.Cursor{}
	if st.HasCursor {
		after = audit.Cursor{TxID: st.CursorTxID, ID: st.CursorID}
	}
	page, err := s.Reader.Stream(ctx, audit.StreamQuery{
		After: after, Limit: batch, Actions: caps.Actions, Exclude: caps.Exclude,
	})
	if err != nil {
		return false, err
	}
	if len(page.Events) == 0 {
		if page.Cursor != after {
			if err := s.States.Skipped(ctx, sk.id, page.Cursor.TxID, page.Cursor.ID, now); err != nil {
				return false, err
			}
		}
		return !page.CaughtUp, nil
	}

	b, err := s.encode(caps.Format, page)
	if err != nil {
		return false, s.failed(ctx, sk, st, err, now)
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	err = sk.adapter.Send(sendCtx, b)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, s.failed(ctx, sk, st, err, now)
	}

	wasFailing, err := s.States.Delivered(ctx, sk.id, page.Cursor.TxID, page.Cursor.ID, len(page.Events), s.clock().Now())
	if err != nil {
		return false, err
	}
	if wasFailing {
		s.audit(ctx, sk, "audit.sink.recover", map[string]any{"failing_since": st.FailingSince, "attempts": st.Attempts})
	}
	return !page.CaughtUp, nil
}

// encode turns a page into a batch in the sink's format.
func (s *Service) encode(format string, page audit.StreamPage) (api.AuditBatch, error) {
	if format == "" {
		format = api.AuditFormatNative
	}
	enc, known := s.Encoders[format]
	if !known && format != api.AuditFormatNative {
		return api.AuditBatch{}, errs.Newf(errs.AdapterFailed,
			"The audit sink asks for events as %q, which this Pando cannot write. Valid answers: native, ocsf.", format)
	}
	b := api.AuditBatch{IDs: page.IDs}
	for _, line := range page.Events {
		var head struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(line, &head)
		b.Actions = append(b.Actions, head.Action)
		out := line
		if enc != nil {
			var err error
			if out, err = enc(line); err != nil {
				return api.AuditBatch{}, err
			}
		}
		b.Events = append(b.Events, out)
	}
	return b, nil
}

// maxErrorText bounds what one failure stores and shows.
const maxErrorText = 500

// failed records a failed send, audits the first failure in a run, and turns
// the sink off once it has failed long enough (R-383).
func (s *Service) failed(ctx context.Context, sk sink, before state.AuditSinkState, cause error, now time.Time) error {
	msg := cause.Error()
	if e := errs.As(cause); e != nil {
		msg = e.Message
	}
	if len(msg) > maxErrorText {
		msg = msg[:maxErrorText]
	}
	step := min(before.Attempts, len(retryAfter)-1)
	st, err := s.States.Failed(ctx, sk.id, msg, now, now.Add(retryAfter[step]))
	if err != nil {
		return err
	}
	s.logger().Warn("audit sink send failed", zap.String("audit_sink", sk.id),
		zap.Int("attempts", st.Attempts), zap.String("error", msg))
	if before.FailingSince == nil {
		s.audit(ctx, sk, "audit.sink.fail", map[string]any{"error": msg})
	}
	if st.FailingSince != nil && now.Sub(*st.FailingSince) >= disableAfter && st.Attempts >= disableAttempts {
		s.disable(ctx, sk, *st.FailingSince, st.Attempts, msg)
	}
	return nil
}

// disable turns a sink off, once, and tells everyone who may send the audit
// log off the installation.
func (s *Service) disable(ctx context.Context, sk sink, since time.Time, attempts int, last string) {
	reason := "Every send since " + since.UTC().Format(time.RFC3339) + " failed, " +
		itoa(attempts) + " attempts in a row. The last error: " + last + " " +
		"Pando turned this audit sink off so the audit log is not held in the live log for it any longer. " +
		"Fix the destination, then turn the adapter back on; it resumes from the last event it was sent, " +
		"and any month removed while it was off is listed as a gap, with that month's archive as the backfill."
	changed, err := s.States.Disable(ctx, sk.id, reason, s.clock().Now())
	if err != nil || !changed {
		return
	}
	s.audit(ctx, sk, "audit.sink.disable", map[string]any{"reason": reason, "attempts": attempts})
	s.logger().Warn("turned off an audit sink that keeps failing", zap.String("audit_sink", sk.id), zap.Int("attempts", attempts))
	if s.Notifier == nil || s.Holders == nil {
		return
	}
	people, err := s.Holders.InstallVerbHolders(ctx, authz.InstallAuditExport)
	if err != nil || len(people) == 0 {
		return
	}
	recipients := make([]api.Recipient, 0, len(people))
	for _, p := range people {
		recipients = append(recipients, api.Recipient{UserID: p})
	}
	_ = s.Notifier.Notify(ctx, api.Notification{
		Kind:       api.NotifyAuditSinkDisabled,
		Recipients: recipients,
		Subject:    "Pando turned off the audit sink " + sk.name,
		Body:       reason,
	})
}

func (s *Service) audit(ctx context.Context, sk sink, action string, detail map[string]any) {
	if s.Audit == nil {
		return
	}
	detail["adapter"] = sk.id
	detail["kind"] = sk.kind
	s.Audit(ctx, audit.Event{
		PrincipalKind: audit.KindSystem, PrincipalID: authz.System().ID,
		Action: action, TargetKind: "adapter", TargetID: sk.id, Detail: detail,
	})
}

// Holding names the enabled sinks that have not been sent every event in
// [lo, hi): the archiver keeps such a month in the live log (R-386). A sink
// Pando turned off does not hold.
func (s *Service) Holding(ctx context.Context, lo, hi time.Time) ([]string, error) {
	all, err := s.sinks(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sk := range all {
		st, err := s.ensure(ctx, sk)
		if err != nil {
			return nil, err
		}
		if st.DisabledAt != nil {
			continue
		}
		held, err := s.States.Holds(ctx, st, lo, hi)
		if err != nil {
			return nil, err
		}
		if held {
			out = append(out, sk.id)
		}
	}
	return out, nil
}

// Status is one sink as the console, API, CLI and MCP show it (R-383, R-385).
type Status struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Declared bool   `json:"declared,omitempty"`

	Transport string   `json:"transport,omitempty"`
	Endpoint  string   `json:"endpoint,omitempty"`
	Format    string   `json:"format,omitempty"`
	Actions   []string `json:"actions,omitempty"`
	Exclude   []string `json:"exclude,omitempty"`

	// Backlog is how many events are past the cursor, at most BacklogCap;
	// BacklogCapped says there are at least that many.
	Backlog       int  `json:"backlog"`
	BacklogCapped bool `json:"backlog_capped,omitempty"`

	// Unusable is why the sink cannot be built from its settings.
	Unusable string `json:"unusable,omitempty"`

	state.AuditSinkState
}

// Statuses lists every audit sink, enabled or not, with where it has got to.
func (s *Service) Statuses(ctx context.Context) ([]Status, error) {
	states := map[string]state.AuditSinkState{}
	if s.States != nil {
		list, err := s.States.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, st := range list {
			states[st.AdapterID] = st
		}
	}
	var out []Status
	add := func(id, kind, name string, enabled, declared bool, a api.AuditSinkAdapter, broken error) error {
		st := Status{ID: id, Kind: kind, Name: name, Enabled: enabled, Declared: declared}
		if a != nil {
			c := a.AuditSinkCapabilities()
			st.Transport, st.Endpoint, st.Format, st.Actions, st.Exclude = c.Transport, c.Endpoint, c.Format, c.Actions, c.Exclude
			if st.Format == "" {
				st.Format = api.AuditFormatNative
			}
		}
		if broken != nil {
			st.Unusable = broken.Error()
			if e := errs.As(broken); e != nil {
				st.Unusable = e.Message
			}
		}
		if have, ok := states[id]; ok {
			st.AuditSinkState = have
			n, err := s.States.Backlog(ctx, have, backlogCap)
			if err != nil {
				return err
			}
			st.Backlog, st.BacklogCapped = n, n >= backlogCap
		}
		st.AdapterID = id
		out = append(out, st)
		return nil
	}
	declared := map[string]bool{}
	for _, d := range s.Declared {
		declared[d.ID] = true
		if err := add(d.ID, d.Adapter.Kind(), d.ID, true, true, d.Adapter, nil); err != nil {
			return nil, err
		}
	}
	if s.Configs != nil {
		rows, err := s.Configs.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.Category != string(api.CategoryAuditSink) || declared[row.ID] {
				continue
			}
			a, berr := s.build(ctx, row)
			if err := add(row.ID, row.Kind, row.Name, row.Enabled, false, a, berr); err != nil {
				return nil, err
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Describe is the one-line disclosure the console shows beside the audit log
// for an enabled sink (R-385).
func Describe(st Status) string {
	what := "Every audit event"
	if len(st.Actions) > 0 {
		what = "Audit events matching " + strings.Join(st.Actions, ", ")
	}
	if len(st.Exclude) > 0 {
		what += " except " + strings.Join(st.Exclude, ", ")
	}
	via := map[string]string{"syslog": "over syslog", "https": "over HTTPS"}[st.Transport]
	return strings.TrimSpace(what + " is sent to " + st.Endpoint + " " + via + ".")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
