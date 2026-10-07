package traefik_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/routing/traefik"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

var ingressRoutes = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}

func kubeAdapter(t *testing.T, cfg string) (*traefik.Adapter, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ingressRoutes: "IngressRouteList"})
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

// TestR169_CertificatesOnKubernetesAreRefusedUntilPandoIssuesThem asserts the
// configuration is refused rather than started with every replica ordering
// its own certificates.
func TestR169_CertificatesOnKubernetesAreRefusedUntilPandoIssuesThem(t *testing.T) {
	e := configureErr(t, `{"delivery":"kubernetes_api","certificates":"http","acme_email":"ops@example.com"}`)
	require.Equal(t, errs.ValidInvalid, e.Code)
	require.Contains(t, e.Remedy, "none")
}
