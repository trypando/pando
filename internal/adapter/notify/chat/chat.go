// Package chat is the notify adapters for chat platforms that take an
// incoming webhook: Slack, Microsoft Teams and Discord (R-374, issue #50).
//
// Each posts to one channel, named by a webhook URL an administrator creates
// in the platform, so each is a channel adapter: it ignores recipients, and
// only event subscriptions send to it (R-373). A message meant for one person
// never lands in a room.
//
// The webhook URL is the credential. Whoever holds it can post to the channel,
// so it is stored sealed like any adapter credential and never shown again
// (R-190).
package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Platform is what differs between the three: a name, and how a notification
// is laid out.
type Platform struct {
	Kind        string
	Name        string
	Description string
	IDPrefix    string
	Placeholder string
	Help        string

	// Hosts are the hosts a webhook URL for the platform is on. A URL on
	// another host is refused at configuration, because a webhook URL pasted
	// into the wrong platform's form fails at the first event otherwise.
	// Empty accepts any host.
	Hosts []string

	Format func(n api.Notification) any
}

// Config is the adapter's configuration.
type Config struct {
	Credentials    Credentials `json:"credentials,omitzero"`
	TimeoutSeconds int         `json:"timeout_seconds,omitempty"`
}

// Credentials is what this adapter keeps secret.
type Credentials struct {
	WebhookURL secret.Value `json:"webhook_url,omitzero"`
}

// Adapter posts to one channel.
type Adapter struct {
	platform Platform
	cfg      Config
	client   *http.Client
}

// New returns an unconfigured adapter for platform.
func New(p Platform) *Adapter { return &Adapter{platform: p} }

func (a *Adapter) Kind() string           { return a.platform.Kind }
func (a *Adapter) Category() api.Category { return api.CategoryNotify }

// Capabilities: a channel, not people.
func (a *Adapter) Capabilities() api.NotifyCapabilities {
	return api.NotifyCapabilities{Audience: api.AudienceChannel}
}

// Configure reads the configuration core assembled, credentials included.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return fmt.Errorf("%s: reading configuration: %w", a.platform.Kind, err)
		}
		if _, inline := top["webhook_url"]; inline {
			return fmt.Errorf("%s: webhook_url is in this adapter's stored configuration, which is unencrypted; set it as a credential instead", a.platform.Kind)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("%s: reading configuration: %w", a.platform.Kind, err)
		}
	}
	if cfg.Credentials.WebhookURL.IsZero() {
		return fmt.Errorf("%s: no webhook URL is set. Create an incoming webhook in %s and set its URL as this adapter's webhook_url credential", a.platform.Kind, a.platform.Name)
	}
	u, err := url.Parse(cfg.Credentials.WebhookURL.Reveal())
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%s: the webhook URL is not an https address. Copy it again from %s", a.platform.Kind, a.platform.Name)
	}
	if len(a.platform.Hosts) > 0 && !hostIn(u.Hostname(), a.platform.Hosts) {
		return fmt.Errorf("%s: the webhook URL is on %s, which is not a %s address. Check it was copied from %s", a.platform.Kind, u.Hostname(), a.platform.Name, a.platform.Name)
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 10
	}
	a.cfg = cfg
	a.client = &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second}
	return nil
}

func hostIn(host string, hosts []string) bool {
	for _, h := range hosts {
		if host == h || len(host) > len(h) && host[len(host)-len(h)-1:] == "."+h {
			return true
		}
	}
	return false
}

// HealthCheck succeeds once configured. It does not post: a health check
// that sends a message to a channel every five minutes would be the outage.
func (a *Adapter) HealthCheck(context.Context) error {
	if a.client == nil {
		return errors.New(a.platform.Kind + ": not configured")
	}
	return nil
}

// Notify posts one message.
func (a *Adapter) Notify(ctx context.Context, n api.Notification) error {
	if a.client == nil {
		return errs.Newf(errs.AdapterUnavailable, "%s is not configured.", a.platform.Name)
	}
	body, err := json.Marshal(a.platform.Format(n))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.Credentials.WebhookURL.Reveal(), bytes.NewReader(body))
	if err != nil {
		return errs.Newf(errs.AdapterFailed, "%s's webhook URL could not be used.", a.platform.Name)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		// The error names the URL, which is the credential; say what
		// happened without it.
		return errs.Newf(errs.AdapterFailed, "Pando could not reach %s. Check the Pando server can reach the internet.", a.platform.Name)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s answered %d: %s", a.platform.Name, resp.StatusCode, bytes.TrimSpace(snippet))
	}
	return nil
}

var _ api.NotifyAdapter = (*Adapter)(nil)

// Info describes the platform's adapter for the forms that configure one.
func (p Platform) Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryNotify,
		Kind:        p.Kind,
		Name:        p.Name,
		Description: p.Description,
		IDPrefix:    p.IDPrefix,
		Fields: []api.Field{
			{Key: "webhook_url", Label: "Webhook URL", Type: "string", Help: p.Help + " Stored encrypted and never shown again.",
				Credential: true, Required: true, Placeholder: p.Placeholder},
			{Key: "timeout_seconds", Label: "Timeout", Type: "int", Help: "Seconds to wait for " + p.Name + " to answer.", Default: "10", Advanced: true},
		},
	}
}
