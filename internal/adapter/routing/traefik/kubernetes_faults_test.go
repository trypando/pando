package traefik_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/routing/traefik"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

var errAPI = errors.New("the API server is unavailable")

func failOn(dyn *dynamicfake.FakeDynamicClient, verb, resource string) {
	dyn.PrependReactor(verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errAPI
	})
}

func kubeEdge(ctx context.Context, a *traefik.Adapter) (api.EdgePlan, bool, error) {
	return a.Edge(ctx, api.EdgeRequest{Ref: "rte_traefik", ProxyUpstream: "http://pando-proxy:8080",
		EdgeConfig: []api.EdgeConfig{api.EdgeConfigKubernetesAPI}})
}

func appRoute(tls bool) api.RouteRequest {
	return api.RouteRequest{
		AppID: "app_01HQ8", Mode: spec.RoutingSubdomain, Hostname: "web.apps.example.com",
		ProxyUpstream: "http://pando-proxy:8080", TLS: api.TLSRequest{Enabled: tls},
	}
}

// TestR254_TraefikOnKubernetesIsConfiguredOrRefusedWithTheSettingThatFixesIt
// asserts the delivery setting and the connection to the cluster: an unknown
// delivery is refused naming both valid answers, a kubeconfig that cannot be
// read is an unavailable adapter with a remedy, and one that can is used.
func TestR254_TraefikOnKubernetesIsConfiguredOrRefusedWithTheSettingThatFixesIt(t *testing.T) {
	e := configureErr(t, `{"delivery":"carrier_pigeon","dir":"DIR"}`)
	require.Equal(t, errs.ValidInvalid, e.Code)
	require.Contains(t, e.Message, "shared_mount")
	require.Contains(t, e.Message, "kubernetes_api")

	e = configureErr(t, `{"delivery":"kubernetes_api","kubeconfig":"/nonexistent/kubeconfig"}`)
	require.Equal(t, errs.AdapterUnavailable, e.Code)
	require.Contains(t, e.Remedy, "kubeconfig")

	a, _ := adapter(t, `{"delivery":"shared_mount","dir":"DIR"}`)
	require.NoError(t, a.HealthCheck(context.Background()), "shared_mount named explicitly is the file delivery")

	// A real connection, to an API server that refuses Pando: the health
	// check names the namespace and what to apply.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"kind":"Status","status":"Failure","reason":"Forbidden","code":403}`, http.StatusForbidden)
	}))
	defer srv.Close()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfig, fmt.Appendf(nil, `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: %q}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`, srv.URL), 0o600))
	real := traefik.New()
	require.NoError(t, real.Configure(context.Background(), fmt.Appendf(nil, `{"delivery":"kubernetes_api","kubeconfig":%q}`, kubeconfig)))
	err := real.HealthCheck(context.Background())
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.ErrorContains(t, err, "namespace pando-edge")
	require.Contains(t, errs.As(err).Remedy, "deploy/kubernetes")
}

// TestR023_AProxyUpstreamThatIsNotAServiceHereIsRefused asserts the upstream
// forms refused before anything is written.
func TestR023_AProxyUpstreamThatIsNotAServiceHereIsRefused(t *testing.T) {
	ctx := context.Background()
	a, dyn := kubeAdapter(t, `{"delivery":"kubernetes_api"}`)
	for _, upstream := range []string{"", "http://pando-proxy:99999999999"} {
		r := appRoute(false)
		r.ProxyUpstream = upstream
		_, err := a.Ensure(ctx, r)
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err), upstream)
	}
	_, _, err := a.Edge(ctx, api.EdgeRequest{Ref: "rte_traefik", ProxyUpstream: "http://pando.pando.svc:8080",
		EdgeConfig: []api.EdgeConfig{api.EdgeConfigKubernetesAPI}})
	require.Contains(t, errs.As(err).Remedy, "http://pando-proxy:8080")

	list, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, list.Items)
}

// TestR174_ATraefikPandoDoesNotRunGetsRoutesOnItsOwnEntryPoint asserts the
// unmanaged Traefik on Kubernetes: app routes use the operator's entry point
// and certificate resolver, no edge is asked for, and the console routes a
// managed edge left behind are removed.
func TestR174_ATraefikPandoDoesNotRunGetsRoutesOnItsOwnEntryPoint(t *testing.T) {
	ctx := context.Background()
	a, dyn := kubeAdapter(t, `{"delivery":"kubernetes_api","managed":false,"entrypoint":"websecure","cert_resolver":"le"}`)
	for _, name := range []string{"pando-console", "pando-console-tls"} {
		_, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRoute",
			"metadata": map[string]any{"name": name, "namespace": "pando-edge"},
		}}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	_, err := a.Ensure(ctx, appRoute(true))
	require.NoError(t, err)
	route, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").Get(ctx, "pando-app-01hq8", metav1.GetOptions{})
	require.NoError(t, err)
	resolver, _, _ := unstructured.NestedString(route.Object, "spec", "tls", "certResolver")
	require.Equal(t, "le", resolver)
	eps, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "entryPoints")
	require.Equal(t, []string{"websecure"}, eps)

	_, needs, err := kubeEdge(ctx, a)
	require.NoError(t, err)
	require.False(t, needs, "Pando runs no edge for a Traefik it does not manage")
	list, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1, "only the app's route is left")

	plain, dyn2 := kubeAdapter(t, `{"delivery":"kubernetes_api","managed":false,"entrypoint":""}`)
	_, err = plain.Ensure(ctx, appRoute(true))
	require.NoError(t, err)
	route, err = dyn2.Resource(ingressRoutes).Namespace("pando-edge").Get(ctx, "pando-app-01hq8", metav1.GetOptions{})
	require.NoError(t, err)
	_, has, _ := unstructured.NestedFieldNoCopy(route.Object, "spec", "entryPoints")
	require.False(t, has, "no entry point set: Traefik's default ones")
	_, has, _ = unstructured.NestedFieldNoCopy(route.Object, "spec", "tls")
	require.False(t, has, "no resolver set: no TLS asked of Traefik")

	failOn(dyn2, "delete", "ingressroutes")
	_, _, err = kubeEdge(ctx, plain)
	require.ErrorIs(t, err, errAPI)
}

// TestR169_AnAppRouteWithoutTLSIsStillOnTheHTTPSEntryPoint asserts that with
// certificates on, every app route is on port 443, and a route that asks for
// no certificate of its own is served with whatever Traefik has; writing it
// again replaces it.
func TestR169_AnAppRouteWithoutTLSIsStillOnTheHTTPSEntryPoint(t *testing.T) {
	ctx := context.Background()
	a, dyn := kubeAdapter(t, `{"delivery":"kubernetes_api","certificates":"http","acme_email":"ops@example.com"}`)
	_, err := a.Ensure(ctx, appRoute(false))
	require.NoError(t, err)
	r := appRoute(false)
	r.Hostname = "renamed.apps.example.com"
	_, err = a.Ensure(ctx, r)
	require.NoError(t, err)

	route, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").Get(ctx, "pando-app-01hq8", metav1.GetOptions{})
	require.NoError(t, err)
	eps, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "entryPoints")
	require.Equal(t, []string{"websecure"}, eps)
	tls, _, _ := unstructured.NestedMap(route.Object, "spec", "tls")
	require.Empty(t, tls)
	routes, _, _ := unstructured.NestedSlice(route.Object, "spec", "routes")
	require.Contains(t, routes[0].(map[string]any)["match"], "renamed.apps.example.com", "the second write replaced the first")
}

// TestR023_ARouteWithNoBackendObservesAsPresentWithNoAddress asserts Observe
// reports a route edited by hand to have no backend as present and pointing
// nowhere, rather than failing or inventing one.
func TestR023_ARouteWithNoBackendObservesAsPresentWithNoAddress(t *testing.T) {
	ctx := context.Background()
	a, dyn := kubeAdapter(t, `{"delivery":"kubernetes_api"}`)
	h, err := a.Ensure(ctx, appRoute(false))
	require.NoError(t, err)
	for _, spec := range []map[string]any{
		{"routes": []any{}},
		{"routes": []any{map[string]any{"match": "Host(`x`)", "services": []any{}}}},
	} {
		obj, err := dyn.Resource(ingressRoutes).Namespace("pando-edge").Get(ctx, "pando-app-01hq8", metav1.GetOptions{})
		require.NoError(t, err)
		obj.Object["spec"] = spec
		_, err = dyn.Resource(ingressRoutes).Namespace("pando-edge").Update(ctx, obj, metav1.UpdateOptions{})
		require.NoError(t, err)
		state, err := a.Observe(ctx, h)
		require.NoError(t, err)
		require.True(t, state.Present)
		require.Empty(t, state.Address)
	}
}

// TestR105_EveryIngressRouteFailureIsReportedWithItsCause asserts that each
// failed call to the API is an adapter error carrying the API's error, for
// routes, the edge's console routes, its redirect middleware and its
// certificate list.
func TestR105_EveryIngressRouteFailureIsReportedWithItsCause(t *testing.T) {
	ctx := context.Background()
	httpTLS := `{"delivery":"kubernetes_api","certificates":"http","acme_email":"ops@example.com","console_hostname":"pando.example.com"}`
	plainCfg := `{"delivery":"kubernetes_api","console_hostname":"pando.example.com"}`
	cases := []struct {
		name, cfg, verb, resource string
		op                        func(*traefik.Adapter) error
	}{
		{"write a route", plainCfg, "create", "ingressroutes", func(a *traefik.Adapter) error { _, err := a.Ensure(ctx, appRoute(false)); return err }},
		{"remove a route", plainCfg, "delete", "ingressroutes", func(a *traefik.Adapter) error { return a.Remove(ctx, api.RouteHandle{AppID: "app_01HQ8"}) }},
		{"observe a route", plainCfg, "get", "ingressroutes", func(a *traefik.Adapter) error {
			_, err := a.Observe(ctx, api.RouteHandle{AppID: "app_01HQ8"})
			return err
		}},
		{"console route", plainCfg, "create", "ingressroutes", func(a *traefik.Adapter) error { _, _, err := kubeEdge(ctx, a); return err }},
		{"stale HTTPS console route", plainCfg, "delete", "ingressroutes", func(a *traefik.Adapter) error { _, _, err := kubeEdge(ctx, a); return err }},
		{"redirect middleware", httpTLS, "create", "middlewares", func(a *traefik.Adapter) error { _, _, err := kubeEdge(ctx, a); return err }},
		{"certificate list", httpTLS, "list", "ingressroutes", func(a *traefik.Adapter) error { _, _, err := kubeEdge(ctx, a); return err }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, dyn := kubeAdapter(t, c.cfg)
			failOn(dyn, c.verb, c.resource)
			err := c.op(a)
			require.ErrorIs(t, err, errAPI)
			require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		})
	}

	// The HTTPS console route is written after the HTTP one: fail only the
	// second write.
	a, dyn := kubeAdapter(t, httpTLS)
	writes := 0
	dyn.PrependReactor("create", "ingressroutes", func(k8stesting.Action) (bool, runtime.Object, error) {
		writes++
		if writes == 2 {
			return true, nil, errAPI
		}
		return false, nil, nil
	})
	_, _, err := kubeEdge(ctx, a)
	require.ErrorIs(t, err, errAPI)
}
