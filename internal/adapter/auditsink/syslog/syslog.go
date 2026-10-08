// Package syslog is the audit sink adapter for a syslog collector (R-382,
// design 12 §5.1): RFC 5424 messages over TCP, octet-counted as RFC 5425 and
// RFC 6587 frame them, with TLS unless the operator turns it off.
//
// Each event is one message. Its MSG is the event as core encoded it, its
// MSGID the action, and its structured data the event's id and outcome, so a
// collector can deduplicate a redelivered event (delivery is at least once).
package syslog

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/auditsink/sinkkit"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "syslog"

// DefaultMaxBatch is how many events one Send carries unless max_batch says.
const DefaultMaxBatch = 200

// Default ports: syslog over TLS (RFC 5425) and over plain TCP.
const (
	defaultTLSPort   = "6514"
	defaultPlainPort = "514"
)

// enterpriseID is the private enterprise number structured data is named
// under, as RFC 5424 §7.2.2 requires of an SD-ID that is not IANA's own.
const enterpriseID = "pando@32473"

// RFC 5424 severities this adapter sends.
const (
	severityWarning       = 4
	severityInformational = 6
)

// facilities are the codes RFC 5424 §6.2.1 names that suit an audit log. The
// default is authpriv: security and authorization messages that a collector
// keeps out of the general log, which is what an audit trail is.
var facilities = map[string]int{
	"auth": 4, "authpriv": 10, "daemon": 3, "user": 1,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19,
	"local4": 20, "local5": 21, "local6": 22, "local7": 23,
}

// Config is the adapter's configuration.
type Config struct {
	Credentials Credentials `json:"credentials,omitzero"`
	sinkkit.Common

	Address        string `json:"address,omitempty"`
	TLS            string `json:"tls,omitempty"`
	CACertificate  string `json:"ca_certificate,omitempty"`
	ServerName     string `json:"server_name,omitempty"`
	AppName        string `json:"app_name,omitempty"`
	Facility       string `json:"facility,omitempty"`
	Hostname       string `json:"hostname,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// Credentials is what this adapter keeps secret: a client certificate and its
// key, for a collector that requires mutual TLS. The certificate is not
// secret, but it is useless without the key and is set beside it.
type Credentials struct {
	ClientCertificate secret.Value `json:"client_certificate,omitzero"`
	ClientKey         secret.Value `json:"client_key,omitzero"`
}

// Adapter sends to one collector over one connection, reused between batches.
type Adapter struct {
	cfg      Config
	address  string
	tls      *tls.Config // nil when TLS is off
	priBase  int         // facility * 8
	hostname string
	now      func() time.Time

	mu   sync.Mutex
	conn net.Conn
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{now: time.Now} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryAuditSink }

// AuditSinkCapabilities reports how core feeds this adapter (R-254).
func (a *Adapter) AuditSinkCapabilities() api.AuditSinkCapabilities {
	return a.cfg.Capabilities("syslog", a.address)
}

// Configure reads the configuration core assembled.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	if err := sinkkit.RefuseInline(Kind, raw, "client_certificate", "client_key"); err != nil {
		return err
	}
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("syslog: reading configuration: %w", err)
		}
	}
	if err := cfg.Normalize(Kind, DefaultMaxBatch); err != nil {
		return err
	}

	switch cfg.TLS {
	case "":
		cfg.TLS = "on"
	case "on", "off":
	default:
		return fmt.Errorf("syslog: %q is not a tls setting. Valid answers: on (the default, RFC 5425) or off (plain TCP)", cfg.TLS)
	}
	address, host, err := normalizeAddress(cfg.Address, cfg.TLS == "on")
	if err != nil {
		return err
	}

	if cfg.Facility == "" {
		cfg.Facility = "authpriv"
	}
	facility, ok := facilities[cfg.Facility]
	if !ok {
		return fmt.Errorf("syslog: %q is not a facility Pando sends as. Valid answers: authpriv, auth, daemon, user, local0 through local7", cfg.Facility)
	}
	if cfg.AppName == "" {
		cfg.AppName = "pando"
	}
	hostname := cfg.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 10
	}

	certSet, keySet := !cfg.Credentials.ClientCertificate.IsZero(), !cfg.Credentials.ClientKey.IsZero()
	if certSet != keySet {
		return errors.New("syslog: mutual TLS needs both a client certificate and its private key. Set both credentials, or neither")
	}

	var tlsConfig *tls.Config
	if cfg.TLS == "on" {
		pool, err := sinkkit.CertPool(Kind, cfg.CACertificate)
		if err != nil {
			return err
		}
		serverName := cfg.ServerName
		if serverName == "" {
			serverName = host
		}
		tlsConfig = &tls.Config{ServerName: serverName, RootCAs: pool, MinVersion: tls.VersionTLS12}
		if certSet {
			pair, err := tls.X509KeyPair([]byte(cfg.Credentials.ClientCertificate.Reveal()), []byte(cfg.Credentials.ClientKey.Reveal()))
			if err != nil {
				// The parse error is not wrapped: it is about the key, and a
				// message about a key is one step from quoting it (R-194).
				return errors.New("syslog: the client certificate and key could not be read as a pair. Paste the PEM certificate and the PEM private key it was issued for, each with its BEGIN and END lines")
			}
			tlsConfig.Certificates = []tls.Certificate{pair}
		}
	} else if certSet || strings.TrimSpace(cfg.CACertificate) != "" {
		return errors.New("syslog: a CA certificate or client certificate is set but tls is off. Turn tls on, or remove them")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeLocked()
	a.cfg, a.address, a.tls = cfg, address, tlsConfig
	a.priBase = facility * 8
	a.hostname = header(hostname, 255)
	return nil
}

// normalizeAddress reads host or host:port, adding the transport's port.
func normalizeAddress(addr string, useTLS bool) (string, string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", errors.New("syslog: no address is set. Set address to the collector's host and port, such as siem.example.com:6514")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = strings.Trim(addr, "[]"), defaultPlainPort
		if useTLS {
			port = defaultTLSPort
		}
	}
	if n, perr := strconv.Atoi(port); host == "" || strings.ContainsAny(host, "/ ") || perr != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("syslog: %q is not a collector address. Use a host and port, such as siem.example.com:6514", addr)
	}
	return net.JoinHostPort(host, port), host, nil
}

func (a *Adapter) timeout() time.Duration { return time.Duration(a.cfg.TimeoutSeconds) * time.Second }

// dial connects and, with TLS on, completes the handshake.
func (a *Adapter) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: a.timeout()}
	var conn net.Conn
	var err error
	if a.tls != nil {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: a.tls}).DialContext(ctx, "tcp", a.address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", a.address)
	}
	if err != nil {
		accepts := "TCP"
		if a.tls != nil {
			accepts = "TCP with TLS, that its certificate is trusted for this address,"
		}
		return nil, errs.Wrap(errs.AdapterFailed, "Pando could not connect to the syslog collector at "+a.address+
			". Check the address, that the collector accepts "+accepts+" on that port, and that it accepts this installation's client certificate if it requires one.", err)
	}
	return conn, nil
}

// HealthCheck connects, completes the TLS handshake, and hangs up. It sends
// nothing: a test message would be an event the collector indexes.
//
// Under TLS 1.3 the client's handshake finishes before the server has judged
// its certificate, so a collector refusing it says so only afterward. A short
// read waits for that: a collector never writes, so running out of time is
// the healthy answer, and anything else — an alert, a hang-up — is not.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.address == "" {
		return errors.New("syslog: not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	conn, err := a.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if a.tls == nil {
		return nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(healthWait))
	var one [1]byte
	if _, err := conn.Read(one[:]); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil
		}
		return errs.Wrap(errs.AdapterFailed, "The syslog collector at "+a.address+
			" closed the connection after the TLS handshake. It may require a client certificate, or not accept the one set.", err)
	}
	return nil
}

// healthWait is how long HealthCheck listens for a collector refusing the
// client certificate after the handshake.
const healthWait = 300 * time.Millisecond

// Send writes every event in the batch as one framed message each.
func (a *Adapter) Send(ctx context.Context, b api.AuditBatch) error {
	if a.address == "" {
		return errs.New(errs.AdapterUnavailable, "The syslog audit sink is not configured.")
	}
	if len(b.Events) == 0 {
		return nil
	}
	var frames []byte
	for i, ev := range b.Events {
		frames = append(frames, a.frame(b, i, ev)...)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	reused := a.conn != nil
	err := a.writeLocked(ctx, frames)
	if err != nil && reused && ctx.Err() == nil {
		// A connection idle since the last batch may have been closed by the
		// collector. One fresh attempt; the collector may see some events
		// twice, which at-least-once delivery allows.
		err = a.writeLocked(ctx, frames)
	}
	return err
}

// writeLocked writes frames on the open connection, dialing one if needed. A
// failed write closes it, so the next attempt starts clean.
func (a *Adapter) writeLocked(ctx context.Context, frames []byte) error {
	if a.conn == nil {
		conn, err := a.dial(ctx)
		if err != nil {
			return err
		}
		a.conn = conn
	}
	deadline := time.Now().Add(a.timeout())
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = a.conn.SetWriteDeadline(deadline)
	// Cancellation without a deadline still ends the write.
	conn := a.conn
	stop := context.AfterFunc(ctx, func() { _ = conn.SetWriteDeadline(time.Unix(1, 0)) })
	_, err := a.conn.Write(frames)
	stop()
	if err != nil {
		a.closeLocked()
		return errs.Wrap(errs.AdapterFailed, "Pando could not finish sending audit events to the syslog collector at "+a.address+". They will be sent again.", err)
	}
	return nil
}

func (a *Adapter) closeLocked() {
	if a.conn != nil {
		_ = a.conn.Close()
		a.conn = nil
	}
}

// frame builds one octet-counted RFC 5424 message (RFC 5425 §4.3).
func (a *Adapter) frame(b api.AuditBatch, i int, ev json.RawMessage) []byte {
	severity, outcome := classify(ev)
	action := ""
	if i < len(b.Actions) {
		action = b.Actions[i]
	}
	var sd strings.Builder
	sd.WriteString("[" + enterpriseID)
	if i < len(b.IDs) {
		sd.WriteString(` id="` + strconv.FormatInt(b.IDs[i], 10) + `"`)
	}
	if outcome != "" {
		sd.WriteString(` outcome="` + sdEscape(outcome) + `"`)
	}
	sd.WriteString("]")

	msg := fmt.Sprintf("<%d>1 %s %s %s - %s %s %s",
		a.priBase+severity,
		a.now().UTC().Format("2006-01-02T15:04:05.000000Z07:00"),
		a.hostname,
		header(a.cfg.AppName, 48),
		header(action, 32),
		sd.String(),
		ev)
	return []byte(strconv.Itoa(len(msg)) + " " + msg)
}

// classify reads the one field of an event that sets its severity: a denied
// or failed event is a warning, everything else informational. Native events
// say "outcome"; OCSF ones say status_id 2 and put the outcome in
// status_detail. Absent fields are fine.
func classify(ev json.RawMessage) (int, string) {
	var e struct {
		Outcome      string `json:"outcome"`
		StatusID     int    `json:"status_id"`
		StatusDetail string `json:"status_detail"`
	}
	_ = json.Unmarshal(ev, &e)
	outcome := e.Outcome
	if outcome == "" {
		outcome = e.StatusDetail
	}
	if outcome == "" && e.StatusID == 1 {
		outcome = "success"
	}
	if outcome == "denied" || outcome == "failed" || e.StatusID == 2 {
		return severityWarning, outcome
	}
	return severityInformational, outcome
}

// header makes a header field RFC 5424 accepts: printable US-ASCII with no
// spaces, at most max characters, and "-" (the nil value) when empty.
func header(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 33 && r <= 126 && b.Len() < max {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
}

// sdEscape escapes a structured data parameter value (RFC 5424 §6.3.3).
func sdEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `]`, `\]`).Replace(s)
}

var _ api.AuditSinkAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one.
func Info() api.KindInfo {
	fields := []api.Field{
		{Key: "address", Label: "Address", Type: "string", Required: true, Placeholder: "siem.example.com:6514",
			Help: "The collector's host and port. Without a port, 6514 with TLS and 514 without."},
		{Key: "tls", Label: "TLS", Type: "select", Default: "on",
			Help: "Whether the connection is encrypted. Off sends the audit log across the network in the clear.",
			Options: []api.Option{
				{Value: "on", Label: "On", Description: "TLS, as RFC 5425 describes."},
				{Value: "off", Label: "Off", Description: "Plain TCP, for a collector on the same host or a trusted network."},
			}},
		{Key: "ca_certificate", Label: "CA certificate", Type: "string", Multiline: true,
			ShownWhen: &api.Condition{Key: "tls", Values: []string{"on", ""}},
			Help:      "PEM certificates to trust for a collector on a private certificate authority. Empty trusts the system's."},
		{Key: "client_certificate", Label: "Client certificate", Type: "string", Multiline: true, Credential: true,
			ShownWhen: &api.Condition{Key: "tls", Values: []string{"on", ""}},
			Help:      "A PEM certificate Pando presents, for a collector that requires mutual TLS. Set with its key, or not at all. Stored encrypted."},
		{Key: "client_key", Label: "Client key", Type: "string", Multiline: true, Credential: true,
			ShownWhen: &api.Condition{Key: "tls", Values: []string{"on", ""}},
			Help:      "The PEM private key for the client certificate. Stored encrypted and never shown again."},
		{Key: "server_name", Label: "Server name", Type: "string", Default: "the address's host", Advanced: true,
			Help: "The name the collector's certificate must carry, when it differs from the address."},
		{Key: "app_name", Label: "App name", Type: "string", Default: "pando", Advanced: true,
			Help: "The APP-NAME each message carries."},
		{Key: "facility", Label: "Facility", Type: "select", Default: "authpriv", Advanced: true,
			Help: "The syslog facility messages are sent as. authpriv is for security messages a collector keeps apart from the general log.",
			Options: []api.Option{
				{Value: "authpriv", Label: "authpriv"}, {Value: "auth", Label: "auth"}, {Value: "daemon", Label: "daemon"},
				{Value: "user", Label: "user"}, {Value: "local0", Label: "local0"}, {Value: "local1", Label: "local1"},
				{Value: "local2", Label: "local2"}, {Value: "local3", Label: "local3"}, {Value: "local4", Label: "local4"},
				{Value: "local5", Label: "local5"}, {Value: "local6", Label: "local6"}, {Value: "local7", Label: "local7"},
			}},
		{Key: "hostname", Label: "Hostname", Type: "string", Default: "this machine's hostname", Advanced: true,
			Help: "The HOSTNAME each message carries."},
		{Key: "timeout_seconds", Label: "Timeout", Type: "int", Default: "10", Advanced: true,
			Help: "Seconds to wait to connect, and for a batch to be written."},
	}
	return api.KindInfo{
		Category: api.CategoryAuditSink,
		Kind:     Kind,
		Name:     "Syslog",
		Description: "Sends every audit event, as it is written, to a syslog collector as an RFC 5424 message over TCP, encrypted with TLS unless you turn it off. " +
			"A copy of the audit log leaves this installation.",
		IDPrefix: "as_",
		Fields:   append(fields, sinkkit.Fields(DefaultMaxBatch)...),
	}
}
