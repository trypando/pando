package traefik_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

func ensureTLS(t *testing.T, a interface {
	Ensure(context.Context, api.RouteRequest) (api.RouteHandle, error)
}, dir, appID, host string) string {
	t.Helper()
	_, err := a.Ensure(context.Background(), api.RouteRequest{
		AppID: appID, Mode: spec.RoutingSubdomain, Hostname: host,
		ProxyUpstream: "http://pando:8080", TLS: api.TLSRequest{Enabled: true},
	})
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(dir, "pando-"+appID+".yml"))
	require.NoError(t, err)
	return string(body)
}

// TestR417_InAClusterEveryTraefikServesTheCertificatesPandoIssues asserts
// O-68's answer for a Traefik Pando runs: once the edge is one of several, it
// orders nothing itself — no resolver, no acme volume — but asks Pando to
// issue a certificate for the console and every app hostname, reads them
// from the directory each control host's runtime writes them to, sends the
// CA's HTTP-01 request to Pando's proxy ahead of the redirect to HTTPS, and
// routes app hostnames with whatever certificate the store holds.
func TestR417_InAClusterEveryTraefikServesTheCertificatesPandoIssues(t *testing.T) {
	a, dir := adapter(t, `{"dir":"DIR","console_hostname":"pando.example.com","certificates":"http","acme_email":"ops@example.com","https_port":8443}`)
	route := ensureTLS(t, a, dir, "app_01", "Notes.example.com")
	require.Contains(t, route, "certResolver: pando", "one host: Traefik orders its own")

	clustered := edgeReq
	clustered.Clustered = true
	plan, needs, err := a.Edge(context.Background(), clustered)
	require.NoError(t, err)
	require.True(t, needs)
	require.False(t, hasArg(plan, "--certificatesresolvers"), "Traefik orders nothing")
	require.False(t, hasArg(plan, "--entrypoints.web.http.redirections"), "the redirect would swallow the challenge")
	require.True(t, hasArg(plan, "--entrypoints.websecure.address=:443"))
	for _, m := range plan.Mounts {
		require.Empty(t, m.Volume, "no acme volume: nothing is kept per edge")
	}
	require.Equal(t, dir, plan.CertificateDir)
	require.NotNil(t, plan.Issue)
	require.Equal(t, api.ChallengeHTTP01, plan.Issue.Challenge)
	require.Equal(t, "ops@example.com", plan.Issue.Email)
	require.Equal(t, []api.CertificateOrder{
		{Name: "pando-tls-notes.example.com", Domains: []string{"notes.example.com"}},
		{Name: "pando-tls-pando.example.com", Domains: []string{"pando.example.com"}},
	}, plan.Issue.Orders)

	list, err := os.ReadFile(filepath.Join(dir, "pando-certificates.yml"))
	require.NoError(t, err)
	require.Contains(t, string(list), "certFile: /etc/traefik/dynamic/pando-tls-notes.example.com.crt")
	require.Contains(t, string(list), "keyFile: /etc/traefik/dynamic/pando-tls-pando.example.com.key")

	console, err := os.ReadFile(filepath.Join(dir, "pando-console.yml"))
	require.NoError(t, err)
	require.Contains(t, string(console), "PathPrefix(`/.well-known/acme-challenge/`)")
	require.Contains(t, string(console), "- pando-redirect-https")
	require.Contains(t, string(console), `port: "8443"`)
	require.NotContains(t, string(console), "certResolver")

	// The route rewritten by the next pass serves the store's certificate.
	route = ensureTLS(t, a, dir, "app_01", "notes.example.com")
	require.Contains(t, route, "tls: {}")
	require.NotContains(t, route, "certResolver")

	// Back to one host: Traefik orders its own again, and the list goes.
	plan, _, err = a.Edge(context.Background(), edgeReq)
	require.NoError(t, err)
	require.Nil(t, plan.Issue)
	require.Empty(t, plan.CertificateDir)
	require.Contains(t, plan.Mounts, api.EdgeMount{Path: "/acme", Volume: "acme"})
	_, err = os.Stat(filepath.Join(dir, "pando-certificates.yml"))
	require.True(t, os.IsNotExist(err))
}

// TestR417_InAClusterAWildcardIsIssuedOnceForEveryEdge asserts DNS-01 in a
// cluster: Pando orders the base domain's wildcard through the DNS provider,
// once, and a hostname outside it gets one of its own.
func TestR417_InAClusterAWildcardIsIssuedOnceForEveryEdge(t *testing.T) {
	a, dir := adapter(t, `{"dir":"DIR","certificates":"dns","acme_email":"ops@example.com",
		"base_domain":"apps.example.com","dns_provider":"cloudflare",
		"credentials":{"dns_credentials":"CF_DNS_API_TOKEN=tok-abc"}}`)
	clustered := edgeReq
	clustered.Clustered = true
	_, _, err := a.Edge(context.Background(), clustered)
	require.NoError(t, err)
	ensureTLS(t, a, dir, "app_01", "notes.apps.example.com")
	ensureTLS(t, a, dir, "app_02", "shop.example.org")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pando-app_03.yml.tmp"), []byte("# pando-tls-hostname: x.example.org\n"), 0o644))

	plan, _, err := a.Edge(context.Background(), clustered)
	require.NoError(t, err)
	require.Equal(t, api.ChallengeDNS01, plan.Issue.Challenge)
	require.Equal(t, "cloudflare", plan.Issue.DNSProvider)
	require.Equal(t, "tok-abc", plan.Issue.DNSCredentials["CF_DNS_API_TOKEN"].Reveal())
	require.Equal(t, []api.CertificateOrder{
		{Name: "pando-tls-shop.example.org", Domains: []string{"shop.example.org"}},
		{Name: "pando-tls-wildcard", Domains: []string{"apps.example.com", "*.apps.example.com"}},
	}, plan.Issue.Orders)
	require.Empty(t, plan.Env, "the DNS provider's key goes to Pando's issuer, not to every edge")
}

// TestR417_ATraefikThatOrdersItsOwnCertificatesSaysSo asserts the capability
// a second control host's join reads (O-68): only a Traefik somebody else
// runs, with its own resolver, orders certificates Pando cannot order instead.
func TestR417_ATraefikThatOrdersItsOwnCertificatesSaysSo(t *testing.T) {
	for cfg, want := range map[string]bool{
		`{"dir":"DIR","managed":false,"cert_resolver":"le"}`:                 true,
		`{"dir":"DIR","managed":false}`:                                      false,
		`{"dir":"DIR","certificates":"http","acme_email":"ops@example.com"}`: false,
	} {
		a, _ := adapter(t, cfg)
		caps, err := a.Capabilities(context.Background())
		require.NoError(t, err)
		require.Equal(t, want, caps.CertificatesPerEdge, cfg)
	}
}
