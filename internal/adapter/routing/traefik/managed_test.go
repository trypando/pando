package traefik_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/routing/traefik"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

var edgeReq = api.EdgeRequest{Ref: "rte_traefik", ProxyUpstream: "http://pando:8080"}

func hasArg(plan api.EdgePlan, prefix string) bool {
	for _, a := range plan.Args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func configureErr(t *testing.T, cfg string) *errs.Error {
	t.Helper()
	dir := t.TempDir()
	err := traefik.New().Configure(context.Background(), []byte(strings.Replace(cfg, "DIR", dir, 1)))
	require.Error(t, err)
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	return e
}

// TestR174_ByDefaultPandoRunsTraefik asserts R-174: a Traefik adapter with no
// other settings is one Pando runs — on port 80, reading the directory Pando
// writes routes to, sending everything to Pando's proxy.
func TestR174_ByDefaultPandoRunsTraefik(t *testing.T) {
	a, dir := adapter(t, "")

	plan, needs, err := a.Edge(context.Background(), edgeReq)
	require.NoError(t, err)
	require.True(t, needs)
	require.Equal(t, "rte_traefik", plan.Name)
	require.Equal(t, traefik.DefaultImage, plan.Image)
	require.Equal(t, "pando", plan.ProxyAlias)
	require.Equal(t, []api.EdgePort{{Host: 80, Container: 80}}, plan.Ports,
		"no certificates configured, so no port 443 either")
	require.Equal(t, dir, plan.Mounts[0].SharedWithPando)
	require.True(t, plan.Mounts[0].ReadOnly, "the edge reads routes; it never writes them")
	require.False(t, hasArg(plan, "--providers.docker"), "labels on a workload would be two adapters owning one thing")
	require.False(t, hasArg(plan, "--certificatesresolvers"))
}

// TestR023_EveryOtherHostnameReachesPandoNotAnApp asserts the console route:
// a hostname that is not an app's reaches Pando's proxy, which answers it with
// the console — and it loses to any app's own route.
func TestR023_EveryOtherHostnameReachesPandoNotAnApp(t *testing.T) {
	a, dir := adapter(t, `{"dir":"DIR","console_hostname":"pando.example.com","certificates":"http","acme_email":"ops@example.com"}`)

	_, _, err := a.Edge(context.Background(), edgeReq)
	require.NoError(t, err)

	body, err := os.ReadFile(filepath.Join(dir, "pando-console.yml"))
	require.NoError(t, err)
	require.Contains(t, string(body), `- url: "http://pando:8080"`)
	require.Contains(t, string(body), "Host(`pando.example.com`)")
	require.Contains(t, string(body), "priority: 1")
	require.Contains(t, string(body), "certResolver: pando", "the console hostname gets a certificate")
}

// TestR169_HTTPCertificatesArePerHostname asserts R-169's HTTP-01 choice.
func TestR169_HTTPCertificatesArePerHostname(t *testing.T) {
	a, dir := adapter(t, `{"dir":"DIR","certificates":"http","acme_email":"ops@example.com"}`)

	plan, _, err := a.Edge(context.Background(), edgeReq)
	require.NoError(t, err)
	require.True(t, hasArg(plan, "--certificatesresolvers.pando.acme.httpchallenge.entrypoint=web"))
	require.True(t, hasArg(plan, "--certificatesresolvers.pando.acme.email=ops@example.com"))
	require.Contains(t, plan.Ports, api.EdgePort{Host: 443, Container: 443})
	require.Contains(t, plan.Mounts, api.EdgeMount{Path: "/acme", Volume: "acme"},
		"certificates outlive the container")

	caps, err := a.Capabilities(context.Background())
	require.NoError(t, err)
	require.True(t, caps.SupportsTLS)
	require.False(t, caps.SupportsWildcardTLS)

	_, err = a.Ensure(context.Background(), api.RouteRequest{
		AppID: "app_01", Mode: spec.RoutingSubdomain, Hostname: "notes.example.com",
		ProxyUpstream: "http://pando:8080", TLS: api.TLSRequest{Enabled: true},
	})
	require.NoError(t, err)
	route, err := os.ReadFile(filepath.Join(dir, "pando-app_01.yml"))
	require.NoError(t, err)
	require.Contains(t, string(route), "- websecure")
	require.Contains(t, string(route), "certResolver: pando")
	require.NotContains(t, string(route), "domains:")
}

// TestR166_DNSCertificatesAreOneWildcard asserts R-166 and R-169's DNS-01
// choice: one certificate for *.base, covering every app one level below it.
func TestR166_DNSCertificatesAreOneWildcard(t *testing.T) {
	a, dir := adapter(t, `{"dir":"DIR","certificates":"dns","acme_email":"ops@example.com",
		"base_domain":"apps.example.com","dns_provider":"cloudflare",
		"credentials":{"dns_credentials":"# scoped to one zone\nCF_DNS_API_TOKEN=tok-abc\n"}}`)

	plan, _, err := a.Edge(context.Background(), edgeReq)
	require.NoError(t, err)
	require.True(t, hasArg(plan, "--certificatesresolvers.pando.acme.dnschallenge.provider=cloudflare"))
	require.Equal(t, "tok-abc", plan.Env["CF_DNS_API_TOKEN"].Reveal())

	caps, err := a.Capabilities(context.Background())
	require.NoError(t, err)
	require.True(t, caps.SupportsWildcardTLS)

	ensure := func(appID, host string) string {
		_, err := a.Ensure(context.Background(), api.RouteRequest{
			AppID: appID, Mode: spec.RoutingSubdomain, Hostname: host,
			ProxyUpstream: "http://pando:8080", TLS: api.TLSRequest{Enabled: true},
		})
		require.NoError(t, err)
		body, err := os.ReadFile(filepath.Join(dir, "pando-"+appID+".yml"))
		require.NoError(t, err)
		return string(body)
	}
	require.Contains(t, ensure("app_01", "notes.apps.example.com"), `sans:
              - "*.apps.example.com"`)
	require.NotContains(t, ensure("app_02", "a.b.apps.example.com"), "domains:",
		"a wildcard covers one level; a deeper name gets its own certificate")
}

// TestR194_DNSCredentialsNeverRender asserts R-194 for the edge's
// environment: printing the plan shows no credential.
func TestR194_DNSCredentialsNeverRender(t *testing.T) {
	a, _ := adapter(t, `{"dir":"DIR","certificates":"dns","acme_email":"ops@example.com",
		"base_domain":"apps.example.com","dns_provider":"digitalocean",
		"credentials":{"dns_credentials":"DO_AUTH_TOKEN=do-secret-1"}}`)
	plan, _, err := a.Edge(context.Background(), edgeReq)
	require.NoError(t, err)
	require.NotContains(t, fmt.Sprintf("%v %+v", plan, plan), "do-secret-1")
}

// TestR105_AMissingDNSCredentialIsNamed asserts R-105: the message names what
// is missing and how to give it.
func TestR105_AMissingDNSCredentialIsNamed(t *testing.T) {
	e := configureErr(t, `{"dir":"DIR","certificates":"dns","acme_email":"ops@example.com",
		"base_domain":"apps.example.com","dns_provider":"route53",
		"credentials":{"dns_credentials":"AWS_ACCESS_KEY_ID=AKIA"}}`)
	require.Contains(t, e.Message, "AWS_SECRET_ACCESS_KEY and AWS_REGION are not there")
	require.Contains(t, e.Remedy, "NAME=value")
}

// TestR169_ARoute53ConfigurationWithoutARegionIsRefused asserts R-169: lego
// cannot reach Route 53 without AWS_REGION, so a configuration missing it is
// refused when it is saved, not when the first certificate fails to issue.
func TestR169_ARoute53ConfigurationWithoutARegionIsRefused(t *testing.T) {
	const route53 = `{"dir":"DIR","certificates":"dns","acme_email":"ops@example.com",
		"base_domain":"apps.example.com","dns_provider":"route53",
		"credentials":{"dns_credentials":"AWS_ACCESS_KEY_ID=AKIA\nAWS_SECRET_ACCESS_KEY=s%s"}}`

	e := configureErr(t, fmt.Sprintf(route53, ""))
	require.Contains(t, e.Message, "Amazon Route 53 needs AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_REGION")
	require.Contains(t, e.Message, "AWS_REGION is not there")
	require.Contains(t, e.Remedy, "us-east-1")

	e = configureErr(t, fmt.Sprintf(route53, `\nAWS_REGION=`))
	require.Contains(t, e.Message, "AWS_REGION is not there", "an empty value is not a region")

	a, _ := adapter(t, fmt.Sprintf(route53, `\nAWS_REGION=us-east-1`))
	plan, _, err := a.Edge(context.Background(), edgeReq)
	require.NoError(t, err)
	require.Equal(t, "us-east-1", plan.Env["AWS_REGION"].Reveal())
}

func TestAProviderWithNoCredentialsSaysNoneAreThere(t *testing.T) {
	e := configureErr(t, `{"dir":"DIR","certificates":"dns","acme_email":"ops@example.com",
		"base_domain":"apps.example.com","dns_provider":"cloudflare"}`)
	require.Contains(t, e.Message, "CF_DNS_API_TOKEN, or CF_API_EMAIL and CF_API_KEY")
	require.Contains(t, e.Message, "none of them is there")
	require.Equal(t, "Enter each as a line of the form NAME=value in the DNS provider credentials.", e.Remedy)
}

func TestTheCredentialsHelpNamesTheRoute53Region(t *testing.T) {
	for _, f := range traefik.Info().Fields {
		if f.Key == "dns_credentials" {
			require.Contains(t, f.Help, "Amazon Route 53: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_REGION (AWS_REGION can be us-east-1")
			return
		}
	}
	t.Fatal("no dns_credentials field")
}

func TestAMalformedCredentialLineIsNamedByNumberNeverQuoted(t *testing.T) {
	e := configureErr(t, `{"dir":"DIR","certificates":"dns","acme_email":"ops@example.com",
		"base_domain":"apps.example.com","dns_provider":"cloudflare",
		"credentials":{"dns_credentials":"CF_DNS_API_TOKEN=ok\nhunter2-no-name"}}`)
	require.Contains(t, e.Message, "Line 2")
	require.NotContains(t, e.Message, "hunter2")
}

// TestAnyOtherDNSProviderTraefikKnowsIsAccepted: "Other" takes any provider
// code with whatever variables it is given, and Traefik is the judge.
func TestAnyOtherDNSProviderTraefikKnowsIsAccepted(t *testing.T) {
	a, _ := adapter(t, `{"dir":"DIR","certificates":"dns","acme_email":"ops@example.com",
		"base_domain":"apps.example.com","dns_provider":"ovh",
		"credentials":{"dns_credentials":"OVH_ENDPOINT=ovh-eu\nOVH_APPLICATION_KEY=k"}}`)
	plan, _, err := a.Edge(context.Background(), edgeReq)
	require.NoError(t, err)
	require.True(t, hasArg(plan, "--certificatesresolvers.pando.acme.dnschallenge.provider=ovh"))
	require.Equal(t, "ovh-eu", plan.Env["OVH_ENDPOINT"].Reveal())
}

// TestR169_NeitherChallengeIsASilentDefault asserts R-169: asking for
// certificates without what they need is refused, and an unknown answer is
// refused with the valid ones.
func TestR169_NeitherChallengeIsASilentDefault(t *testing.T) {
	e := configureErr(t, `{"dir":"DIR","certificates":"http"}`)
	require.Contains(t, e.Message, "email")

	e = configureErr(t, `{"dir":"DIR","certificates":"dns","acme_email":"ops@example.com","dns_provider":"cloudflare"}`)
	require.Contains(t, e.Message, "base domain")

	e = configureErr(t, `{"dir":"DIR","certificates":"yes"}`)
	require.Contains(t, e.Message, "Valid answers")

	a, _ := adapter(t, "")
	caps, err := a.Capabilities(context.Background())
	require.NoError(t, err)
	require.False(t, caps.SupportsTLS, "no challenge chosen means no TLS claimed")
}

// TestR174_ATraefikSomebodyElseRunsHasNoEdge: managed off is the mode that
// existed before R-174 — routes only, and nothing of Pando's in front.
func TestR174_ATraefikSomebodyElseRunsHasNoEdge(t *testing.T) {
	a, dir := adapter(t, "")
	_, _, err := a.Edge(context.Background(), edgeReq)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dir, "pando-console.yml"))

	external := traefik.New()
	require.NoError(t, external.Configure(context.Background(), []byte(`{"dir":"`+dir+`","managed":false}`)))
	_, needs, err := external.Edge(context.Background(), edgeReq)
	require.NoError(t, err)
	require.False(t, needs)
	require.NoFileExists(t, filepath.Join(dir, "pando-console.yml"),
		"another Traefik's other hostnames are not Pando's to answer")
}

// TestTheBundledTraefikIsPinnedToARelease: a minor tag such as v3.7 moves
// under an install between two starts; a patch release does not.
func TestTheBundledTraefikIsPinnedToARelease(t *testing.T) {
	require.Regexp(t, `^traefik:v3\.7\.\d+$`, traefik.DefaultImage)
}

func TestTheTraefikImageCanBeOverridden(t *testing.T) {
	a, _ := adapter(t, `{"dir":"DIR","image":"traefik:v3.4","http_port":8081}`)
	plan, _, err := a.Edge(context.Background(), edgeReq)
	require.NoError(t, err)
	require.Equal(t, "traefik:v3.4", plan.Image)
	require.Equal(t, 8081, plan.Ports[0].Host)
}
