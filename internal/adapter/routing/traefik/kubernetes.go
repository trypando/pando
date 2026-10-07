package traefik

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Routes as Traefik IngressRoutes, for an edge on the Kubernetes runtime
// (notes-kubernetes-runtime-issue-72.md, O-42).
//
// On Docker this adapter writes one file per app into a directory Traefik
// mounts. A cluster has no such directory every replica of the edge can watch,
// so with delivery kubernetes_api the same routes are IngressRoute objects in
// the edge's namespace, which Traefik's Kubernetes CRD provider watches and
// applies within seconds.
//
// Every IngressRoute names one backend: the Service Pando's proxy is reached
// by in that namespace — an ExternalName the runtime makes, resolving to
// Pando's own Service (R-023). Three things keep it so: this file writes no
// other backend; Traefik is started reading only its own namespace, with
// cross-namespace references off, so a route cannot name an app's Service; and
// every app namespace's NetworkPolicy refuses the edge's pods.

// Ways the adapter's routes reach Traefik.
const (
	DeliverySharedMount   = string(api.EdgeConfigSharedMount)
	DeliveryKubernetesAPI = string(api.EdgeConfigKubernetesAPI)

	defaultEdgeNamespace = "pando-edge"
	consoleRoute         = "pando-console"
)

var ingressRoutes = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}

var serviceName = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// newDynamic connects to the cluster's API: a kubeconfig file when one is set,
// otherwise the cluster Pando runs in. Replaced in tests.
var newDynamic = func(cfg Config) (dynamic.Interface, error) {
	var (
		rc  *rest.Config
		err error
	)
	if cfg.Kubeconfig != "" {
		rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: cfg.Kubeconfig}, &clientcmd.ConfigOverrides{}).ClientConfig()
	} else {
		rc, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(rc)
}

func (a *Adapter) viaAPI() bool { return a.config.Delivery == DeliveryKubernetesAPI }

// configureKubernetes checks the settings that only apply with delivery
// kubernetes_api, and connects.
func (a *Adapter) configureKubernetes(cfg *Config) error {
	switch cfg.Delivery {
	case "":
		cfg.Delivery = DeliverySharedMount
		return nil
	case DeliverySharedMount:
		return nil
	case DeliveryKubernetesAPI:
	default:
		return errs.Newf(errs.ValidInvalid,
			"Traefik's delivery setting is %q. Valid answers: shared_mount (route files in a directory Traefik mounts, on Docker) or kubernetes_api (IngressRoute objects, on Kubernetes).",
			cfg.Delivery)
	}
	if cfg.Namespace == "" {
		cfg.Namespace = defaultEdgeNamespace
	}
	if (cfg.Managed == nil || *cfg.Managed) && (cfg.Certificates == CertsHTTP || cfg.Certificates == CertsDNS) {
		// On Kubernetes certificates are issued once, by Pando's leader, and
		// kept as Secrets every Traefik replica reads (the note's
		// "Certificates with several replicas"). That issuer is not built
		// yet, and Traefik's own ACME would have every replica order the same
		// certificates and fail HTTP-01 whenever the CA reached another
		// replica.
		return errs.New(errs.ValidInvalid,
			"On Kubernetes, Traefik's certificates are issued by Pando, and this release of Pando does not issue them yet.").
			WithRemedy("Set certificates to none to serve plain HTTP, or terminate TLS at the cluster's load balancer in front of the edge.")
	}
	dyn, err := newDynamic(*cfg)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable,
			"Traefik routing is set to write IngressRoutes, and Pando could not connect to the Kubernetes API.", err).
			WithRemedy("Run Pando inside the cluster, as deploy/kubernetes does, or set kubeconfig to a kubeconfig file.")
	}
	a.dyn = dyn
	return nil
}

func (a *Adapter) routes() dynamic.ResourceInterface {
	return a.dyn.Resource(ingressRoutes).Namespace(a.config.Namespace)
}

// healthKubernetes checks Pando can read and write IngressRoutes.
func (a *Adapter) healthKubernetes(ctx context.Context) error {
	if a.dyn == nil {
		return errs.New(errs.Internal, "Traefik routing is not configured.")
	}
	if _, err := a.routes().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return errs.Wrap(errs.AdapterFailed,
			fmt.Sprintf("Pando cannot read Traefik's IngressRoutes in the namespace %s.", a.config.Namespace), err).
			WithRemedy("Install Traefik's CRDs and apply deploy/kubernetes, which gives Pando's ServiceAccount its role in that namespace.")
	}
	return nil
}

// proxyService is the Service an IngressRoute sends traffic to: the host of
// ProxyUpstream, which must be a Service in the edge's namespace.
func proxyService(upstream string) (string, int64, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Hostname() == "" {
		return "", 0, errs.New(errs.AdapterFailed, "Pando did not say where to send this app's traffic.")
	}
	if !serviceName.MatchString(u.Hostname()) {
		return "", 0, errs.Newf(errs.AdapterFailed,
			"On Kubernetes, Traefik reaches Pando through a Service in its own namespace, so Pando's proxy upstream must be a short name such as http://pando-proxy:8080, and it is %q.", upstream).
			WithRemedy("Set PANDO_SERVER_PROXY_UPSTREAM to http://pando-proxy:8080, as deploy/kubernetes does.")
	}
	port := int64(80)
	if p := u.Port(); p != "" {
		n, err := strconv.ParseInt(p, 10, 32)
		if err != nil {
			return "", 0, errs.Newf(errs.AdapterFailed, "%q is not a port in Pando's proxy upstream.", p)
		}
		port = n
	}
	return u.Hostname(), port, nil
}

// ingressRoute is one route to Pando's proxy, as an object.
func (a *Adapter) ingressRoute(name, appID string, routes []any, tls map[string]any) *unstructured.Unstructured {
	spec := map[string]any{"routes": routes}
	if ep := a.entryPoint(); ep != "" {
		spec["entryPoints"] = []any{ep}
	}
	if tls != nil {
		spec["tls"] = tls
	}
	labels := map[string]any{"app.kubernetes.io/managed-by": "pando"}
	if appID != "" {
		labels["pando.dev/app"] = sanitize(appID)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": ingressRoutes.Group + "/" + ingressRoutes.Version,
		"kind":       "IngressRoute",
		"metadata": map[string]any{
			"name": name, "namespace": a.config.Namespace, "labels": labels,
		},
		"spec": spec,
	}}
}

func proxyBackend(service string, port int64) []any {
	// passHostHeader: Pando's proxy needs the original Host to resolve a
	// subdomain app.
	return []any{map[string]any{"name": service, "port": port, "passHostHeader": true}}
}

// putRoute creates or replaces an IngressRoute.
func (a *Adapter) putRoute(ctx context.Context, obj *unstructured.Unstructured) error {
	existing, err := a.routes().Get(ctx, obj.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = a.routes().Create(ctx, obj, metav1.CreateOptions{})
	case err == nil:
		obj.SetResourceVersion(existing.GetResourceVersion())
		_, err = a.routes().Update(ctx, obj, metav1.UpdateOptions{})
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Pando could not write Traefik's IngressRoute.", err)
	}
	return nil
}

// ensureKubernetes writes an app's IngressRoute.
func (a *Adapter) ensureKubernetes(ctx context.Context, r api.RouteRequest, rule string) (api.RouteHandle, error) {
	service, port, err := proxyService(r.ProxyUpstream)
	if err != nil {
		return api.RouteHandle{}, err
	}
	var tls map[string]any
	if resolver := a.resolver(); r.TLS.Enabled && resolver != "" {
		tls = map[string]any{"certResolver": resolver}
	}
	name := routerName(r.AppID)
	obj := a.ingressRoute(name, r.AppID, []any{map[string]any{
		"match": rule, "kind": "Rule", "services": proxyBackend(service, port),
	}}, tls)
	if err := a.putRoute(ctx, obj); err != nil {
		return api.RouteHandle{}, err
	}
	return api.RouteHandle{AppID: r.AppID, Handle: a.config.Namespace + "/" + name}, nil
}

func (a *Adapter) removeKubernetes(ctx context.Context, h api.RouteHandle) error {
	err := a.routes().Delete(ctx, routerName(h.AppID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return errs.Wrap(errs.AdapterFailed, "Pando could not remove Traefik's IngressRoute.", err)
	}
	return nil
}

// observeKubernetes reports whether the IngressRoute is there, and the
// address it sends traffic to. It never rewrites a missing one (design 05).
func (a *Adapter) observeKubernetes(ctx context.Context, h api.RouteHandle) (api.RouteState, error) {
	obj, err := a.routes().Get(ctx, routerName(h.AppID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return api.RouteState{}, nil
	}
	if err != nil {
		return api.RouteState{}, errs.Wrap(errs.AdapterFailed, "Pando could not read Traefik's IngressRoute.", err)
	}
	return api.RouteState{Present: true, Address: backendOf(obj)}, nil
}

// backendOf is the first route's first service, as an address.
func backendOf(obj *unstructured.Unstructured) string {
	routes, _, _ := unstructured.NestedSlice(obj.Object, "spec", "routes")
	if len(routes) == 0 {
		return ""
	}
	route, _ := routes[0].(map[string]any)
	services, _ := route["services"].([]any)
	if len(services) == 0 {
		return ""
	}
	svc, _ := services[0].(map[string]any)
	return fmt.Sprintf("http://%v:%v", svc["name"], svc["port"])
}

// edgeKubernetes is the Traefik Pando runs on Kubernetes, reading
// IngressRoutes from its own namespace.
func (a *Adapter) edgeKubernetes(ctx context.Context, r api.EdgeRequest) (api.EdgePlan, bool, error) {
	if !a.managed() {
		err := a.routes().Delete(ctx, consoleRoute, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return api.EdgePlan{}, false, errs.Wrap(errs.AdapterFailed, "Pando could not remove its console route from Traefik.", err)
		}
		return api.EdgePlan{}, false, nil
	}
	service, port, err := proxyService(r.ProxyUpstream)
	if err != nil {
		return api.EdgePlan{}, false, err
	}

	// Every hostname that is not an app's reaches Pando, which answers with
	// the console; lowest priority, so an app's route always wins.
	routes := []any{}
	if h := a.config.ConsoleHostname; h != "" {
		routes = append(routes, map[string]any{
			"match": "Host(`" + h + "`)", "kind": "Rule", "services": proxyBackend(service, port),
		})
	}
	routes = append(routes, map[string]any{
		"match": "PathPrefix(`/`)", "kind": "Rule", "priority": int64(1), "services": proxyBackend(service, port),
	})
	if err := a.putRoute(ctx, a.ingressRoute(consoleRoute, "", routes, nil)); err != nil {
		return api.EdgePlan{}, false, err
	}

	ns := a.config.Namespace
	args := []string{
		"--providers.kubernetescrd",
		// Only its own namespace: a route written anywhere else is ignored.
		"--providers.kubernetescrd.namespaces=" + ns,
		// A route here cannot name a Service in an app's namespace.
		"--providers.kubernetescrd.allowCrossNamespace=false",
		// pando-proxy is an ExternalName Service, the one in the namespace.
		"--providers.kubernetescrd.allowExternalNameServices=true",
		"--entrypoints." + entryWeb + ".address=:80",
		"--api.dashboard=false",
		"--ping=true",
		"--log.level=INFO",
	}
	return api.EdgePlan{
		Name:            r.Ref,
		Image:           a.config.Image,
		Args:            args,
		Env:             nil,
		Ports:           []api.EdgePort{{Host: a.config.HTTPPort, Container: 80}},
		ProxyAlias:      service,
		ReadsRoutesFrom: api.EdgeConfigKubernetesAPI,
	}, true, nil
}
