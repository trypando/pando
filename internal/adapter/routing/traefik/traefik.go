// Package traefik routes apps through a Traefik edge (R-160, R-164, R-165).
//
// Traefik sits in front of Pando and forwards to Pando's proxy. It never points
// at a workload, and that is the single most important thing about this file.
// R-023 makes Pando's proxy the only enforcement point for every request to
// every app; a Traefik router pointing straight at a container would be a path
// to an app that skips authorization entirely, and it would work, which is why
// the instinct to do it is dangerous. `RouteRequest.ProxyUpstream` is in the
// request precisely so an adapter is *told* where to point rather than
// discovering it.
//
// Configuration is written as Traefik's file provider — one YAML file per app
// in a watched directory. Chosen over the Docker label provider because labels
// live on the workload container, which would mean this adapter reaching into
// the runtime adapter's objects: two adapters owning one thing, and a route
// that disappears when a deploy recreates the container.
package traefik

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/dynamic"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "traefik"

// Adapter writes Traefik file-provider configuration, and by default describes
// the Traefik Pando runs to read it (R-174, managed.go).
type Adapter struct {
	config Config

	// dnsEnv is the DNS provider's credentials, as the environment Traefik
	// reads them from. Held as secret.Value so it cannot reach a log (R-194).
	dnsEnv map[string]secret.Value

	// dyn writes IngressRoutes, with delivery kubernetes_api (kubernetes.go).
	dyn dynamic.Interface
}

// Config is the adapter's configuration.
type Config struct {
	// Managed means Pando runs Traefik itself (R-174): the default. False is
	// a Traefik somebody else runs, which Pando only writes route files for —
	// an install that already has Traefik on :80 and :443 for other things.
	// A pointer so an unset value is the default rather than false.
	Managed *bool `json:"managed,omitempty"`

	// Delivery is how routes reach Traefik: shared_mount, files in Dir, which
	// Traefik mounts (Docker, the default); or kubernetes_api, IngressRoute
	// objects in Namespace (the Kubernetes runtime, kubernetes.go). Core says
	// which the runtime offers, and Edge refuses a mismatch.
	Delivery string `json:"delivery,omitempty"`

	// Namespace holds the IngressRoutes, with delivery kubernetes_api.
	Namespace string `json:"namespace,omitempty"`

	// Kubeconfig connects to the cluster from outside it, with delivery
	// kubernetes_api. Empty is the cluster Pando runs in.
	Kubeconfig string `json:"kubeconfig,omitempty"`

	// Dir is the directory Traefik's file provider watches. Pando writes one
	// file per app here and Traefik picks them up; there is no API call and no
	// reload signal.
	Dir string `json:"dir"`

	// BaseDomain is what a subdomain app's hostname is carved out of, when the
	// route request does not carry one.
	BaseDomain string `json:"base_domain,omitempty"`

	// For a Traefik somebody else runs: the entrypoint routers attach to and
	// the ACME resolver it has configured. Empty CertResolver means Pando asks
	// for no certificate and Traefik serves whatever it already has — the
	// honest behavior when issuance is the edge's business (O-5). A Traefik
	// Pando runs has its own entrypoints and resolver, and ignores both.
	EntryPoint   string `json:"entrypoint,omitempty"`
	CertResolver string `json:"cert_resolver,omitempty"`

	// For the Traefik Pando runs.

	// Image is the Traefik image. Pinned by Pando's release; set to override.
	Image string `json:"image,omitempty"`

	// HTTPPort and HTTPSPort are the host ports Traefik takes.
	HTTPPort  int `json:"http_port,omitempty"`
	HTTPSPort int `json:"https_port,omitempty"`

	// ConsoleHostname is the hostname the console is served on over HTTPS.
	// Every hostname that is not an app's reaches the console anyway; this is
	// the one a certificate is asked for on its behalf.
	ConsoleHostname string `json:"console_hostname,omitempty"`

	// Certificates is how Traefik gets them (R-169): "http" (HTTP-01, per
	// hostname), "dns" (DNS-01, a wildcard for the base domain), or "none".
	// Neither challenge is a silent default: none serves plain HTTP on :80,
	// and the adapter says so.
	Certificates string `json:"certificates,omitempty"`

	// ACMEEmail is the address Let's Encrypt registers the account to.
	ACMEEmail string `json:"acme_email,omitempty"`

	// DNSProvider is the DNS-01 provider's code, as Traefik names it.
	DNSProvider string `json:"dns_provider,omitempty"`

	// Credentials arrive from encrypted storage (R-190), never from config.
	Credentials struct {
		// DNS is the provider's credentials as KEY=value lines, one per line.
		DNS string `json:"dns_credentials,omitempty"`
	} `json:"credentials,omitempty"`
}

// Certificate modes.
const (
	CertsNone = "none"
	CertsHTTP = "http"
	CertsDNS  = "dns"
)

// DefaultImage is the Traefik this release of Pando runs.
const DefaultImage = "traefik:v3.2"

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryRouting }

func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	cfg := Config{Dir: "/etc/traefik/dynamic", EntryPoint: "websecure"}
	if len(raw) > 0 {
		// Credentials at the top level were stored in the clear. The create
		// handler and the database both refuse them; refusing them here too
		// means a row written some other way does not quietly work (R-190).
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err == nil {
			if _, inline := top["dns_credentials"]; inline {
				return errs.New(errs.ValidInvalid, "The DNS provider credentials are in this adapter's stored settings, which are not encrypted.").
					WithRemedy("Set them as the adapter's DNS provider credentials instead.")
			}
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The Traefik routing configuration could not be read.", err)
		}
	}
	if cfg.Dir == "" && cfg.Delivery != DeliveryKubernetesAPI {
		return errs.New(errs.ValidInvalid, "Traefik routing needs a directory to write its configuration to.").
			WithRemedy("Set dir to the directory Traefik's file provider watches.")
	}

	dnsEnv, err := validateManaged(&cfg)
	if err != nil {
		return err
	}
	if err := a.configureKubernetes(&cfg); err != nil {
		return err
	}
	// Not kept in the config once parsed: the plain text lives only as long
	// as this call.
	cfg.Credentials.DNS = ""

	a.config = cfg
	a.dnsEnv = dnsEnv
	return nil
}

// managed reports whether Pando runs this Traefik.
func (a *Adapter) managed() bool { return a.config.Managed == nil || *a.config.Managed }

// HealthCheck verifies the configuration directory is writable.
//
// Not whether Traefik is running: Pando does not manage Traefik and cannot see
// it. What it can check is the one thing it is responsible for — that a route
// it writes will land somewhere Traefik reads. A directory that has become
// read-only produces routes that silently never appear, which is the failure
// this catches.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.viaAPI() {
		return a.healthKubernetes(ctx)
	}
	if a.config.Dir == "" {
		return errs.New(errs.Internal, "Traefik routing is not configured.")
	}
	if err := os.MkdirAll(a.config.Dir, 0o755); err != nil {
		return errs.Wrap(errs.AdapterFailed,
			fmt.Sprintf("Pando cannot create Traefik's configuration directory at %s.", a.config.Dir), err)
	}

	probe := filepath.Join(a.config.Dir, ".pando-probe.yml")
	// G306: 0644 because Traefik reads this directory as its own user, and the
	// probe's whole job is to prove that it can. The content is a comment.
	if err := os.WriteFile(probe, []byte("# pando write probe\n"), 0o644); err != nil { //nolint:gosec
		return errs.Wrap(errs.AdapterFailed,
			fmt.Sprintf("Pando cannot write to Traefik's configuration directory at %s.", a.config.Dir), err)
	}
	return os.Remove(probe)
}

// Capabilities: subdomain and path, with TLS when certificates are configured.
//
// SupportsWildcardTLS is true only for a Traefik Pando runs with DNS-01: a
// wildcard needs a DNS provider's credentials, and a Traefik somebody else runs
// may or may not have them — this adapter cannot see. Saying yes without them
// would produce a plan that succeeds and an app nobody can reach over HTTPS.
func (a *Adapter) Capabilities(context.Context) (api.RoutingCapabilities, error) {
	tls := a.config.CertResolver != ""
	wildcard := false
	if a.managed() {
		tls = a.config.Certificates == CertsHTTP || a.config.Certificates == CertsDNS
		wildcard = a.config.Certificates == CertsDNS
	}
	return api.RoutingCapabilities{
		Modes: []api.RoutingMode{spec.RoutingSubdomain, spec.RoutingPath},

		// Subdomain, because an install that has gone to the trouble of putting
		// Traefik in front of Pando has a hostname and wants apps on it. Path
		// mode remains available per app (R-162), and deviating from this
		// default is gated by app.routing.override.
		DefaultMode: spec.RoutingSubdomain,

		// Its base domain, when set, is where new apps are named (R-162's
		// sibling: a default the adapter declares rather than a setting kept
		// in step with it).
		BaseDomain: a.config.BaseDomain,

		SupportsTLS:         tls,
		SupportsWildcardTLS: wildcard,

		// Traefik is an inbound edge: something has to reach it.
		RequiresPublicReachability: true,
	}, nil
}

// Ensure writes the router for an app.
//
// Idempotent by construction: the file is named for the app and rewritten
// whole, so applying the same route twice leaves the same file. The reconciler
// calls this on every pass and must not accumulate anything.
func (a *Adapter) Ensure(ctx context.Context, r api.RouteRequest) (api.RouteHandle, error) {
	if r.ProxyUpstream == "" {
		// Refused rather than defaulted. A default here would be Pando's
		// address as this adapter guesses it, and a wrong guess produces a
		// route to nothing — or worse, to something else.
		return api.RouteHandle{}, errs.New(errs.AdapterFailed,
			"Pando did not say where to send this app's traffic.")
	}

	rule, err := a.rule(r)
	if err != nil {
		return api.RouteHandle{}, err
	}
	if a.viaAPI() {
		return a.ensureKubernetes(ctx, r, rule)
	}

	if err := os.MkdirAll(a.config.Dir, 0o755); err != nil {
		return api.RouteHandle{}, errs.Wrap(errs.AdapterFailed,
			"Pando could not write Traefik's configuration.", err)
	}

	body := a.render(r, rule)
	path := a.pathFor(r.AppID)

	// Written to a temporary file and renamed, because Traefik watches the
	// directory and will read a half-written file the moment it appears.
	tmp := path + ".tmp"
	// G306: 0644 because Traefik runs as a different user and has to read it.
	// Router rules, not credentials: Traefik never sees a secret, which is why
	// the proxy and not the router makes the authorization decision (R-023).
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil { //nolint:gosec
		return api.RouteHandle{}, errs.Wrap(errs.AdapterFailed,
			"Pando could not write Traefik's configuration.", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return api.RouteHandle{}, errs.Wrap(errs.AdapterFailed,
			"Pando could not write Traefik's configuration.", err)
	}
	return api.RouteHandle{AppID: r.AppID, Handle: path}, nil
}

func (a *Adapter) Remove(ctx context.Context, h api.RouteHandle) error {
	if a.viaAPI() {
		return a.removeKubernetes(ctx, h)
	}
	path := h.Handle
	if path == "" {
		path = a.pathFor(h.AppID)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errs.Wrap(errs.AdapterFailed, "Pando could not remove Traefik's configuration.", err)
	}
	return nil
}

// Observe reports whether the route file is there.
//
// It reports what exists and never remediates — the reconciler decides what to
// do about drift (design 05). An adapter that quietly rewrote a missing file
// here would make drift undetectable.
func (a *Adapter) Observe(ctx context.Context, h api.RouteHandle) (api.RouteState, error) {
	if a.viaAPI() {
		return a.observeKubernetes(ctx, h)
	}
	path := h.Handle
	if path == "" {
		path = a.pathFor(h.AppID)
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return api.RouteState{Present: false}, nil
	}
	if err != nil {
		return api.RouteState{}, errs.Wrap(errs.AdapterFailed, "Pando could not read Traefik's configuration.", err)
	}
	return api.RouteState{Present: true, Address: addressIn(string(body))}, nil
}

// rule builds the Traefik matcher for a route.
func (a *Adapter) rule(r api.RouteRequest) (string, error) {
	switch r.Mode {
	case spec.RoutingSubdomain:
		host := r.Hostname
		if host == "" {
			return "", errs.New(errs.AdapterFailed, "This app has no hostname to route.")
		}
		return fmt.Sprintf("Host(`%s`)", host), nil

	case spec.RoutingPath:
		prefix := r.PathPrefix
		if prefix == "" {
			return "", errs.New(errs.AdapterFailed, "This app has no path to route.")
		}
		if !strings.HasPrefix(prefix, "/") {
			prefix = "/" + prefix
		}
		if r.Hostname != "" {
			return fmt.Sprintf("Host(`%s`) && PathPrefix(`%s`)", r.Hostname, prefix), nil
		}
		return fmt.Sprintf("PathPrefix(`%s`)", prefix), nil

	default:
		// Port mode reaches Pando's proxy directly and needs no edge router.
		// Refused rather than ignored: silently writing nothing would leave an
		// app the planner believed was routed.
		return "", errs.Newf(errs.AdapterFailed,
			"Traefik routing does not handle %q addressing.", r.Mode)
	}
}

// render writes the dynamic configuration for one app.
//
// The service points at ProxyUpstream — Pando — and the path prefix is NOT
// stripped. Pando's proxy strips it and sets X-Forwarded-Prefix itself (R-167),
// because it is the thing that knows which app the prefix belonged to. Stripping
// here would hand Pando a path it cannot resolve.
func (a *Adapter) render(r api.RouteRequest, rule string) string {
	name := routerName(r.AppID)

	var b strings.Builder
	b.WriteString("# Written by Pando. Do not edit: this file is rewritten on every deploy.\n")
	b.WriteString("#\n")
	b.WriteString("# The service below points at Pando's proxy, never at the app's container.\n")
	b.WriteString("# Every request to every app goes through Pando so that authorization is\n")
	b.WriteString("# applied exactly once, in one place (R-023).\n")
	b.WriteString("http:\n")
	b.WriteString("  routers:\n")
	fmt.Fprintf(&b, "    %s:\n", name)
	fmt.Fprintf(&b, "      rule: %q\n", rule)
	fmt.Fprintf(&b, "      service: %s\n", name)
	if ep := a.entryPoint(); ep != "" {
		fmt.Fprintf(&b, "      entryPoints:\n        - %s\n", ep)
	}
	if resolver := a.resolver(); r.TLS.Enabled && resolver != "" {
		b.WriteString("      tls:\n")
		fmt.Fprintf(&b, "        certResolver: %s\n", resolver)
		// One wildcard for every app under the base domain, issued before the
		// first app exists, rather than one certificate per app (R-166).
		if main, ok := a.wildcardFor(r.Hostname); ok {
			b.WriteString("        domains:\n")
			fmt.Fprintf(&b, "          - main: %q\n", main)
			fmt.Fprintf(&b, "            sans:\n              - %q\n", "*."+main)
		}
	}
	b.WriteString("  services:\n")
	fmt.Fprintf(&b, "    %s:\n", name)
	b.WriteString("      loadBalancer:\n")
	b.WriteString("        servers:\n")
	fmt.Fprintf(&b, "          - url: %q\n", r.ProxyUpstream)
	// Pando's proxy needs the original Host to resolve a subdomain app, so the
	// header is passed through rather than rewritten to the upstream's.
	b.WriteString("        passHostHeader: true\n")
	return b.String()
}

func (a *Adapter) pathFor(appID string) string {
	return filepath.Join(a.config.Dir, "pando-"+sanitize(appID)+".yml")
}

func routerName(appID string) string { return "pando-" + sanitize(appID) }

// sanitize keeps a Traefik router name and a filename to safe characters.
//
// App IDs are Pando's own prefixed ULIDs, so this cannot bite today. It is here
// because "the caller only passes safe values" is how a path escapes a
// directory later, in a change that looks unrelated.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// addressIn pulls the upstream back out of a rendered file, for Observe.
func addressIn(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "- url: "); ok {
			return strings.Trim(after, `"`)
		}
	}
	return ""
}

var _ api.RoutingAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryRouting,
		Kind:        Kind,
		Name:        "Traefik",
		Description: "Gives apps their own hostnames through a Traefik Pando runs on ports 80 and 443, with certificates.",
		IDPrefix:    "rte_",
		Fields: []api.Field{
			{Key: "base_domain", Label: "Base domain", Type: "string", Help: "Apps are served at <app>.<base domain>. Point this domain and *.<base domain> at this machine.", Placeholder: "apps.example.com"},
			{Key: "console_hostname", Label: "Console hostname", Type: "string", Help: "Where the console is served over HTTPS. Any hostname pointed at this machine that is not an app's shows the console; this is the one a certificate is issued for.", Placeholder: "pando.example.com"},
			{Key: "certificates", Label: "Certificates", Type: "select", Default: CertsNone,
				Options: []api.Option{
					{Value: CertsHTTP, Label: "One per hostname", Description: "Let's Encrypt checks each hostname over port 80, which has to be reachable from the internet."},
					{Value: CertsDNS, Label: "One wildcard for the base domain", Description: "Covers every app before it exists. Needs your DNS provider's credentials."},
					{Value: CertsNone, Label: "None", Description: "Apps are served over plain HTTP on port 80."},
				}},
			{Key: "acme_email", Label: "Certificate email", Type: "string", Help: "The address Let's Encrypt registers the certificates to.", Placeholder: "ops@example.com",
				ShownWhen: &api.Condition{Key: "certificates", Values: []string{CertsHTTP, CertsDNS}}},
			{Key: "dns_provider", Label: "DNS provider", Type: "select", Other: true, Options: providerOptions(),
				Help:      "Who hosts the base domain's DNS. Choose Other for any provider Traefik supports, and enter its code, such as gcloud or ovh.",
				ShownWhen: &api.Condition{Key: "certificates", Values: []string{CertsDNS}}},
			{Key: "dns_credentials", Label: "DNS provider credentials", Type: "string", Credential: true, Multiline: true, Help: credentialsHelp(),
				ShownWhen: &api.Condition{Key: "certificates", Values: []string{CertsDNS}}},
			{Key: "managed", Label: "Pando runs Traefik", Type: "bool", Default: "true", Help: "Off if this machine already runs a Traefik that should serve Pando's apps. Pando then only writes route files into the directory that Traefik watches."},
			{Key: "http_port", Label: "HTTP port", Type: "int", Default: "80", ShownWhen: &api.Condition{Key: "managed", Values: []string{"true"}}, Advanced: true},
			{Key: "https_port", Label: "HTTPS port", Type: "int", Default: "443", ShownWhen: &api.Condition{Key: "managed", Values: []string{"true"}}, Advanced: true},
			{Key: "image", Label: "Traefik image", Type: "string", Default: DefaultImage, Help: "Set to run a different Traefik release than this Pando ships with.", ShownWhen: &api.Condition{Key: "managed", Values: []string{"true"}}, Advanced: true},
			{Key: "delivery", Label: "Route delivery", Type: "select", Default: DeliverySharedMount, Advanced: true,
				Help: "How routes reach Traefik. Use IngressRoutes with the Kubernetes runtime.",
				Options: []api.Option{
					{Value: DeliverySharedMount, Label: "Files", Description: "Route files in a directory Traefik mounts. The Docker runtime."},
					{Value: DeliveryKubernetesAPI, Label: "IngressRoutes", Description: "IngressRoute objects Traefik watches in the cluster's API. The Kubernetes runtime."},
				}},
			{Key: "namespace", Label: "IngressRoute namespace", Type: "string", Default: defaultEdgeNamespace, Advanced: true,
				ShownWhen: &api.Condition{Key: "delivery", Values: []string{DeliveryKubernetesAPI}}},
			{Key: "kubeconfig", Label: "Kubeconfig file", Type: "string", Default: "The cluster Pando runs in", Advanced: true,
				ShownWhen: &api.Condition{Key: "delivery", Values: []string{DeliveryKubernetesAPI}}},
			{Key: "dir", Label: "Configuration directory", Type: "string", Help: "Where Pando writes Traefik's route files.", Default: "/etc/traefik/dynamic", Advanced: true},
			{Key: "entrypoint", Label: "Entry point", Type: "string", Help: "The entry point of your Traefik that apps are served on.", Default: "websecure", ShownWhen: &api.Condition{Key: "managed", Values: []string{"false"}}, Advanced: true},
			{Key: "cert_resolver", Label: "Certificate resolver", Type: "string", Help: "The certificate resolver your Traefik has configured.", Placeholder: "letsencrypt", ShownWhen: &api.Condition{Key: "managed", Values: []string{"false"}}},
		},
	}
}
