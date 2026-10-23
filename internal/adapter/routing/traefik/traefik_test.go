package traefik_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/routing/traefik"
	"github.com/trypando/pando/internal/core/spec"
)

func adapter(t *testing.T, cfg string) (*traefik.Adapter, string) {
	t.Helper()
	dir := t.TempDir()
	if cfg == "" {
		cfg = `{"dir":"` + dir + `"}`
	} else {
		cfg = strings.Replace(cfg, "DIR", dir, 1)
	}
	a := traefik.New()
	require.NoError(t, a.Configure(context.Background(), json.RawMessage(cfg)))
	return a, dir
}

// TestR023_TraefikPointsAtPandoNeverAtTheWorkload is the assertion this whole
// adapter exists to satisfy.
//
// An adapter author's instinct is to point the edge straight at the container —
// it is simpler, it is what every Traefik tutorial shows, and it works. It is
// also a path to an app that skips authorization entirely, because Pando's
// proxy is the single enforcement point (R-023). The route must name Pando.
func TestR023_TraefikPointsAtPandoNeverAtTheWorkload(t *testing.T) {
	a, dir := adapter(t, "")

	_, err := a.Ensure(context.Background(), api.RouteRequest{
		AppID:         "app_01HQ8",
		Mode:          spec.RoutingSubdomain,
		Hostname:      "notes.example.com",
		Port:          9001,
		ProxyUpstream: "http://pando:8080",
	})
	require.NoError(t, err)

	body := readOnly(t, dir)
	require.Contains(t, body, `- url: "http://pando:8080"`)

	// The workload's own port is in the request and must not appear in the
	// output. If it does, something is routing around the proxy.
	require.NotContains(t, body, "9001")
	require.NotContains(t, body, "localhost")
}

// TestR023_ARouteWithNoUpstreamIsRefused asserts the adapter does not invent an
// address when Pando fails to supply one.
//
// A default would be this adapter guessing where Pando lives, and a wrong guess
// routes an app to nothing — or to something else.
func TestR023_ARouteWithNoUpstreamIsRefused(t *testing.T) {
	a, _ := adapter(t, "")

	_, err := a.Ensure(context.Background(), api.RouteRequest{
		AppID: "app_01HQ8", Mode: spec.RoutingSubdomain, Hostname: "notes.example.com",
	})
	require.Error(t, err)
}

// TestR167_ThePathPrefixIsNotStrippedAtTheEdge asserts the division of labour
// that R-167 depends on.
//
// Pando's proxy strips the prefix and sets X-Forwarded-Prefix, because it is the
// thing that knows which app the prefix belonged to. An edge that stripped it
// first would hand Pando a path it cannot resolve.
func TestR167_ThePathPrefixIsNotStrippedAtTheEdge(t *testing.T) {
	a, dir := adapter(t, "")

	_, err := a.Ensure(context.Background(), api.RouteRequest{
		AppID: "app_01HQ8", Mode: spec.RoutingPath, PathPrefix: "/notes",
		ProxyUpstream: "http://pando:8080",
	})
	require.NoError(t, err)

	body := readOnly(t, dir)
	require.Contains(t, body, "PathPrefix(`/notes`)")
	require.NotContains(t, strings.ToLower(body), "stripprefix")
}

// TestEnsureIsIdempotent asserts the reconciler can call this on every pass.
//
// The reconciler converges continuously; an adapter that accumulated a file or
// a router per call would fill a directory and then a disk.
func TestEnsureIsIdempotent(t *testing.T) {
	a, dir := adapter(t, "")
	req := api.RouteRequest{
		AppID: "app_01HQ8", Mode: spec.RoutingSubdomain, Hostname: "notes.example.com",
		ProxyUpstream: "http://pando:8080",
	}

	first, err := a.Ensure(context.Background(), req)
	require.NoError(t, err)
	before := readOnly(t, dir)

	second, err := a.Ensure(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, before, readOnly(t, dir))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "one app, one file, however many times it is applied")
}

// TestObserveReportsWithoutRemediating asserts design 05's division: adapters
// report, the reconciler decides.
func TestObserveReportsWithoutRemediating(t *testing.T) {
	a, dir := adapter(t, "")
	req := api.RouteRequest{
		AppID: "app_01HQ8", Mode: spec.RoutingSubdomain, Hostname: "notes.example.com",
		ProxyUpstream: "http://pando:8080",
	}
	handle, err := a.Ensure(context.Background(), req)
	require.NoError(t, err)

	state, err := a.Observe(context.Background(), handle)
	require.NoError(t, err)
	require.True(t, state.Present)
	require.Equal(t, "http://pando:8080", state.Address)

	// Something removed the route behind Pando's back.
	require.NoError(t, os.Remove(handle.Handle))

	state, err = a.Observe(context.Background(), handle)
	require.NoError(t, err)
	require.False(t, state.Present, "Observe reports the drift")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "and does not quietly put it back")
}

// TestR162_CapabilitiesAreHonestAboutTLS asserts the planner gets a true answer.
//
// Claiming TLS without a resolver configured would produce a plan that succeeds
// and an app nobody can reach over HTTPS — which is the failure R-254's
// capabilities-as-data exists to prevent.
func TestR162_CapabilitiesAreHonestAboutTLS(t *testing.T) {
	plain, _ := adapter(t, `{"dir":"DIR"}`)
	caps, err := plain.Capabilities(context.Background())
	require.NoError(t, err)
	require.False(t, caps.SupportsTLS, "no resolver configured, so no TLS claimed")
	require.Equal(t, spec.RoutingSubdomain, caps.DefaultMode)
	require.True(t, caps.RequiresPublicReachability)

	// Somebody else's Traefik, with a resolver it says it has.
	withACME, _ := adapter(t, `{"dir":"DIR","managed":false,"cert_resolver":"letsencrypt"}`)
	caps, err = withACME.Capabilities(context.Background())
	require.NoError(t, err)
	require.True(t, caps.SupportsTLS)
	require.False(t, caps.SupportsWildcardTLS,
		"a wildcard needs DNS-01 credentials, which Pando cannot see another Traefik holding")

	// A resolver name means nothing to the Traefik Pando runs: it has its own.
	ignored, _ := adapter(t, `{"dir":"DIR","cert_resolver":"letsencrypt"}`)
	caps, err = ignored.Capabilities(context.Background())
	require.NoError(t, err)
	require.False(t, caps.SupportsTLS)
}

// TestPortModeIsRefusedRatherThanIgnored — a port-mode app reaches Pando's proxy
// directly and needs no edge router. Writing nothing would leave an app the
// planner believed was routed.
func TestPortModeIsRefusedRatherThanIgnored(t *testing.T) {
	a, dir := adapter(t, "")

	_, err := a.Ensure(context.Background(), api.RouteRequest{
		AppID: "app_01HQ8", Mode: spec.RoutingPort, Port: 9001,
		ProxyUpstream: "http://pando:8080",
	})
	require.Error(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func readOnly(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	body, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	require.NoError(t, err)
	return string(body)
}

// TestR162_AnAddressCannotAddToTheRoutingRule: each value in a Traefik rule
// is quoted in backticks, so an address carrying one would end the quote and
// widen the rule. Refused, and nothing is written.
func TestR162_AnAddressCannotAddToTheRoutingRule(t *testing.T) {
	a, dir := adapter(t, "")
	for _, req := range []api.RouteRequest{
		{Mode: spec.RoutingSubdomain, Hostname: "x`) || HostRegexp(`.+"},
		{Mode: spec.RoutingPath, PathPrefix: "/x`) || PathPrefix(`/"},
	} {
		req.AppID, req.ProxyUpstream = "app_01HQ8", "http://pando:8080"
		_, err := a.Ensure(context.Background(), req)
		require.Error(t, err)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
