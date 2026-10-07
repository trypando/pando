package traefik_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/routing/traefik"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

var (
	ingressRoutes = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}
	middlewares   = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "middlewares"}
)

func kubeAdapter(t *testing.T, cfg string) (*traefik.Adapter, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ingressRoutes: "IngressRouteList", middlewares: "MiddlewareList"})
	t.Cleanup(traefik.UseDynamic(dyn))
	a := traefik.New()
	require.NoError(t, a.Configure(context.Background(), []byte(cfg)))
	return a, dyn
}

func routeServices(t *testing.T, obj *unstructured.Unstructured) []map[string]any {
	t.Helper()
	routes, _, err := unstructured.NestedSlice(obj.Object, "spec", "routes")
	require.NoError(t, err)
	var out []map[string]any
	for _, r := range routes {
		for _, s := range r.(map[string]any)["services"].([]any) {
			out = append(out, s.(map[string]any))
		}
	}
	return out
}

// TestR023_EveryIngressRouteNamesPandosProxy asserts R-023 on Kubernetes:
// an app's IngressRoute and the console's catch-all both send traffic to the
// Service Pando's proxy is reached by, never to an app's Service.
func TestR023_EveryIngressRouteNamesPandosProxy(t *testing.T) {
	ctx := context.Background()
	a, dyn := kubeAdapter(t, `{"delivery":"kubernetes_api","base_domain":"apps.example.com"}`)
	require.NoError(t, a.HealthCheck(ctx))

	h, err := a.Ensure(ctx, api.RouteRequest{
		AppID: "app_01HQ8", Mode: spec.RoutingSubdomain, Hostname: "web.apps.example.com",
		ProxyUpstream: "http://pando-proxy:8080",
	})
	require.NoError(t, err)

	plan, needs, err := a.Edge(ctx, api.EdgeRequest{Ref: "rte_traefik", ProxyUpstream: "http://pando-proxy:8080",
		EdgeConfig: []api.EdgeConfig{api.EdgeConfigKubernetesAPI}})
	require.NoError(t, err)
	require.True(t, needs)

	list, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 2, "the app's route and the console's")
	for _, obj := range list.Items {
		for _, svc := range routeServices(t, &obj) {
			require.Equal(t, "pando-proxy", svc["name"], obj.GetName())
			require.EqualValues(t, 8080, svc["port"], obj.GetName())
			require.Equal(t, true, svc["passHostHeader"])
		}
	}

	state, err := a.Observe(ctx, h)
	require.NoError(t, err)
	require.True(t, state.Present)
	require.Equal(t, "http://pando-proxy:8080", state.Address)

	// Traefik reads only its own namespace and cannot follow a route into
	// another one, so a route cannot name an app's Service.
	require.Contains(t, plan.Args, "--providers.kubernetescrd.namespaces=pando-edge")
	require.Contains(t, plan.Args, "--providers.kubernetescrd.allowCrossNamespace=false")
	require.Empty(t, plan.Mounts, "routes come from the API, not a shared directory")
	require.Equal(t, api.EdgeConfigKubernetesAPI, plan.ReadsRoutesFrom)
	require.Equal(t, "pando-proxy", plan.ProxyAlias)

	require.NoError(t, a.Remove(ctx, h))
	state, err = a.Observe(ctx, h)
	require.NoError(t, err)
	require.False(t, state.Present)
}

// TestR023_AnUpstreamOutsideTheEdgesNamespaceIsRefused asserts the route's
// backend must be a Service in the edge's namespace: a qualified name would
// be a route to somewhere else.
func TestR023_AnUpstreamOutsideTheEdgesNamespaceIsRefused(t *testing.T) {
	a, _ := kubeAdapter(t, `{"delivery":"kubernetes_api"}`)
	_, err := a.Ensure(context.Background(), api.RouteRequest{
		AppID: "app_01HQ8", Mode: spec.RoutingSubdomain, Hostname: "web.example.com",
		ProxyUpstream: "http://web.pando-app-01hq8.svc.cluster.local:3000",
	})
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "short name")
}

// TestR254_AnEdgeTheRuntimeCannotDeliverRoutesToIsRefused asserts R-254:
// core passes the runtime's EdgeConfig, and a mismatch is a readable refusal
// in both directions rather than an edge that never learns a route.
func TestR254_AnEdgeTheRuntimeCannotDeliverRoutesToIsRefused(t *testing.T) {
	ctx := context.Background()
	k8s, _ := kubeAdapter(t, `{"delivery":"kubernetes_api"}`)
	_, _, err := k8s.Edge(ctx, api.EdgeRequest{Ref: "rte_traefik", ProxyUpstream: "http://pando-proxy:8080"})
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "shared_mount")

	files, _ := adapter(t, "")
	_, _, err = files.Edge(ctx, api.EdgeRequest{Ref: "rte_traefik", ProxyUpstream: "http://pando:8080",
		EdgeConfig: []api.EdgeConfig{api.EdgeConfigKubernetesAPI}})
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "kubernetes_api")
}

// TestR169_OnKubernetesTraefikAsksPandoForItsCertificates asserts R-169 and
// R-174 on Kubernetes: Traefik orders nothing itself, its edge asks Pando for a
// certificate for every hostname served over HTTPS and for the console's, and
// every route names the Secret Pando keeps the certificate in. Plain HTTP is
// redirected, except a certificate authority's challenge, which reaches
// Pando's proxy.
func TestR169_OnKubernetesTraefikAsksPandoForItsCertificates(t *testing.T) {
	ctx := context.Background()
	a, dyn := kubeAdapter(t, `{"delivery":"kubernetes_api","certificates":"http","acme_email":"ops@example.com","console_hostname":"pando.example.com"}`)
	_, err := a.Ensure(ctx, api.RouteRequest{
		AppID: "app_01HQ8", Mode: spec.RoutingSubdomain, Hostname: "Web.apps.example.com",
		ProxyUpstream: "http://pando-proxy:8080", TLS: api.TLSRequest{Enabled: true, Hostname: "web.apps.example.com"},
	})
	require.NoError(t, err)

	plan, needs, err := a.Edge(ctx, api.EdgeRequest{Ref: "rte_traefik", ProxyUpstream: "http://pando-proxy:8080",
		EdgeConfig: []api.EdgeConfig{api.EdgeConfigKubernetesAPI}})
	require.NoError(t, err)
	require.True(t, needs)
	require.False(t, hasArg(plan, "--certificatesresolvers"), "Traefik's replicas order nothing")
	require.Contains(t, plan.Ports, api.EdgePort{Host: 443, Container: 443})
	require.NotNil(t, plan.Issue)
	require.Equal(t, api.ChallengeHTTP01, plan.Issue.Challenge)
	require.Equal(t, "ops@example.com", plan.Issue.Email)
	require.ElementsMatch(t, []api.CertificateOrder{
		{Name: "pando-tls-pando.example.com", Domains: []string{"pando.example.com"}},
		{Name: "pando-tls-web.apps.example.com", Domains: []string{"web.apps.example.com"}},
	}, plan.Issue.Orders)

	route, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").Get(ctx, "pando-app-01hq8", metav1.GetOptions{})
	require.NoError(t, err)
	secretName, _, _ := unstructured.NestedString(route.Object, "spec", "tls", "secretName")
	require.Equal(t, "pando-tls-web.apps.example.com", secretName)
	eps, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "entryPoints")
	require.Equal(t, []string{"websecure"}, eps)

	console, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").Get(ctx, "pando-console", metav1.GetOptions{})
	require.NoError(t, err)
	routes, _, _ := unstructured.NestedSlice(console.Object, "spec", "routes")
	require.Len(t, routes, 2)
	challenge := routes[0].(map[string]any)
	require.Contains(t, challenge["match"], "/.well-known/acme-challenge/")
	require.Nil(t, challenge["middlewares"], "the CA's request is answered, not redirected")
	require.NotNil(t, routes[1].(map[string]any)["middlewares"], "everything else on port 80 goes to HTTPS")
	for _, svc := range routeServices(t, console) {
		require.Equal(t, "pando-proxy", svc["name"])
	}
}

// TestR169_DNS01OnKubernetesOrdersTheWildcardThroughTheNamedProvider asserts
// the DNS-01 settings carry over: one wildcard for the base domain, through the
// DNS provider and credentials the adapter already holds, which any app under
// the base domain is served with.
func TestR169_DNS01OnKubernetesOrdersTheWildcardThroughTheNamedProvider(t *testing.T) {
	ctx := context.Background()
	a, dyn := kubeAdapter(t, `{"delivery":"kubernetes_api","certificates":"dns","acme_email":"ops@example.com",
		"base_domain":"apps.example.com","dns_provider":"cloudflare","credentials":{"dns_credentials":"CF_DNS_API_TOKEN=tok"}}`)
	_, err := a.Ensure(ctx, api.RouteRequest{
		AppID: "app_01HQ8", Mode: spec.RoutingSubdomain, Hostname: "web.apps.example.com",
		ProxyUpstream: "http://pando-proxy:8080", TLS: api.TLSRequest{Enabled: true},
	})
	require.NoError(t, err)
	plan, _, err := a.Edge(ctx, api.EdgeRequest{Ref: "rte_traefik", ProxyUpstream: "http://pando-proxy:8080",
		EdgeConfig: []api.EdgeConfig{api.EdgeConfigKubernetesAPI}})
	require.NoError(t, err)
	require.Equal(t, api.ChallengeDNS01, plan.Issue.Challenge)
	require.Equal(t, "cloudflare", plan.Issue.DNSProvider)
	require.Equal(t, "tok", plan.Issue.DNSCredentials["CF_DNS_API_TOKEN"].Reveal())
	require.Equal(t, []api.CertificateOrder{{Name: "pando-tls-wildcard", Domains: []string{"apps.example.com", "*.apps.example.com"}}}, plan.Issue.Orders)

	route, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").Get(ctx, "pando-app-01hq8", metav1.GetOptions{})
	require.NoError(t, err)
	secretName, _, _ := unstructured.NestedString(route.Object, "spec", "tls", "secretName")
	require.Equal(t, "pando-tls-wildcard", secretName)
}

// TestR169_AnUnnamedDNSProviderIsRefusedOnKubernetes asserts Pando's issuer
// refuses at configure a provider it cannot drive, rather than failing at the
// first order.
func TestR169_AnUnnamedDNSProviderIsRefusedOnKubernetes(t *testing.T) {
	e := configureErr(t, `{"delivery":"kubernetes_api","certificates":"dns","acme_email":"ops@example.com",
		"base_domain":"apps.example.com","dns_provider":"ovh","credentials":{"dns_credentials":"OVH_KEY=x"}}`)
	require.Equal(t, errs.ValidInvalid, e.Code)
	require.Contains(t, e.Message, "cloudflare")
}

// TestR174_AnAppsIngressRouteIsNamedAsTheAPIAccepts: an app ID has an
// underscore and capitals ("app_01M4…"), which a Kubernetes object name
// refuses. The fake client validates nothing, so the name is checked here as
// the API server checks it; on a kind cluster the route was refused and every
// deploy failed at "Routing traffic".
func TestR174_AnAppsIngressRouteIsNamedAsTheAPIAccepts(t *testing.T) {
	ctx := context.Background()
	a, dyn := kubeAdapter(t, `{"delivery":"kubernetes_api","certificates":"none","console_hostname":"pando.example.com"}`)
	appID := "app_01M4BCVYCB4F0Z9N0TC6GFCTN3"
	_, err := a.Ensure(ctx, api.RouteRequest{AppID: appID, Mode: spec.RoutingPath, PathPrefix: "/web",
		ProxyUpstream: "http://pando-proxy:8080"})
	require.NoError(t, err)

	list, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	var names []string
	for _, item := range list.Items {
		require.Empty(t, validation.IsDNS1123Subdomain(item.GetName()), "%q is not a name the API server accepts", item.GetName())
		names = append(names, item.GetName())
	}
	require.Contains(t, names, "pando-app-01m4bcvycb4f0z9n0tc6gfctn3")

	state, err := a.Observe(ctx, api.RouteHandle{AppID: appID})
	require.NoError(t, err)
	require.True(t, state.Present, "Observe finds the route by the same name")
	require.NoError(t, a.Remove(ctx, api.RouteHandle{AppID: appID}))
	_, err = dyn.Resource(ingressRoutes).Namespace("pando-edge").Get(ctx, "pando-app-01m4bcvycb4f0z9n0tc6gfctn3", metav1.GetOptions{})
	require.Error(t, err, "Remove deletes it by the same name")
}
