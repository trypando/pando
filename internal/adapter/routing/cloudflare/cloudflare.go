// Package cloudflare routes apps through a Cloudflare Tunnel (R-160, design 03
// §4.5).
//
// cloudflared runs beside Pando and dials out to Cloudflare, so nothing on
// this machine needs to be reachable from the internet and Cloudflare issues
// the certificates. Every ingress rule this adapter writes sends traffic to
// Pando's proxy — never to an app's container. R-023 has no exception for a
// tunnel: a rule pointing at a workload would be a way to an app that skips
// authorization, and it would work, which is why the instinct is dangerous.
//
// Cloudflare Access is not configured here. It could only ever be a layer in
// front of Pando's proxy, never the only check; whether Pando should manage it
// at all is O-23.
package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "cloudflare"

// DefaultImage is the cloudflared this release of Pando runs.
const DefaultImage = "cloudflare/cloudflared:2025.1.0"

// ManagedComment marks every DNS record Pando created. A record without it is
// not Pando's, and Pando never changes or removes one (design 03 §4.5).
const ManagedComment = "Managed by Pando — changes are reset"

// Config is the adapter's configuration.
type Config struct {
	AccountID string `json:"account_id"`

	// Zone is the Cloudflare zone apps are named in, such as example.com. An
	// app is <app>.<zone>, never deeper: Cloudflare's included certificate
	// covers *.zone and nothing below it, so a deeper name would have no
	// HTTPS. There is deliberately no setting for one.
	Zone string `json:"zone"`

	// ConsoleHostname is where the console is served, and the host path-mode
	// apps are served under. Empty is the zone itself.
	ConsoleHostname string `json:"console_hostname,omitempty"`

	// TunnelID attaches to an existing tunnel instead of creating one. Pando
	// still writes its rules into that tunnel's ingress, and leaves the rules
	// it did not write alone.
	TunnelID string `json:"tunnel_id,omitempty"`

	// Image is the cloudflared image. Pinned by Pando's release; set to
	// override.
	Image string `json:"image,omitempty"`

	// APIBase is Cloudflare's API. Settable for tests, not offered in the form.
	APIBase string `json:"api_base,omitempty"`

	Credentials struct {
		APIToken secret.Value `json:"api_token,omitzero"`
	} `json:"credentials,omitzero"`
}

// Adapter writes a tunnel's ingress and DNS through Cloudflare's API.
type Adapter struct {
	config Config
	api    *client

	// mu serializes changes to the tunnel's ingress, which is one document:
	// two deploys writing it at once would each drop the other's rule.
	mu sync.Mutex

	// Found once and kept: the zone, the tunnel. Guarded by mu.
	zoneID   string
	tunnelID string
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryRouting }

var (
	accountPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	uuidPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		// A token at the top level was stored in the clear. The create
		// handler and the database both refuse one; refusing it here too means
		// a row written some other way does not quietly work (R-190).
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The Cloudflare routing configuration could not be read.", err)
		}
		if _, inline := top["api_token"]; inline {
			return errs.New(errs.ValidInvalid, "The Cloudflare API token is in this adapter's stored settings, which are not encrypted.").
				WithRemedy("Set it as the adapter's API token credential instead.")
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The Cloudflare routing configuration could not be read.", err)
		}
	}

	cfg.Zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfg.Zone), "."))
	cfg.ConsoleHostname = strings.ToLower(strings.TrimSpace(cfg.ConsoleHostname))

	switch {
	case cfg.Credentials.APIToken.IsZero():
		return errs.New(errs.ValidInvalid, "Cloudflare routing needs an API token, and none is set.").
			WithRemedy(tokenRemedy)
	case !accountPattern.MatchString(cfg.AccountID):
		return errs.New(errs.ValidInvalid, "Cloudflare routing needs the account ID, a 32-character code shown on the account's overview page in Cloudflare's dashboard.")
	case cfg.Zone == "" || !strings.Contains(cfg.Zone, "."):
		return errs.New(errs.ValidInvalid, "Cloudflare routing needs the zone apps are named in, such as example.com.")
	case cfg.TunnelID != "" && !uuidPattern.MatchString(cfg.TunnelID):
		return errs.Newf(errs.ValidInvalid, "%q is not a Cloudflare tunnel ID. A tunnel ID looks like 6ff42ae2-765d-4adf-8112-31c55c1551ef.", cfg.TunnelID)
	}
	if cfg.ConsoleHostname == "" {
		cfg.ConsoleHostname = cfg.Zone
	}
	if !within(cfg.ConsoleHostname, cfg.Zone) {
		return errs.Newf(errs.ValidInvalid, "The console hostname %s is not in the Cloudflare zone %s.", cfg.ConsoleHostname, cfg.Zone)
	}
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}

	a.config = cfg
	a.api = newClient(cfg.APIBase, cfg.Credentials.APIToken)
	a.zoneID, a.tunnelID = "", cfg.TunnelID
	return nil
}

// HealthCheck asks Cloudflare whether the token can see the zone.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	_, err := a.zone(ctx)
	return err
}

// Capabilities: subdomain by default, path under the console hostname (R-164).
//
// TLS is Cloudflare's: its included certificate covers the zone and one level
// below, which is exactly where apps are named.
func (a *Adapter) Capabilities(context.Context) (api.RoutingCapabilities, error) {
	return api.RoutingCapabilities{
		Modes:               []api.RoutingMode{spec.RoutingSubdomain, spec.RoutingPath},
		DefaultMode:         spec.RoutingSubdomain,
		BaseDomain:          a.config.Zone,
		SupportsTLS:         true,
		SupportsWildcardTLS: true,

		// The tunnel dials out. Nothing here needs to be reachable.
		RequiresPublicReachability: false,
	}, nil
}

// Ensure writes an app's ingress rule and, for its own hostname, a DNS record.
//
// Idempotent: the rule is found by hostname and path and replaced whole, so a
// change someone made to it in Cloudflare's dashboard is reset here — which is
// how the reconciler's next pass undoes drift.
func (a *Adapter) Ensure(ctx context.Context, r api.RouteRequest) (api.RouteHandle, error) {
	if r.ProxyUpstream == "" {
		return api.RouteHandle{}, errs.New(errs.AdapterFailed, "Pando did not say where to send this app's traffic.")
	}
	rule, err := a.rule(r)
	if err != nil {
		return api.RouteHandle{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	tunnelID, err := a.ensureTunnel(ctx)
	if err != nil {
		return api.RouteHandle{}, err
	}
	if err := a.ensureRecord(ctx, rule.Hostname, tunnelID); err != nil {
		return api.RouteHandle{}, err
	}
	if err := a.updateIngress(ctx, tunnelID, func(rules []ingressRule) []ingressRule {
		return upsert(rules, rule)
	}); err != nil {
		return api.RouteHandle{}, err
	}
	return api.RouteHandle{AppID: r.AppID, Handle: handleOf(rule)}, nil
}

// Remove deletes the app's rule, and its DNS record when Pando made one and no
// other rule still needs the name.
func (a *Adapter) Remove(ctx context.Context, h api.RouteHandle) error {
	host, path := parseHandle(h.Handle)
	if host == "" {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	tunnelID, err := a.ensureTunnel(ctx)
	if err != nil {
		return err
	}
	stillUsed := false
	if err := a.updateIngress(ctx, tunnelID, func(rules []ingressRule) []ingressRule {
		out := rules[:0]
		for _, r := range rules {
			if r.Hostname == host && r.Path == path {
				continue
			}
			if r.Hostname == host {
				stillUsed = true
			}
			out = append(out, r)
		}
		return out
	}); err != nil {
		return err
	}
	if stillUsed || host == a.config.ConsoleHostname {
		return nil
	}
	return a.removeRecord(ctx, host)
}

// Observe reads the rule back. It reports and never remediates (design 05):
// a rule changed in the dashboard shows here as pointing elsewhere, and the
// reconciler's next Ensure puts it back.
func (a *Adapter) Observe(ctx context.Context, h api.RouteHandle) (api.RouteState, error) {
	host, path := parseHandle(h.Handle)
	a.mu.Lock()
	tunnelID, err := a.knownTunnel(ctx)
	a.mu.Unlock()
	if err != nil || tunnelID == "" {
		return api.RouteState{}, err
	}
	cfg, err := a.api.tunnelConfig(ctx, a.config.AccountID, tunnelID)
	if err != nil {
		return api.RouteState{}, err
	}
	for _, r := range cfg.Ingress {
		if r.Hostname == host && r.Path == path {
			return api.RouteState{Present: true, Address: r.Service}, nil
		}
	}
	return api.RouteState{}, nil
}

// Edge makes sure the tunnel exists and routes the console, then describes the
// cloudflared that holds it open (R-174).
func (a *Adapter) Edge(ctx context.Context, r api.EdgeRequest) (api.EdgePlan, bool, error) {
	if r.ProxyUpstream == "" {
		return api.EdgePlan{}, false, errs.New(errs.AdapterFailed, "Pando did not say where the tunnel should send traffic.")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	tunnelID, err := a.ensureTunnel(ctx)
	if err != nil {
		return api.EdgePlan{}, false, err
	}
	if err := a.ensureRecord(ctx, a.config.ConsoleHostname, tunnelID); err != nil {
		return api.EdgePlan{}, false, err
	}
	// The console's hostname, and last, everything else the tunnel is asked
	// for: Pando answers a hostname that is not an app's with the console.
	if err := a.updateIngress(ctx, tunnelID, func(rules []ingressRule) []ingressRule {
		return upsert(rules, ingressRule{Hostname: a.config.ConsoleHostname, Service: r.ProxyUpstream})
	}, r.ProxyUpstream); err != nil {
		return api.EdgePlan{}, false, err
	}

	token, err := a.api.tunnelToken(ctx, a.config.AccountID, tunnelID)
	if err != nil {
		return api.EdgePlan{}, false, err
	}
	return api.EdgePlan{
		Name:  r.Ref,
		Image: a.config.Image,
		// The image's entrypoint is cloudflared; the token is read from the
		// environment rather than argv, where `ps` would show it.
		Args:       []string{"tunnel", "--no-autoupdate", "run"},
		Env:        map[string]secret.Value{"TUNNEL_TOKEN": token},
		ProxyAlias: hostOf(r.ProxyUpstream),
	}, true, nil
}

// --- the tunnel --------------------------------------------------------------

// tunnelName is Pando's own tunnel's name. Derived from the zone, which is
// known at Configure, rather than the adapter's ID, which arrives only with
// the first Edge call — an app routed before that would otherwise have made a
// second tunnel under another name.
func (a *Adapter) tunnelName() string { return "pando-" + a.config.Zone }

// knownTunnel is the tunnel's ID if it exists, without creating one.
func (a *Adapter) knownTunnel(ctx context.Context) (string, error) {
	if a.tunnelID != "" {
		return a.tunnelID, nil
	}
	t, err := a.api.tunnelByName(ctx, a.config.AccountID, a.tunnelName())
	if err != nil || t == nil {
		return "", err
	}
	a.tunnelID = t.ID
	return t.ID, nil
}

// ensureTunnel finds the tunnel, creating Pando's own when none is named.
func (a *Adapter) ensureTunnel(ctx context.Context) (string, error) {
	if a.config.TunnelID != "" {
		if _, err := a.api.tunnel(ctx, a.config.AccountID, a.config.TunnelID); err != nil {
			return "", fmt.Errorf("the tunnel %s: %w", a.config.TunnelID, err)
		}
		return a.config.TunnelID, nil
	}
	id, err := a.knownTunnel(ctx)
	if err != nil || id != "" {
		return id, err
	}
	t, err := a.api.createTunnel(ctx, a.config.AccountID, a.tunnelName())
	if err != nil {
		return "", err
	}
	a.tunnelID = t.ID
	return t.ID, nil
}

// updateIngress reads the ingress, applies change, keeps the catch-all last,
// and writes it only if it changed. catchAll, when given, is what the
// catch-all sends to.
func (a *Adapter) updateIngress(ctx context.Context, tunnelID string, change func([]ingressRule) []ingressRule, catchAll ...string) error {
	cfg, err := a.api.tunnelConfig(ctx, a.config.AccountID, tunnelID)
	if err != nil {
		return err
	}
	before, _ := json.Marshal(cfg.Ingress)

	// The catch-all is Cloudflare's requirement: the last rule matches
	// everything. It is split off, the change applied to the rest, and put
	// back last.
	fallback := ingressRule{Service: "http_status:404"}
	rules := make([]ingressRule, 0, len(cfg.Ingress)+1)
	for _, r := range cfg.Ingress {
		if r.Hostname == "" && r.Path == "" {
			fallback = r
			continue
		}
		rules = append(rules, r)
	}
	rules = change(rules)
	if len(catchAll) > 0 && catchAll[0] != "" {
		fallback = ingressRule{Service: catchAll[0]}
	}
	cfg.Ingress = append(rules, fallback)

	after, _ := json.Marshal(cfg.Ingress)
	if string(before) == string(after) {
		return nil
	}
	return a.api.putTunnelConfig(ctx, a.config.AccountID, tunnelID, cfg)
}

// upsert replaces the rule for the same hostname and path, or adds it ahead
// of any broader rule for that hostname — Cloudflare matches in order, and a
// path rule placed after its hostname's catch-all would never be reached.
func upsert(rules []ingressRule, rule ingressRule) []ingressRule {
	for i := range rules {
		if rules[i].Hostname == rule.Hostname && rules[i].Path == rule.Path {
			rules[i] = rule
			return rules
		}
	}
	if rule.Path != "" {
		for i := range rules {
			if rules[i].Hostname == rule.Hostname && rules[i].Path == "" {
				return append(rules[:i], append([]ingressRule{rule}, rules[i:]...)...)
			}
		}
	}
	return append(rules, rule)
}

// rule is the ingress rule for a route.
func (a *Adapter) rule(r api.RouteRequest) (ingressRule, error) {
	switch r.Mode {
	case spec.RoutingSubdomain:
		host := strings.ToLower(r.Hostname)
		if host == "" {
			return ingressRule{}, errs.New(errs.AdapterFailed, "This app has no hostname to route.")
		}
		if !within(host, a.config.Zone) {
			return ingressRule{}, errs.Newf(errs.AdapterFailed,
				"This app's hostname, %s, is not in the Cloudflare zone %s, so the tunnel cannot serve it.", host, a.config.Zone).
				WithRemedy("Set the app's hostname to one ending in " + a.config.Zone + ".")
		}
		return ingressRule{Hostname: host, Service: r.ProxyUpstream}, nil

	case spec.RoutingPath:
		prefix := r.PathPrefix
		if prefix == "" {
			return ingressRule{}, errs.New(errs.AdapterFailed, "This app has no path to route.")
		}
		if !strings.HasPrefix(prefix, "/") {
			prefix = "/" + prefix
		}
		host := strings.ToLower(r.Hostname)
		if host == "" {
			host = a.config.ConsoleHostname
		}
		// cloudflared matches a path as a regular expression. The prefix is
		// quoted, and anchored so /notes does not also match /notes-archive.
		// It is not stripped here: Pando's proxy strips it and sets
		// X-Forwarded-Prefix (R-167), because only the proxy knows which app
		// the prefix belonged to.
		path := "^" + regexp.QuoteMeta(strings.TrimSuffix(prefix, "/")) + "(/.*)?$"
		return ingressRule{Hostname: host, Path: path, Service: r.ProxyUpstream}, nil

	default:
		return ingressRule{}, errs.Newf(errs.AdapterFailed, "Cloudflare routing does not handle %q addressing.", r.Mode)
	}
}

// --- DNS ---------------------------------------------------------------------

func (a *Adapter) zone(ctx context.Context) (string, error) {
	if a.zoneID != "" {
		return a.zoneID, nil
	}
	z, err := a.api.zoneByName(ctx, a.config.Zone)
	if err != nil {
		return "", err
	}
	a.zoneID = z.ID
	return z.ID, nil
}

// ensureRecord points a hostname at the tunnel. A record Pando did not make is
// never changed: the name is somebody else's, and the error says so.
func (a *Adapter) ensureRecord(ctx context.Context, host, tunnelID string) error {
	zoneID, err := a.zone(ctx)
	if err != nil {
		return err
	}
	want := dnsRecord{
		Type: "CNAME", Name: host, Content: tunnelID + ".cfargotunnel.com",
		Proxied: true, Comment: ManagedComment, TTL: 1,
	}
	existing, err := a.api.recordsNamed(ctx, zoneID, host)
	if err != nil {
		return err
	}
	for _, rec := range existing {
		if rec.Comment != ManagedComment {
			return errs.Newf(errs.AdapterFailed,
				"%s already has a %s record in Cloudflare that Pando did not create, so Pando will not change it.", host, rec.Type).
				WithRemedy("Delete that record in Cloudflare's dashboard if it is no longer used, or give the app a different hostname.")
		}
	}
	if len(existing) == 0 {
		return a.api.createRecord(ctx, zoneID, want)
	}
	rec := existing[0]
	if rec.Type == want.Type && rec.Content == want.Content && rec.Proxied {
		return nil
	}
	// Pando's own record, changed in the dashboard: reset (design 03 §4.5).
	want.ID = rec.ID
	return a.api.updateRecord(ctx, zoneID, want)
}

func (a *Adapter) removeRecord(ctx context.Context, host string) error {
	zoneID, err := a.zone(ctx)
	if err != nil {
		return err
	}
	existing, err := a.api.recordsNamed(ctx, zoneID, host)
	if err != nil {
		return err
	}
	for _, rec := range existing {
		if rec.Comment != ManagedComment {
			continue
		}
		if err := a.api.deleteRecord(ctx, zoneID, rec.ID); err != nil {
			return err
		}
	}
	return nil
}

// --- helpers -----------------------------------------------------------------

// within reports whether host is zone or a name under it.
func within(host, zone string) bool {
	return host == zone || strings.HasSuffix(host, "."+zone)
}

// handleOf and parseHandle carry a rule's identity in the route handle.
func handleOf(r ingressRule) string { return r.Hostname + "\x1f" + r.Path }

func parseHandle(h string) (string, string) {
	host, path, _ := strings.Cut(h, "\x1f")
	return host, path
}

func hostOf(upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

var _ api.RoutingAdapter = (*Adapter)(nil)

// tokenHelp says how to make the one credential this adapter takes. Each
// permission is used, and none is spare:
//
//   - Account, Cloudflare Tunnel, Edit: create the tunnel, write its ingress,
//     and read the token cloudflared runs with.
//   - Zone, DNS, Edit: point each app's hostname at the tunnel.
//   - Zone, Zone, Read: find the zone's ID from its name.
const tokenPermissions = "Account / Cloudflare Tunnel / Edit; Zone / DNS / Edit; Zone / Zone / Read"

const tokenHelp = "In Cloudflare, go to My Profile, API Tokens, Create Token, Create Custom Token. " +
	"Permissions: " + tokenPermissions + ". " +
	"Account Resources: your account. Zone Resources: the zone below. " +
	"Pando uses it to create the tunnel and to point each app's hostname at it."

// tokenRemedy is what a refused or missing token's error says to do.
const tokenRemedy = "Create a custom API token in Cloudflare with these permissions: " + tokenPermissions +
	", for your account and the zone. Set it as this adapter's API token."

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryRouting,
		Kind:        Kind,
		Name:        "Cloudflare Tunnel",
		Description: "Gives apps their own hostnames through a Cloudflare Tunnel. Nothing on this machine needs to be reachable from the internet, and Cloudflare issues the certificates.",
		IDPrefix:    "rte_",
		Fields: []api.Field{
			{Key: "api_token", Label: "API token", Type: "string", Credential: true, Required: true,
				Help: tokenHelp},
			{Key: "account_id", Label: "Account ID", Type: "string", Required: true,
				Help: "Shown on the account's overview page in Cloudflare's dashboard.", Placeholder: "0123456789abcdef0123456789abcdef"},
			{Key: "zone", Label: "Zone", Type: "string", Required: true,
				Help: "Your domain in Cloudflare. Apps are served at <app>.<zone>.", Placeholder: "example.com"},
			{Key: "console_hostname", Label: "Console hostname", Type: "string",
				Help: "Where the console is served, and the hostname apps on a path are served under. Empty is the zone itself.", Placeholder: "pando.example.com"},
			{Key: "tunnel_id", Label: "Existing tunnel", Type: "string",
				Help:    "Leave empty and Pando creates a tunnel of its own. Or give a tunnel's ID to use that one: Pando adds its rules and leaves the rest alone.",
				Default: "A tunnel of Pando's own", Placeholder: "6ff42ae2-765d-4adf-8112-31c55c1551ef", Advanced: true},
			{Key: "image", Label: "cloudflared image", Type: "string", Default: DefaultImage,
				Help: "Set to run a different cloudflared release than this Pando ships with.", Advanced: true},
		},
	}
}
