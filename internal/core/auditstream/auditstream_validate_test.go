package auditstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/auditstream"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// probeSink is an audit sink whose every answer a test sets, and which
// records what it was configured with and sent.
type probeSink struct {
	mu        sync.Mutex
	caps      api.AuditSinkCapabilities
	configErr error
	sendFn    func(ctx context.Context, b api.AuditBatch) error

	configured json.RawMessage
	batches    []api.AuditBatch
}

func (p *probeSink) Kind() string                      { return "probe" }
func (p *probeSink) Category() api.Category            { return api.CategoryAuditSink }
func (p *probeSink) HealthCheck(context.Context) error { return nil }
func (p *probeSink) Configure(_ context.Context, raw json.RawMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configured = raw
	return p.configErr
}
func (p *probeSink) AuditSinkCapabilities() api.AuditSinkCapabilities { return p.caps }
func (p *probeSink) Send(ctx context.Context, b api.AuditBatch) error {
	if p.sendFn != nil {
		if err := p.sendFn(ctx, b); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.batches = append(p.batches, b)
	return nil
}

func (p *probeSink) config(t *testing.T) map[string]any {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var m map[string]any
	require.NoError(t, json.Unmarshal(p.configured, &m))
	return m
}

// credStore is a CredentialStore in memory; an adapter in failing cannot be
// opened.
type credStore struct {
	creds   map[string]map[string]secret.Value
	failing map[string]bool
}

func (c credStore) Resolve(_ context.Context, id string) (map[string]secret.Value, error) {
	if c.failing[id] {
		return nil, errors.New("the sealing key does not open this credential")
	}
	return c.creds[id], nil
}

func newOf(sinks map[string]*probeSink) func(string) api.AuditSinkAdapter {
	return func(kind string) api.AuditSinkAdapter {
		if p, ok := sinks[kind]; ok {
			return p
		}
		return nil
	}
}

// TestR382_ValidateIsANoOpWithoutAnyAdapters asserts a Pando built with no
// audit sink adapters validates nothing rather than refusing everything.
func TestR382_ValidateIsANoOpWithoutAnyAdapters(t *testing.T) {
	var none *auditstream.Service
	assert.NoError(t, none.Validate(context.Background(), "as_x", "splunk", nil, nil))
	assert.NoError(t, (&auditstream.Service{}).Validate(context.Background(), "as_x", "splunk", nil, nil))
}

// TestR382_ValidateRefusesAKindThisBuildDoesNotHave asserts an unknown kind is
// refused as invalid input, naming the kind.
func TestR382_ValidateRefusesAKindThisBuildDoesNotHave(t *testing.T) {
	s := &auditstream.Service{New: newOf(nil)}
	err := s.Validate(context.Background(), "", "splunk", nil, nil)
	e := errs.As(err)
	require.NotNil(t, e)
	assert.Equal(t, errs.ValidInvalid, e.Code)
	assert.Equal(t, `This build of Pando has no audit sink of kind "splunk".`, e.Message)
}

// TestR382_ValidateConfiguresWithStoredCredentialsUnlessReplaced asserts a
// sink is validated as it would be saved: its settings, the credentials given,
// and the stored ones not replaced; a credential given empty removes the
// stored one. Stored credentials are read only for an existing sink.
func TestR382_ValidateConfiguresWithStoredCredentialsUnlessReplaced(t *testing.T) {
	ctx := context.Background()
	p := &probeSink{}
	s := &auditstream.Service{
		New: newOf(map[string]*probeSink{"probe": p}),
		Credentials: credStore{creds: map[string]map[string]secret.Value{
			"as_x": {"token": secret.New("stored-token"), "password": secret.New("stored-password"), "key": secret.New("stored-key")},
		}},
	}

	require.NoError(t, s.Validate(ctx, "as_x", "probe", json.RawMessage(`{"url":"https://siem.example.com"}`),
		map[string]secret.Value{"token": secret.New("new-token"), "password": {}}))
	cfg := p.config(t)
	assert.Equal(t, "https://siem.example.com", cfg["url"], "the settings are kept")
	assert.Equal(t, map[string]any{"token": "new-token", "key": "stored-key"}, cfg["credentials"],
		"given replaces stored, empty removes it, the rest are kept")

	require.NoError(t, s.Validate(ctx, "", "probe", nil, map[string]secret.Value{"token": secret.New("t")}))
	assert.Equal(t, map[string]any{"token": "t"}, p.config(t)["credentials"], "a new sink has nothing stored")

	require.NoError(t, s.Validate(ctx, "as_new", "probe", nil, nil))
	assert.NotContains(t, p.config(t), "credentials", "no credentials, none passed")
}

// TestR382_ValidateIgnoresStoredCredentialsThatCannotBeOpened asserts stored
// credentials that do not open are left out rather than failing a save that
// replaces them.
func TestR382_ValidateIgnoresStoredCredentialsThatCannotBeOpened(t *testing.T) {
	p := &probeSink{}
	s := &auditstream.Service{
		New:         newOf(map[string]*probeSink{"probe": p}),
		Credentials: credStore{failing: map[string]bool{"as_x": true}},
	}
	require.NoError(t, s.Validate(context.Background(), "as_x", "probe", nil, map[string]secret.Value{"token": secret.New("fresh")}))
	assert.Equal(t, map[string]any{"token": "fresh"}, p.config(t)["credentials"])
}

// TestR382_ValidateReturnsTheAdaptersRefusal asserts the adapter's refusal is
// what the save returns: its own error envelope as is, a plain error as
// invalid input that quotes it, and settings that are not an object as
// unreadable.
func TestR382_ValidateReturnsTheAdaptersRefusal(t *testing.T) {
	ctx := context.Background()
	refusal := errs.New(errs.ValidInvalid, "The collector address is missing. Valid answer: a URL such as https://siem.example.com.")
	p := &probeSink{configErr: refusal}
	s := &auditstream.Service{New: newOf(map[string]*probeSink{"probe": p})}

	err := s.Validate(ctx, "", "probe", json.RawMessage(`{}`), nil)
	assert.Same(t, refusal, errs.As(err))

	p.configErr = errors.New("port 99999 is out of range")
	e := errs.As(s.Validate(ctx, "", "probe", json.RawMessage(`{}`), nil))
	require.NotNil(t, e)
	assert.Equal(t, errs.ValidInvalid, e.Code)
	assert.Equal(t, "The audit sink's settings were refused. port 99999 is out of range", e.Message)

	p.configErr = nil
	e = errs.As(s.Validate(ctx, "", "probe", json.RawMessage(`["not", "an", "object"]`), nil))
	require.NotNil(t, e)
	assert.Equal(t, errs.ValidInvalid, e.Code)
	assert.Equal(t, "The audit sink's settings could not be read.", e.Message)
}

// TestR385_DescribeNamesWhatIsSentWhereAndHow asserts R-385's disclosure line.
func TestR385_DescribeNamesWhatIsSentWhereAndHow(t *testing.T) {
	assert.Equal(t, "Every audit event is sent to logs.example.com:6514 over syslog.",
		auditstream.Describe(auditstream.Status{Transport: "syslog", Endpoint: "logs.example.com:6514"}))
	assert.Equal(t, "Audit events matching app., grant. except app.use are sent to siem.example.com:443 over HTTPS.",
		auditstream.Describe(auditstream.Status{Transport: "https", Endpoint: "siem.example.com:443",
			Actions: []string{"app.", "grant."}, Exclude: []string{"app.use"}}))
}
