package traefik

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
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

var (
	ingressRoutes = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}
	middlewares   = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "middlewares"}
)

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
	if (cfg.Managed == nil || *cfg.Managed) && cfg.Certificates == CertsDNS {
		// On Kubernetes certificates are issued once, by Pando's leader, and
		// kept as Secrets every Traefik replica reads (the note's
		// "Certificates with several replicas"); Traefik's own ACME would
		// have every replica order the same certificates. Pando's issuer
		// knows the five providers offered by name, not every code Traefik
		// does.
		if _, ok := knownProvider(cfg.DNSProvider); !ok {
			names := make([]string, 0, len(dnsProviders))
			for _, p := range dnsProviders {
				names = append(names, p.Code)
			}
			return errs.Newf(errs.ValidInvalid,
				"On Kubernetes Pando issues Traefik's certificates itself, through one of the DNS providers %s, and %q is not one of them.",
				strings.Join(names, ", "), cfg.DNSProvider).
				WithRemedy("Choose one of those DNS providers, or set certificates to http for one certificate per hostname.")
		}
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

// ingressRoute is one IngressRoute to Pando's proxy, as an object.
func (a *Adapter) ingressRoute(name, appID string, entryPoints []string, routes []any, tls map[string]any, annotations map[string]any) *unstructured.Unstructured {
	spec := map[string]any{"routes": routes}
	if len(entryPoints) > 0 {
		eps := make([]any, 0, len(entryPoints))
		for _, ep := range entryPoints {
			eps = append(eps, ep)
		}
		spec["entryPoints"] = eps
	}
	if tls != nil {
		spec["tls"] = tls
	}
	labels := map[string]any{"app.kubernetes.io/managed-by": "pando"}
	if appID != "" {
		labels["pando.dev/app"] = sanitize(appID)
	}
	meta := map[string]any{"name": name, "namespace": a.config.Namespace, "labels": labels}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": ingressRoutes.Group + "/" + ingressRoutes.Version,
		"kind":       "IngressRoute",
		"metadata":   meta,
		"spec":       spec,
	}}
}

func proxyBackend(service string, port int64) []any {
	// passHostHeader: Pando's proxy needs the original Host to resolve a
	// subdomain app.
	return []any{map[string]any{"name": service, "port": port, "passHostHeader": true}}
}

// putRoute creates or replaces an IngressRoute.
func (a *Adapter) putRoute(ctx context.Context, obj *unstructured.Unstructured) error {
	return a.put(ctx, a.routes(), obj, "IngressRoute")
}

func (a *Adapter) put(ctx context.Context, client dynamic.ResourceInterface, obj *unstructured.Unstructured, kind string) error {
	existing, err := client.Get(ctx, obj.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = client.Create(ctx, obj, metav1.CreateOptions{})
	case err == nil:
		obj.SetResourceVersion(existing.GetResourceVersion())
		_, err = client.Update(ctx, obj, metav1.UpdateOptions{})
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Pando could not write Traefik's "+kind+".", err)
	}
	return nil
}

// annoTLSHost marks an app route served over HTTPS with the hostname its
// certificate is for, which is how Edge learns what to ask Pando to issue.
const annoTLSHost = "pando.dev/tls-hostname"

// certificateName is the TLS Secret a hostname's routes name: the base
// domain's wildcard when it covers the hostname, otherwise one of its own.
func (a *Adapter) certificateName(host string) string {
	if _, ok := a.wildcardFor(host); ok {
		return "pando-tls-wildcard"
	}
	return "pando-tls-" + strings.ToLower(host)
}

// ensureKubernetes writes an app's IngressRoute. Over HTTPS when TLS is asked
// for and certificates are configured, naming the Secret Pando keeps the
// hostname's certificate in.
func (a *Adapter) ensureKubernetes(ctx context.Context, r api.RouteRequest, rule string) (api.RouteHandle, error) {
	service, port, err := proxyService(r.ProxyUpstream)
	if err != nil {
		return api.RouteHandle{}, err
	}
	var (
		tls         map[string]any
		annotations map[string]any
		entryPoints = []string{a.entryPoint()}
	)
	switch {
	case !a.managed():
		if resolver := a.resolver(); r.TLS.Enabled && resolver != "" {
			tls = map[string]any{"certResolver": resolver}
		}
	case a.tls() && r.TLS.Enabled && r.Hostname != "":
		tls = map[string]any{"secretName": a.certificateName(r.Hostname)}
		annotations = map[string]any{annoTLSHost: strings.ToLower(r.Hostname)}
		entryPoints = []string{entryWebTLS}
	case a.tls():
		entryPoints = []string{entryWebTLS}
		tls = map[string]any{}
	}
	if entryPoints[0] == "" {
		entryPoints = nil
	}
	name := routerName(r.AppID)
	obj := a.ingressRoute(name, r.AppID, entryPoints, []any{map[string]any{
		"match": rule, "kind": "Rule", "services": proxyBackend(service, port),
	}}, tls, annotations)
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

const (
	consoleRouteTLS = "pando-console-tls"
	redirectHTTPS   = "pando-redirect-https"
)

// edgeKubernetes is the Traefik Pando runs on Kubernetes, reading
// IngressRoutes from its own namespace, and, when certificates are on, the
// certificates Pando's leader issues for it (R-169): Traefik orders none
// itself, because its replicas would each order the same ones.
func (a *Adapter) edgeKubernetes(ctx context.Context, r api.EdgeRequest) (api.EdgePlan, bool, error) {
	if !a.managed() {
		for _, name := range []string{consoleRoute, consoleRouteTLS} {
			err := a.routes().Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return api.EdgePlan{}, false, errs.Wrap(errs.AdapterFailed, "Pando could not remove its console route from Traefik.", err)
			}
		}
		return api.EdgePlan{}, false, nil
	}
	service, port, err := proxyService(r.ProxyUpstream)
	if err != nil {
		return api.EdgePlan{}, false, err
	}
	backend := proxyBackend(service, port)

	// Port 80. Every hostname that is not an app's reaches Pando, which
	// answers with the console; lowest priority, so an app's route wins.
	// With certificates on, plain HTTP is redirected to HTTPS — except a
	// certificate authority's HTTP-01 request, which reaches Pando's proxy
	// on every hostname, where the leader's answer waits (R-169).
	web := []any{}
	if a.tls() {
		if err := a.put(ctx, a.dyn.Resource(middlewares).Namespace(a.config.Namespace), &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": middlewares.Group + "/" + middlewares.Version,
			"kind":       "Middleware",
			"metadata": map[string]any{"name": redirectHTTPS, "namespace": a.config.Namespace,
				"labels": map[string]any{"app.kubernetes.io/managed-by": "pando"}},
			"spec": map[string]any{"redirectScheme": map[string]any{"scheme": "https", "permanent": true}},
		}}, "Middleware"); err != nil {
			return api.EdgePlan{}, false, err
		}
		web = append(web,
			map[string]any{"match": "PathPrefix(`/.well-known/acme-challenge/`)", "kind": "Rule", "priority": int64(1000), "services": backend},
			map[string]any{"match": "PathPrefix(`/`)", "kind": "Rule", "priority": int64(1), "services": backend,
				"middlewares": []any{map[string]any{"name": redirectHTTPS}}},
		)
	} else {
		if h := a.config.ConsoleHostname; h != "" {
			web = append(web, map[string]any{"match": "Host(`" + h + "`)", "kind": "Rule", "services": backend})
		}
		web = append(web, map[string]any{"match": "PathPrefix(`/`)", "kind": "Rule", "priority": int64(1), "services": backend})
	}
	if err := a.putRoute(ctx, a.ingressRoute(consoleRoute, "", []string{entryWeb}, web, nil, nil)); err != nil {
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
	ports := []api.EdgePort{{Host: a.config.HTTPPort, Container: 80}}
	plan := api.EdgePlan{
		Name:            r.Ref,
		Image:           a.config.Image,
		ProxyAlias:      service,
		ReadsRoutesFrom: api.EdgeConfigKubernetesAPI,
	}

	if a.tls() {
		args = append(args, "--entrypoints."+entryWebTLS+".address=:443")
		ports = append(ports, api.EdgePort{Host: a.config.HTTPSPort, Container: 443})

		// Port 443: the console's hostname with its certificate, and any
		// other hostname with whatever Traefik has.
		tlsRoutes := []any{}
		var consoleTLS map[string]any
		if h := a.config.ConsoleHostname; h != "" {
			tlsRoutes = append(tlsRoutes, map[string]any{"match": "Host(`" + h + "`)", "kind": "Rule", "services": backend})
			consoleTLS = map[string]any{"secretName": a.certificateName(h)}
		} else {
			consoleTLS = map[string]any{}
		}
		tlsRoutes = append(tlsRoutes, map[string]any{"match": "PathPrefix(`/`)", "kind": "Rule", "priority": int64(1), "services": backend})
		if err := a.putRoute(ctx, a.ingressRoute(consoleRouteTLS, "", []string{entryWebTLS}, tlsRoutes, consoleTLS, nil)); err != nil {
			return api.EdgePlan{}, false, err
		}

		issue, err := a.certificateIssue(ctx)
		if err != nil {
			return api.EdgePlan{}, false, err
		}
		plan.Issue = issue
	} else if err := a.routes().Delete(ctx, consoleRouteTLS, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return api.EdgePlan{}, false, errs.Wrap(errs.AdapterFailed, "Pando could not remove its HTTPS console route from Traefik.", err)
	}

	plan.Args, plan.Ports = args, ports
	return plan, true, nil
}

// certificateIssue is what Pando is asked to issue: a certificate for every
// hostname an app route serves over HTTPS and for the console's, or, with
// DNS-01, the base domain's wildcard and one for each hostname outside it.
func (a *Adapter) certificateIssue(ctx context.Context) (*api.CertificateIssue, error) {
	issue := &api.CertificateIssue{Email: a.config.ACMEEmail, Challenge: api.ChallengeHTTP01}
	if a.config.Certificates == CertsDNS {
		issue.Challenge = api.ChallengeDNS01
		issue.DNSProvider = a.config.DNSProvider
		issue.DNSCredentials = make(map[string]secret.Value, len(a.dnsEnv))
		for k, v := range a.dnsEnv {
			issue.DNSCredentials[k] = v
		}
	}

	hosts := map[string]bool{}
	if h := a.config.ConsoleHostname; h != "" {
		hosts[strings.ToLower(h)] = true
	}
	list, err := a.routes().List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=pando"})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Pando could not read Traefik's IngressRoutes.", err)
	}
	for _, obj := range list.Items {
		if h := obj.GetAnnotations()[annoTLSHost]; h != "" {
			hosts[h] = true
		}
	}

	orders := map[string]api.CertificateOrder{}
	if a.config.Certificates == CertsDNS && a.config.BaseDomain != "" {
		// Issued before the first app exists, so a new app is served over
		// HTTPS at once (R-166).
		base := strings.ToLower(a.config.BaseDomain)
		orders["pando-tls-wildcard"] = api.CertificateOrder{Name: "pando-tls-wildcard", Domains: []string{base, "*." + base}}
	}
	for h := range hosts {
		name := a.certificateName(h)
		if _, ok := orders[name]; ok {
			continue
		}
		orders[name] = api.CertificateOrder{Name: name, Domains: []string{h}}
	}
	names := make([]string, 0, len(orders))
	for n := range orders {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		issue.Orders = append(issue.Orders, orders[n])
	}
	return issue, nil
}
