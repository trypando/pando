// Package ntfy is the notify adapter for ntfy, the push notification service
// (R-374, issue #50): ntfy.sh or a server of your own.
//
// A topic is a channel, so this is a channel adapter and only subscriptions
// send to it (R-373). On ntfy.sh anyone who knows a topic's name can read it,
// which makes the name a credential: it is stored sealed, like a password.
package ntfy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "ntfy"

// DefaultServer is the public ntfy server.
const DefaultServer = "https://ntfy.sh"

// Config is the adapter's configuration.
type Config struct {
	Credentials    Credentials `json:"credentials,omitzero"`
	ServerURL      string      `json:"server_url,omitempty"`
	Priority       string      `json:"priority,omitempty"`
	TimeoutSeconds int         `json:"timeout_seconds,omitempty"`
}

// Credentials is what this adapter keeps secret.
type Credentials struct {
	Topic       secret.Value `json:"topic,omitzero"`
	AccessToken secret.Value `json:"access_token,omitzero"`
}

// Adapter publishes to one topic.
type Adapter struct {
	cfg    Config
	client *http.Client
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryNotify }

// Capabilities: a channel, not people.
func (a *Adapter) Capabilities() api.NotifyCapabilities {
	return api.NotifyCapabilities{Audience: api.AudienceChannel}
}

// Configure reads the configuration core assembled.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return fmt.Errorf("ntfy: reading configuration: %w", err)
		}
		for _, k := range []string{"topic", "access_token"} {
			if _, inline := top[k]; inline {
				return fmt.Errorf("ntfy: %s is in this adapter's stored configuration, which is unencrypted; set it as a credential instead", k)
			}
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("ntfy: reading configuration: %w", err)
		}
	}
	if cfg.Credentials.Topic.IsZero() {
		return errors.New("ntfy: no topic is set. Set the topic to publish to as this adapter's topic credential")
	}
	if strings.ContainsAny(cfg.Credentials.Topic.Reveal(), "/?# ") {
		return errors.New("ntfy: a topic is one name, such as pando-alerts-7f3k, with no slashes or spaces")
	}
	if cfg.ServerURL == "" {
		cfg.ServerURL = DefaultServer
	}
	if u, err := url.Parse(cfg.ServerURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("ntfy: %q is not a server address. Use one such as %s", cfg.ServerURL, DefaultServer)
	}
	switch cfg.Priority {
	case "", "min", "low", "default", "high", "urgent", "max":
	default:
		return fmt.Errorf("ntfy: %q is not a priority. Valid answers: min, low, default, high, urgent", cfg.Priority)
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 10
	}
	a.cfg = cfg
	a.client = &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second}
	return nil
}

// HealthCheck succeeds once configured. Publishing to check would be the
// thing it checks for.
func (a *Adapter) HealthCheck(context.Context) error {
	if a.client == nil {
		return errors.New("ntfy: not configured")
	}
	return nil
}

// Notify publishes one message.
func (a *Adapter) Notify(ctx context.Context, n api.Notification) error {
	if a.client == nil {
		return errs.New(errs.AdapterUnavailable, "ntfy is not configured.")
	}
	var body strings.Builder
	body.WriteString(n.Body)
	for _, f := range n.Fields {
		body.WriteString("\n" + f.Label + ": " + f.Value)
	}
	target := strings.TrimRight(a.cfg.ServerURL, "/") + "/" + url.PathEscape(a.cfg.Credentials.Topic.Reveal())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(strings.TrimSpace(body.String())))
	if err != nil {
		return errs.New(errs.AdapterFailed, "The ntfy server address could not be used.")
	}
	// Header values cannot carry a line break.
	req.Header.Set("Title", strings.Join(strings.Fields(n.Subject), " "))
	if a.cfg.Priority != "" {
		req.Header.Set("Priority", a.cfg.Priority)
	}
	if n.Link != "" {
		req.Header.Set("Click", n.Link)
	}
	if !a.cfg.Credentials.AccessToken.IsZero() {
		req.Header.Set("Authorization", "Bearer "+a.cfg.Credentials.AccessToken.Reveal())
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return errs.Newf(errs.AdapterFailed, "Pando could not reach the ntfy server at %s.", a.cfg.ServerURL)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("the ntfy server answered %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

var _ api.NotifyAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one.
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryNotify,
		Kind:        Kind,
		Name:        "ntfy",
		Description: "Publishes the events a subscription chooses to an ntfy topic, on ntfy.sh or your own server, for push notifications on a phone or desktop.",
		IDPrefix:    "ntf_",
		Fields: []api.Field{
			{Key: "topic", Label: "Topic", Type: "string", Credential: true, Required: true, Placeholder: "pando-alerts-7f3k",
				Help: "The topic to publish to. On ntfy.sh anyone who knows a topic's name can read it, so pick one nobody would guess. Stored encrypted."},
			{Key: "server_url", Label: "Server", Type: "string", Default: DefaultServer, Help: "The ntfy server."},
			{Key: "access_token", Label: "Access token", Type: "string", Credential: true,
				Help: "A token for a server that requires one. Stored encrypted and never shown again."},
			{Key: "priority", Label: "Priority", Type: "select", Default: "default", Advanced: true,
				Help: "How insistently the phone announces a message.",
				Options: []api.Option{{Value: "min", Label: "Min"}, {Value: "low", Label: "Low"}, {Value: "default", Label: "Default"},
					{Value: "high", Label: "High"}, {Value: "urgent", Label: "Urgent"}}},
			{Key: "timeout_seconds", Label: "Timeout", Type: "int", Default: "10", Advanced: true, Help: "Seconds to wait for the server to answer."},
		},
	}
}
