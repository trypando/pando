package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

const (
	account     = "0123456789abcdef0123456789abcdef"
	zoneID      = "zone-1"
	otherZoneID = "zone-2"
	upstream    = "http://pando:8080"
)

// fakeCloudflare is the part of Cloudflare's API the adapter uses, in memory.
type fakeCloudflare struct {
	t       *testing.T
	mu      sync.Mutex
	tunnels map[string]string // id -> name
	config  map[string]json.RawMessage
	records map[string]dnsRecord
	inZone  map[string]string // record id -> zone id
	nextID  int
	refuse  bool
	puts    int
}

func newFake(t *testing.T) (*fakeCloudflare, *httptest.Server) {
	f := &fakeCloudflare{t: t, tunnels: map[string]string{}, config: map[string]json.RawMessage{}, records: map[string]dnsRecord{}, inZone: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeCloudflare) id() string {
	f.nextID++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.nextID)
}

func reply(w http.ResponseWriter, status int, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": status < 300, "errors": []any{}, "result": result})
}

func (f *fakeCloudflare) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(f.t, "Bearer tok-cf", r.Header.Get("Authorization"))
	if f.refuse {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false,
			"errors": []map[string]any{{"code": 10000, "message": "Authentication error"}}})
		return
	}

	p := r.URL.Path
	tunnels := "/accounts/" + account + "/cfd_tunnel"
	switch {
	case r.Method == http.MethodGet && p == "/zones":
		switch name := r.URL.Query().Get("name"); name {
		case "example.com":
			reply(w, 200, []zone{{ID: zoneID, Name: name}})
		case "example.org":
			reply(w, 200, []zone{{ID: otherZoneID, Name: name}})
		default:
			reply(w, 200, []zone{})
		}

	case r.Method == http.MethodGet && p == tunnels:
		var out []tunnel
		for id, name := range f.tunnels {
			if name == r.URL.Query().Get("name") {
				out = append(out, tunnel{ID: id, Name: name})
			}
		}
		reply(w, 200, out)

	case r.Method == http.MethodPost && p == tunnels:
		var body map[string]string
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(f.t, "cloudflare", body["config_src"], "remotely managed, so cloudflared needs no file")
		id := f.id()
		f.tunnels[id] = body["name"]
		reply(w, 200, tunnel{ID: id, Name: body["name"]})

	case strings.HasPrefix(p, tunnels+"/"):
		rest := strings.TrimPrefix(p, tunnels+"/")
		id, sub, _ := strings.Cut(rest, "/")
		if _, ok := f.tunnels[id]; !ok {
			reply(w, 404, nil)
			return
		}
		switch {
		case sub == "" && r.Method == http.MethodGet:
			reply(w, 200, tunnel{ID: id, Name: f.tunnels[id]})
		case sub == "token":
			reply(w, 200, "run-token-"+id)
		case sub == "configurations" && r.Method == http.MethodGet:
			reply(w, 200, map[string]any{"config": f.config[id]})
		case sub == "configurations" && r.Method == http.MethodPut:
			var body struct {
				Config json.RawMessage `json:"config"`
			}
			require.NoError(f.t, json.NewDecoder(r.Body).Decode(&body))
			f.config[id] = body.Config
			f.puts++
			reply(w, 200, nil)
		default:
			reply(w, 404, nil)
		}

	case strings.HasPrefix(p, "/zones/") && strings.Contains(p, "/dns_records"):
		inZone, rest, _ := strings.Cut(strings.TrimPrefix(p, "/zones/"), "/dns_records")
		require.Contains(f.t, []string{zoneID, otherZoneID}, inZone)
		recID := strings.TrimPrefix(rest, "/")
		switch r.Method {
		case http.MethodGet:
			var out []dnsRecord
			for id, rec := range f.records {
				// A record a test put in directly is in the default zone.
				where := f.inZone[id]
				if where == "" {
					where = zoneID
				}
				if rec.Name == r.URL.Query().Get("name") && where == inZone {
					out = append(out, rec)
				}
			}
			reply(w, 200, out)
		case http.MethodPost, http.MethodPut:
			var rec dnsRecord
			require.NoError(f.t, json.NewDecoder(r.Body).Decode(&rec))
			if recID == "" {
				recID = f.id()
			}
			rec.ID = recID
			f.records[recID] = rec
			f.inZone[recID] = inZone
			reply(w, 200, rec)
		case http.MethodDelete:
			delete(f.records, recID)
			reply(w, 200, map[string]string{"id": recID})
		}

	default:
		reply(w, 404, nil)
	}
}

// ingress is the tunnel's current rules.
func (f *fakeCloudflare) ingress(t *testing.T, tunnelID string) []ingressRule {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var cfg struct {
		Ingress []ingressRule `json:"ingress"`
	}
	if raw := f.config[tunnelID]; len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, &cfg))
	}
	return cfg.Ingress
}

func (f *fakeCloudflare) record(name string) (dnsRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.records {
		if r.Name == name {
			return r, true
		}
	}
	return dnsRecord{}, false
}

// zoneOfRecord is the zone a record was written in.
func (f *fakeCloudflare) zoneOfRecord(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, r := range f.records {
		if r.Name == name {
			return f.inZone[id]
		}
	}
	return ""
}

func (f *fakeCloudflare) onlyTunnel(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.tunnels, 1)
	for id := range f.tunnels {
		return id
	}
	return ""
}

func configured(t *testing.T, srv *httptest.Server, extra string) *Adapter {
	t.Helper()
	raw := `{"account_id":"` + account + `","zone":"example.com","api_base":"` + srv.URL + `",
		"credentials":{"api_token":"tok-cf"}` + extra + `}`
	a := New()
	require.NoError(t, a.Configure(context.Background(), json.RawMessage(raw)))
	return a
}

func subdomain(appID, host string) api.RouteRequest {
	return api.RouteRequest{AppID: appID, Mode: spec.RoutingSubdomain, Hostname: host, Port: 9001, ProxyUpstream: upstream}
}

// TestR023_TheTunnelSendsEveryAppToPando is the assertion this adapter exists
// to satisfy: the rule names Pando's proxy, never the app's container.
func TestR023_TheTunnelSendsEveryAppToPando(t *testing.T) {
	f, srv := newFake(t)
	a := configured(t, srv, "")

	_, err := a.Ensure(context.Background(), subdomain("app_01", "notes.example.com"))
	require.NoError(t, err)

	rules := f.ingress(t, f.onlyTunnel(t))
	require.Equal(t, "notes.example.com", rules[0].Hostname)
	require.Equal(t, upstream, rules[0].Service)
	for _, r := range rules {
		require.NotContains(t, r.Service, "9001", "the workload's port must never be a destination")
	}
	require.Equal(t, "", rules[len(rules)-1].Hostname, "Cloudflare requires the last rule to match everything")
}

// TestR174_PandoCreatesTheTunnelAndRunsCloudflared asserts R-174 for the
// tunnel: one tunnel, whoever gets there first, and a cloudflared that dials
// out with the token in its environment and publishes nothing.
func TestR174_PandoCreatesTheTunnelAndRunsCloudflared(t *testing.T) {
	f, srv := newFake(t)
	a := configured(t, srv, `,"console_hostname":"pando.example.com"`)

	// An app routed before the edge is started must not make a second tunnel.
	_, err := a.Ensure(context.Background(), subdomain("app_01", "notes.example.com"))
	require.NoError(t, err)
	plan, needs, err := a.Edge(context.Background(), api.EdgeRequest{Ref: "rte_cf", ProxyUpstream: upstream})
	require.NoError(t, err)
	require.True(t, needs)

	id := f.onlyTunnel(t)
	require.Equal(t, "pando-example.com", f.tunnels[id])
	require.Equal(t, "run-token-"+id, plan.Env["TUNNEL_TOKEN"].Reveal())
	require.NotContains(t, strings.Join(plan.Args, " "), "run-token", "a token in argv is visible to ps")
	require.Empty(t, plan.Ports, "the tunnel dials out; nothing is published")
	require.Empty(t, plan.Mounts)
	require.Equal(t, "pando", plan.ProxyAlias)
	require.NotContains(t, fmt.Sprintf("%v %+v", plan, plan), "run-token", "R-194")

	rules := f.ingress(t, id)
	require.Equal(t, upstream, rules[len(rules)-1].Service,
		"any other hostname sent to the tunnel reaches Pando, which answers with the console")
	_, ok := f.record("pando.example.com")
	require.True(t, ok, "the console hostname points at the tunnel")
}

// TestR148_ARuleChangedInTheDashboardIsResetByTheNextEnsure asserts that
// Observe reports drift without fixing it (design 05), and Ensure fixes it.
func TestR148_ARuleChangedInTheDashboardIsResetByTheNextEnsure(t *testing.T) {
	f, srv := newFake(t)
	a := configured(t, srv, "")
	h, err := a.Ensure(context.Background(), subdomain("app_01", "notes.example.com"))
	require.NoError(t, err)

	id := f.onlyTunnel(t)
	f.mu.Lock()
	f.config[id] = json.RawMessage(`{"ingress":[{"hostname":"notes.example.com","service":"http://10.0.0.5:3000"},{"service":"http_status:404"}]}`)
	f.mu.Unlock()

	state, err := a.Observe(context.Background(), h)
	require.NoError(t, err)
	require.True(t, state.Present)
	require.Equal(t, "http://10.0.0.5:3000", state.Address, "reported as found")
	require.Equal(t, "http://10.0.0.5:3000", f.ingress(t, id)[0].Service, "and not fixed by looking")

	_, err = a.Ensure(context.Background(), subdomain("app_01", "notes.example.com"))
	require.NoError(t, err)
	require.Equal(t, upstream, f.ingress(t, id)[0].Service)
}

func TestEnsuringAnUnchangedRouteWritesNothing(t *testing.T) {
	f, srv := newFake(t)
	a := configured(t, srv, "")
	for range 3 {
		_, err := a.Ensure(context.Background(), subdomain("app_01", "notes.example.com"))
		require.NoError(t, err)
	}
	require.Equal(t, 1, f.puts)
}

// TestADNSRecordPandoDidNotCreateIsNeverChanged: the comment is how Pando
// knows a record is its own.
func TestADNSRecordPandoDidNotCreateIsNeverChanged(t *testing.T) {
	f, srv := newFake(t)
	f.records["r1"] = dnsRecord{ID: "r1", Type: "A", Name: "notes.example.com", Content: "203.0.113.7"}
	a := configured(t, srv, "")

	_, err := a.Ensure(context.Background(), subdomain("app_01", "notes.example.com"))
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "did not create")
	require.NotEmpty(t, e.Remedy)
	require.Equal(t, "203.0.113.7", f.records["r1"].Content)
}

func TestPandosOwnRecordChangedInTheDashboardIsReset(t *testing.T) {
	f, srv := newFake(t)
	a := configured(t, srv, "")
	_, err := a.Ensure(context.Background(), subdomain("app_01", "notes.example.com"))
	require.NoError(t, err)

	rec, _ := f.record("notes.example.com")
	require.Equal(t, ManagedComment, rec.Comment)
	require.True(t, rec.Proxied)
	f.mu.Lock()
	rec.Content = "somewhere-else.example.net"
	f.records[rec.ID] = rec
	f.mu.Unlock()

	_, err = a.Ensure(context.Background(), subdomain("app_01", "notes.example.com"))
	require.NoError(t, err)
	rec, _ = f.record("notes.example.com")
	require.Equal(t, f.onlyTunnel(t)+".cfargotunnel.com", rec.Content)
}

// TestAttachingToAnExistingTunnelLeavesItsOtherRulesAlone covers the option
// of using a tunnel Pando did not make.
func TestAttachingToAnExistingTunnelLeavesItsOtherRulesAlone(t *testing.T) {
	f, srv := newFake(t)
	existing := "6ff42ae2-765d-4adf-8112-31c55c1551ef"
	f.tunnels[existing] = "home-lab"
	f.config[existing] = json.RawMessage(`{"ingress":[{"hostname":"nas.example.com","service":"http://192.168.1.10:5000","originRequest":{"noTLSVerify":true}},{"service":"http_status:404"}],"warp-routing":{"enabled":true}}`)
	a := configured(t, srv, `,"tunnel_id":"`+existing+`"`)

	h, err := a.Ensure(context.Background(), subdomain("app_01", "notes.example.com"))
	require.NoError(t, err)
	require.Len(t, f.tunnels, 1, "no tunnel of Pando's own")

	rules := f.ingress(t, existing)
	require.Equal(t, "nas.example.com", rules[0].Hostname)
	require.JSONEq(t, `{"noTLSVerify":true}`, string(rules[0].Extra["originRequest"]), "kept as it was")
	require.Contains(t, string(f.config[existing]), "warp-routing")

	require.NoError(t, a.Remove(context.Background(), h))
	rules = f.ingress(t, existing)
	require.Len(t, rules, 2)
	require.Equal(t, "nas.example.com", rules[0].Hostname)
	_, ok := f.record("notes.example.com")
	require.False(t, ok, "Pando's record goes with its route")
}

// TestR167_PathRoutesAreAnchoredAndAheadOfTheirHostname asserts R-167's other
// half: the prefix is matched, not stripped here (the proxy strips it), and a
// path rule is not shadowed by its hostname's broader rule.
func TestR167_PathRoutesAreAnchoredAndAheadOfTheirHostname(t *testing.T) {
	f, srv := newFake(t)
	a := configured(t, srv, `,"console_hostname":"pando.example.com"`)
	_, _, err := a.Edge(context.Background(), api.EdgeRequest{Ref: "rte_cf", ProxyUpstream: upstream})
	require.NoError(t, err)

	_, err = a.Ensure(context.Background(), api.RouteRequest{
		AppID: "app_02", Mode: spec.RoutingPath, PathPrefix: "/notes", ProxyUpstream: upstream,
	})
	require.NoError(t, err)

	rules := f.ingress(t, f.onlyTunnel(t))
	require.Equal(t, "pando.example.com", rules[0].Hostname)
	require.Equal(t, `^/notes(/.*)?$`, rules[0].Path)
	require.Equal(t, "pando.example.com", rules[1].Hostname)
	require.Empty(t, rules[1].Path, "the console's own rule comes after the path it would shadow")
}

func TestAHostnameOutsideTheZoneIsRefusedWithTheFix(t *testing.T) {
	_, srv := newFake(t)
	a := configured(t, srv, "")
	_, err := a.Ensure(context.Background(), subdomain("app_01", "notes.other.org"))
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "example.com")
	require.Contains(t, e.Remedy, "ending in example.com")
}

// TestR162_AppsAreNamedOneLevelBelowTheZone: the zone is the base domain, so
// every app is where Cloudflare's included certificate reaches, and the
// console may sit anywhere in the zone.
func TestR162_AppsAreNamedOneLevelBelowTheZone(t *testing.T) {
	_, srv := newFake(t)
	caps, err := configured(t, srv, `,"console_hostname":"pando.example.com"`).Capabilities(context.Background())
	require.NoError(t, err)
	require.Equal(t, "example.com", caps.BaseDomain)
	require.True(t, caps.SupportsTLS)
	require.False(t, caps.RequiresPublicReachability)
	require.Equal(t, spec.RoutingSubdomain, caps.DefaultMode)
}

// TestR190_ATokenStoredInTheClearIsRefused asserts R-190.
func TestR190_ATokenStoredInTheClearIsRefused(t *testing.T) {
	err := New().Configure(context.Background(), json.RawMessage(
		`{"account_id":"`+account+`","zone":"example.com","api_token":"tok-cf"}`))
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "not encrypted")
	require.NotContains(t, e.Message, "tok-cf")
}

func TestConfigurationMistakesAreNamed(t *testing.T) {
	cases := map[string]string{
		`{"account_id":"` + account + `","zone":"example.com"}`:                                                                      "API token",
		`{"account_id":"nope","zone":"example.com","credentials":{"api_token":"t"}}`:                                                 "account ID",
		`{"account_id":"` + account + `","zone":"example.com","tunnel_id":"x","credentials":{"api_token":"t"}}`:                      "tunnel ID",
		`{"account_id":"` + account + `","zone":"example.com","console_hostname":"pando.other.org","credentials":{"api_token":"t"}}`: "not in the Cloudflare zone",
	}
	for raw, want := range cases {
		err := New().Configure(context.Background(), json.RawMessage(raw))
		require.Error(t, err, raw)
		require.Contains(t, err.Error(), want, raw)
	}
}

// TestR105_ARefusedTokenSaysWhichPermissionsItNeeds asserts R-105.
func TestR105_ARefusedTokenSaysWhichPermissionsItNeeds(t *testing.T) {
	f, srv := newFake(t)
	f.refuse = true
	err := configured(t, srv, "").HealthCheck(context.Background())
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "Authentication error")
	require.Contains(t, e.Remedy, "Account / Cloudflare Tunnel / Edit; Zone / DNS / Edit; Zone / Zone / Read")
	require.NotContains(t, e.Message+e.Remedy, "tok-cf")
}

// TestR162_AppsCanBeNamedInOtherZones: the zone stays the default a new app is
// named in, and a hostname in another configured zone is served by the same
// tunnel, with its DNS record written in that zone.
func TestR162_AppsCanBeNamedInOtherZones(t *testing.T) {
	f, srv := newFake(t)
	a := configured(t, srv, `,"other_zones":" Example.org. , example.com"`)
	ctx := context.Background()

	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.Equal(t, "example.com", caps.BaseDomain, "the default zone is still where new apps go")
	require.Equal(t, []string{"example.com", "example.org"}, caps.Domains, "normalized, the default first, no repeats")
	require.NoError(t, a.HealthCheck(ctx))

	_, err = a.Ensure(ctx, subdomain("app_01", "notes.example.com"))
	require.NoError(t, err)
	h, err := a.Ensure(ctx, subdomain("app_02", "docs.example.org"))
	require.NoError(t, err)

	require.Equal(t, zoneID, f.zoneOfRecord("notes.example.com"))
	require.Equal(t, otherZoneID, f.zoneOfRecord("docs.example.org"))
	rules := f.ingress(t, f.onlyTunnel(t))
	require.Equal(t, "docs.example.org", rules[1].Hostname)
	require.Equal(t, upstream, rules[1].Service, "R-023: every zone's apps go to Pando's proxy")

	require.NoError(t, a.Remove(ctx, h))
	_, ok := f.record("docs.example.org")
	require.False(t, ok, "removed from the zone it was written in")

	_, err = a.Ensure(ctx, subdomain("app_03", "notes.other.net"))
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "example.com or example.org")
	require.Contains(t, e.Remedy, "other zones")
}

func TestTheConsoleCanBeInAnotherZone(t *testing.T) {
	_, srv := newFake(t)
	a := configured(t, srv, `,"other_zones":"example.org","console_hostname":"pando.example.org"`)
	_, ok, err := a.Edge(context.Background(), api.EdgeRequest{Ref: "rte_cf", ProxyUpstream: upstream})
	require.NoError(t, err)
	require.True(t, ok)
}

func TestAnOtherZoneThatIsNotADomainIsNamed(t *testing.T) {
	err := New().Configure(context.Background(), json.RawMessage(
		`{"account_id":"`+account+`","zone":"example.com","other_zones":"example.org, localhost","credentials":{"api_token":"t"}}`))
	require.ErrorContains(t, err, `"localhost" in other zones`)
}

func TestAnOtherZoneTheTokenCannotSeeFailsTheHealthCheck(t *testing.T) {
	_, srv := newFake(t)
	err := configured(t, srv, `,"other_zones":"example.net"`).HealthCheck(context.Background())
	require.ErrorContains(t, err, "example.net")
}

func TestARecordOutsideEveryZoneIsRefused(t *testing.T) {
	_, srv := newFake(t)
	a := configured(t, srv, "")
	err := a.ensureRecord(context.Background(), "x.other.net", "t")
	require.ErrorContains(t, err, "is not in the Cloudflare zone example.com")
}
