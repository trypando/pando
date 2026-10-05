// Package smtp is the email notify adapter (R-232, R-374, issue #50).
//
// It reaches people: each notification goes to the email address of every
// account it names, so Pando's own notifications reach someone who is not
// looking at the console — which is what R-266 was waiting for. Any SMTP
// server will do, SendGrid's, Mailgun's and Amazon SES's relays included.
package smtp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	netsmtp "net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "smtp"

// Security is how the connection is protected.
const (
	SecurityStartTLS = "starttls"
	SecurityTLS      = "tls"
	SecurityNone     = "none"
)

// Config is the adapter's configuration.
type Config struct {
	Credentials    Credentials `json:"credentials,omitzero"`
	Host           string      `json:"host,omitempty"`
	Port           int         `json:"port,omitempty"`
	Username       string      `json:"username,omitempty"`
	From           string      `json:"from,omitempty"`
	Security       string      `json:"security,omitempty"`
	TimeoutSeconds int         `json:"timeout_seconds,omitempty"`
}

// Credentials is what this adapter keeps secret.
type Credentials struct {
	Password secret.Value `json:"password,omitzero"`
}

// Adapter sends email.
type Adapter struct {
	cfg  Config
	from *mail.Address

	// dial is replaced in tests.
	dial func(ctx context.Context) (*netsmtp.Client, error)
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryNotify }

// Capabilities: email reaches the people a notification names.
func (a *Adapter) Capabilities() api.NotifyCapabilities {
	return api.NotifyCapabilities{Audience: api.AudiencePeople}
}

// Configure reads the configuration core assembled.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return fmt.Errorf("smtp: reading configuration: %w", err)
		}
		if _, inline := top["password"]; inline {
			return errors.New("smtp: password is in this adapter's stored configuration, which is unencrypted; set it as a credential instead")
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("smtp: reading configuration: %w", err)
		}
	}
	if cfg.Host == "" {
		return errors.New("smtp: no server is set. Set host to your mail server, such as smtp.sendgrid.net")
	}
	if cfg.Security == "" {
		cfg.Security = SecurityStartTLS
	}
	switch cfg.Security {
	case SecurityStartTLS, SecurityTLS, SecurityNone:
	default:
		return fmt.Errorf("smtp: %q is not a security setting. Valid answers: starttls, tls, none", cfg.Security)
	}
	if cfg.Port == 0 {
		cfg.Port = 587
		if cfg.Security == SecurityTLS {
			cfg.Port = 465
		}
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return fmt.Errorf("smtp: %q is not an address to send from. Use one such as Pando <pando@example.com>", cfg.From)
	}
	if !cfg.Credentials.Password.IsZero() && cfg.Username == "" {
		return errors.New("smtp: a password is set and no username. Set username, or remove the password for a server that needs none")
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 15
	}
	a.cfg, a.from = cfg, from
	if a.dial == nil {
		a.dial = a.connect
	}
	return nil
}

func (a *Adapter) timeout() time.Duration { return time.Duration(a.cfg.TimeoutSeconds) * time.Second }

// connect opens a session, secured as configured, and signs in.
func (a *Adapter) connect(ctx context.Context) (*netsmtp.Client, error) {
	addr := net.JoinHostPort(a.cfg.Host, strconv.Itoa(a.cfg.Port))
	dialer := &net.Dialer{Timeout: a.timeout()}
	tlsConfig := &tls.Config{ServerName: a.cfg.Host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var err error
	if a.cfg.Security == SecurityTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Pando could not connect to the mail server "+addr+".", err)
	}
	_ = conn.SetDeadline(time.Now().Add(a.timeout()))
	c, err := netsmtp.NewClient(conn, a.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("the mail server %s did not answer as a mail server: %w", addr, err)
	}
	if a.cfg.Security == SecurityStartTLS {
		if err := c.StartTLS(tlsConfig); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("the mail server %s refused to encrypt the connection (STARTTLS): %w", addr, err)
		}
	}
	if a.cfg.Username != "" {
		auth := netsmtp.PlainAuth("", a.cfg.Username, a.cfg.Credentials.Password.Reveal(), a.cfg.Host)
		if err := c.Auth(auth); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("the mail server %s refused the username and password: %w", addr, err)
		}
	}
	return c, nil
}

// HealthCheck connects, signs in and says goodbye, sending nothing.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.dial == nil {
		return errors.New("smtp: not configured")
	}
	c, err := a.dial(ctx)
	if err != nil {
		return err
	}
	return c.Quit()
}

// Notify sends one email to each recipient that has an address. A recipient
// without one is skipped: an account with no email is someone this adapter
// cannot reach, and the console still can.
func (a *Adapter) Notify(ctx context.Context, n api.Notification) error {
	if a.dial == nil {
		return errs.New(errs.AdapterUnavailable, "Email is not configured.")
	}
	var to []string
	for _, r := range n.Recipients {
		if addr, err := mail.ParseAddress(r.Email); err == nil {
			to = append(to, addr.Address)
		}
	}
	if len(to) == 0 {
		return nil
	}

	c, err := a.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	// One message per recipient, so nobody sees who else was told.
	for _, rcpt := range to {
		if err := a.send(c, rcpt, n); err != nil {
			return err
		}
	}
	return c.Quit()
}

func (a *Adapter) send(c *netsmtp.Client, rcpt string, n api.Notification) error {
	if err := c.Mail(a.from.Address); err != nil {
		return fmt.Errorf("the mail server refused the sender %s: %w", a.from.Address, err)
	}
	if err := c.Rcpt(rcpt); err != nil {
		return fmt.Errorf("the mail server refused the recipient %s: %w", rcpt, err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("the mail server refused the message: %w", err)
	}
	if _, err := w.Write(Message(a.from, rcpt, n, time.Now())); err != nil {
		return fmt.Errorf("the mail server refused the message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the mail server refused the message: %w", err)
	}
	return nil
}

// Message renders one email: plain text, UTF-8, with a subject that cannot
// carry a header of its own (a line break in an app's name ends there).
func Message(from *mail.Address, to string, n api.Notification, now time.Time) []byte {
	subject := strings.Join(strings.Fields(n.Subject), " ")

	var body strings.Builder
	body.WriteString(n.Body)
	if len(n.Fields) > 0 {
		body.WriteString("\n")
		for _, f := range n.Fields {
			body.WriteString("\n" + f.Label + ": " + f.Value)
		}
	}
	if n.Link != "" {
		body.WriteString("\n\n" + n.Link)
	}
	body.WriteString("\n\n-- \nSent by Pando. Choose which notifications reach you under Account, Notifications.\n")

	var b strings.Builder
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from.String())
	header("To", to)
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", now.UTC().Format(time.RFC1123Z))
	header("Message-ID", "<"+messageID()+"@"+domainOf(from.Address)+">")
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "8bit")
	header("Auto-Submitted", "auto-generated")
	if n.EventID != "" {
		header("X-Pando-Event-Id", n.EventID)
	}
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body.String(), "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}

func messageID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return "pando.invalid"
}

var _ api.NotifyAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one.
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryNotify,
		Kind:        Kind,
		Name:        "Email (SMTP)",
		Description: "Emails Pando's notifications to the people they are for, at the address on their account, through any SMTP server — SendGrid, Mailgun and Amazon SES included. Event subscriptions can send to it too.",
		IDPrefix:    "ntf_",
		Fields: []api.Field{
			{Key: "host", Label: "Server", Type: "string", Required: true, Placeholder: "smtp.sendgrid.net", Help: "The mail server's host name."},
			{Key: "from", Label: "From", Type: "string", Required: true, Placeholder: "Pando <pando@example.com>", Help: "The address email comes from."},
			{Key: "username", Label: "Username", Type: "string", Help: "For SendGrid, apikey."},
			{Key: "password", Label: "Password", Type: "string", Credential: true, Help: "For SendGrid, an API key. Stored encrypted and never shown again."},
			{Key: "security", Label: "Security", Type: "select", Default: SecurityStartTLS, Advanced: true, Help: "How the connection is encrypted.",
				Options: []api.Option{{Value: SecurityStartTLS, Label: "STARTTLS"}, {Value: SecurityTLS, Label: "TLS"}, {Value: SecurityNone, Label: "None"}}},
			{Key: "port", Label: "Port", Type: "int", Default: "587", Advanced: true, Help: "587 for STARTTLS, 465 for TLS."},
			{Key: "timeout_seconds", Label: "Timeout", Type: "int", Default: "15", Advanced: true, Help: "Seconds to wait for the server."},
		},
	}
}
