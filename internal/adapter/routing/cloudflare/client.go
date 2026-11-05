package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// DefaultAPIBase is Cloudflare's API.
const DefaultAPIBase = "https://api.cloudflare.com/client/v4"

// client is the handful of Cloudflare API calls this adapter makes. Not an SDK
// on purpose: seven endpoints do not justify a dependency that would bring the
// rest of Cloudflare's API into the binary.
type client struct {
	base  string
	token secret.Value
	http  *http.Client
}

func newClient(base string, token secret.Value) *client {
	if base == "" {
		base = DefaultAPIBase
	}
	return &client{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// envelope is every Cloudflare API response.
type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// do makes one call and decodes its result into out, when out is not nil.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return errs.Wrap(errs.Internal, "Pando could not build a request to Cloudflare.", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not build a request to Cloudflare.", err)
	}
	// Revealed into the header and nowhere else (R-194).
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Pando could not reach Cloudflare's API.", err).
			WithRemedy("Check that this machine can reach api.cloudflare.com.")
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Pando could not read Cloudflare's answer.", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return errs.Newf(errs.AdapterFailed, "Cloudflare answered %s %s with something that is not its API's format (HTTP %d).",
			method, path, resp.StatusCode)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errs.Newf(errs.AdapterFailed, "Cloudflare refused the API token: %s", joinErrors(env.Errors)).
			WithRemedy(tokenRemedy)
	}
	if !env.Success || resp.StatusCode >= 300 {
		return errs.Wrap(errs.AdapterFailed,
			fmt.Sprintf("Cloudflare refused %s %s: %s", method, path, joinErrors(env.Errors)),
			&refusal{status: resp.StatusCode, errors: env.Errors})
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return errs.Wrap(errs.AdapterFailed, "Pando could not read Cloudflare's answer.", err)
		}
	}
	return nil
}

// refusal is what Cloudflare answered when it refused a call, kept under the
// error a caller sees so the adapter can tell one refusal from another.
type refusal struct {
	status int
	errors []apiError
}

func (r *refusal) Error() string {
	return fmt.Sprintf("HTTP %d: %s", r.status, joinErrors(r.errors))
}

// notFound reports whether Cloudflare refused a call because what it named
// does not exist: HTTP 404, or a tunnel whose configuration it no longer has
// (code 1055), which is how a deleted tunnel's configuration answers.
func notFound(err error) bool {
	var r *refusal
	if !errors.As(err, &r) {
		return false
	}
	if r.status == http.StatusNotFound {
		return true
	}
	for _, e := range r.errors {
		if e.Code == codeTunnelConfigNotFound {
			return true
		}
	}
	return false
}

const codeTunnelConfigNotFound = 1055

func joinErrors(list []apiError) string {
	if len(list) == 0 {
		return "no reason given."
	}
	parts := make([]string, 0, len(list))
	for _, e := range list {
		parts = append(parts, fmt.Sprintf("%s (code %d)", strings.TrimSuffix(e.Message, "."), e.Code))
	}
	return strings.Join(parts, "; ") + "."
}

// --- zones -------------------------------------------------------------------

type zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *client) zoneByName(ctx context.Context, name string) (zone, error) {
	var zones []zone
	if err := c.do(ctx, http.MethodGet, "/zones", url.Values{"name": {name}}, nil, &zones); err != nil {
		return zone{}, err
	}
	if len(zones) == 0 {
		return zone{}, errs.Newf(errs.AdapterFailed, "The Cloudflare API token cannot see a zone named %s.", name).
			WithRemedy("Check the zone's spelling, and that the token has Zone → Zone: Read for it.")
	}
	return zones[0], nil
}

// --- tunnels -------------------------------------------------------------------

type tunnel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// DeletedAt is set on a deleted tunnel, which Cloudflare still returns by
	// ID.
	DeletedAt *string `json:"deleted_at,omitempty"`
}

func (c *client) tunnelByName(ctx context.Context, account, name string) (*tunnel, error) {
	var list []tunnel
	q := url.Values{"name": {name}, "is_deleted": {"false"}}
	if err := c.do(ctx, http.MethodGet, "/accounts/"+account+"/cfd_tunnel", q, nil, &list); err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

func (c *client) tunnel(ctx context.Context, account, id string) (tunnel, error) {
	var t tunnel
	err := c.do(ctx, http.MethodGet, "/accounts/"+account+"/cfd_tunnel/"+id, nil, nil, &t)
	return t, err
}

// createTunnel makes a remotely managed tunnel: its ingress lives at
// Cloudflare and is written through the API, so cloudflared needs nothing on
// disk.
func (c *client) createTunnel(ctx context.Context, account, name string) (tunnel, error) {
	var t tunnel
	body := map[string]string{"name": name, "config_src": "cloudflare"}
	err := c.do(ctx, http.MethodPost, "/accounts/"+account+"/cfd_tunnel", nil, body, &t)
	return t, err
}

func (c *client) tunnelToken(ctx context.Context, account, id string) (secret.Value, error) {
	var token string
	if err := c.do(ctx, http.MethodGet, "/accounts/"+account+"/cfd_tunnel/"+id+"/token", nil, nil, &token); err != nil {
		return secret.Value{}, err
	}
	return secret.New(token), nil
}

// ingressRule is one rule of a tunnel's ingress. Cloudflare matches them in
// order; the last must match everything.
type ingressRule struct {
	Hostname string `json:"hostname,omitempty"`
	Path     string `json:"path,omitempty"`
	Service  string `json:"service"`

	// Anything else a rule carries — origin request settings someone set in
	// the dashboard — is kept as it was on a rule Pando did not write.
	Extra map[string]json.RawMessage `json:"-"`
}

func (r ingressRule) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	for k, v := range r.Extra {
		m[k] = v
	}
	if r.Hostname != "" {
		m["hostname"] = r.Hostname
	}
	if r.Path != "" {
		m["path"] = r.Path
	}
	m["service"] = r.Service
	return json.Marshal(m)
}

func (r *ingressRule) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	take := func(k string) string {
		var s string
		if v, ok := m[k]; ok {
			_ = json.Unmarshal(v, &s)
			delete(m, k)
		}
		return s
	}
	r.Hostname, r.Path, r.Service = take("hostname"), take("path"), take("service")
	r.Extra = m
	return nil
}

type tunnelConfig struct {
	Ingress []ingressRule `json:"ingress"`

	// The rest of the configuration — warp routing, origin defaults — is
	// kept as found and written back untouched.
	Extra map[string]json.RawMessage `json:"-"`
}

func (c *client) tunnelConfig(ctx context.Context, account, id string) (tunnelConfig, error) {
	var out struct {
		Config json.RawMessage `json:"config"`
	}
	if err := c.do(ctx, http.MethodGet, "/accounts/"+account+"/cfd_tunnel/"+id+"/configurations", nil, nil, &out); err != nil {
		return tunnelConfig{}, err
	}
	cfg := tunnelConfig{Extra: map[string]json.RawMessage{}}
	if len(out.Config) == 0 || string(out.Config) == "null" {
		return cfg, nil
	}
	if err := json.Unmarshal(out.Config, &cfg.Extra); err != nil {
		return tunnelConfig{}, errs.Wrap(errs.AdapterFailed, "Pando could not read the tunnel's configuration.", err)
	}
	if raw, ok := cfg.Extra["ingress"]; ok {
		if err := json.Unmarshal(raw, &cfg.Ingress); err != nil {
			return tunnelConfig{}, errs.Wrap(errs.AdapterFailed, "Pando could not read the tunnel's ingress rules.", err)
		}
		delete(cfg.Extra, "ingress")
	}
	return cfg, nil
}

func (c *client) putTunnelConfig(ctx context.Context, account, id string, cfg tunnelConfig) error {
	body := map[string]any{}
	for k, v := range cfg.Extra {
		body[k] = v
	}
	body["ingress"] = cfg.Ingress
	return c.do(ctx, http.MethodPut, "/accounts/"+account+"/cfd_tunnel/"+id+"/configurations", nil,
		map[string]any{"config": body}, nil)
}

// --- DNS -------------------------------------------------------------------------

type dnsRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment,omitempty"`
	TTL     int    `json:"ttl,omitempty"`
}

// recordsNamed is every record at a name, of any type: a CNAME cannot sit
// beside another record, so any of them is a conflict.
func (c *client) recordsNamed(ctx context.Context, zoneID, name string) ([]dnsRecord, error) {
	var list []dnsRecord
	err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/dns_records", url.Values{"name": {name}}, nil, &list)
	return list, err
}

func (c *client) createRecord(ctx context.Context, zoneID string, r dnsRecord) error {
	return c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", nil, r, nil)
}

func (c *client) updateRecord(ctx context.Context, zoneID string, r dnsRecord) error {
	id := r.ID
	r.ID = ""
	return c.do(ctx, http.MethodPut, "/zones/"+zoneID+"/dns_records/"+id, nil, r, nil)
}

func (c *client) deleteRecord(ctx context.Context, zoneID, id string) error {
	return c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+id, nil, nil, nil)
}
