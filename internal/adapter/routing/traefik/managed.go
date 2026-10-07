package traefik

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// The Traefik Pando runs (R-174, design 03 §4.4).
//
// This file says what Traefik is started with; the runtime adapter starts it,
// and core joins the two. Traefik's static configuration is its command line,
// generated here from the adapter's settings — so there is no traefik.yml for
// anyone to edit and have silently overwritten.

const (
	// Entrypoints and the resolver of the Traefik Pando runs. Its own names;
	// nothing outside this file and the routes it renders sees them.
	entryWeb      = "web"
	entryWebTLS   = "websecure"
	resolverPando = "pando"

	// Where Traefik reads routes and keeps certificates inside its container.
	containerRoutes = "/etc/traefik/dynamic"
	containerACME   = "/acme"

	// consoleFile routes every hostname that is not an app's to Pando, which
	// answers it with the console. Its name cannot collide with an app's file,
	// which is always "pando-app_…".
	consoleFile = "pando-console.yml"
)

// dnsProvider is a DNS-01 provider Pando names, and the credentials it needs.
type dnsProvider struct {
	Code  string
	Label string

	// Needs lists the sets of variables that satisfy the provider; one full
	// set is enough. Cloudflare takes a scoped token or a global key and email.
	Needs [][]string
}

// dnsProviders are the five offered by name (design 03 §4.4). Any other code
// Traefik knows works through "other", with whatever variables it is given.
var dnsProviders = []dnsProvider{
	{Code: "cloudflare", Label: "Cloudflare", Needs: [][]string{{"CF_DNS_API_TOKEN"}, {"CF_API_EMAIL", "CF_API_KEY"}}},
	{Code: "route53", Label: "Amazon Route 53", Needs: [][]string{{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}}},
	{Code: "digitalocean", Label: "DigitalOcean", Needs: [][]string{{"DO_AUTH_TOKEN"}}},
	{Code: "porkbun", Label: "Porkbun", Needs: [][]string{{"PORKBUN_API_KEY", "PORKBUN_SECRET_API_KEY"}}},
	{Code: "namecheap", Label: "Namecheap", Needs: [][]string{{"NAMECHEAP_API_USER", "NAMECHEAP_API_KEY"}}},
}

func knownProvider(code string) (dnsProvider, bool) {
	for _, p := range dnsProviders {
		if p.Code == code {
			return p, true
		}
	}
	return dnsProvider{}, false
}

var (
	envName      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	providerCode = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
)

// validateManaged fills a Pando-run Traefik's defaults and refuses a
// configuration that would start a Traefik unable to do what it says. It
// returns the DNS provider's credentials as environment.
func validateManaged(cfg *Config) (map[string]secret.Value, error) {
	if cfg.Managed != nil && !*cfg.Managed {
		return nil, nil
	}
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 80
	}
	if cfg.HTTPSPort == 0 {
		cfg.HTTPSPort = 443
	}
	for _, p := range []int{cfg.HTTPPort, cfg.HTTPSPort} {
		if p < 1 || p > 65535 {
			return nil, errs.Newf(errs.ValidInvalid, "%d is not a port number. A port is between 1 and 65535.", p)
		}
	}
	if cfg.Certificates == "" {
		cfg.Certificates = CertsNone
	}

	switch cfg.Certificates {
	case CertsNone:
		return nil, nil
	case CertsHTTP, CertsDNS:
	default:
		return nil, errs.Newf(errs.ValidInvalid,
			"Traefik's certificates setting is %q. Valid answers: http (one certificate per hostname), dns (a wildcard, with a DNS provider), or none.",
			cfg.Certificates)
	}

	if !strings.Contains(cfg.ACMEEmail, "@") {
		return nil, errs.New(errs.ValidInvalid,
			"Let's Encrypt needs an email address to issue certificates, and Traefik's settings do not have one.").
			WithRemedy("Set the certificate email address, or set certificates to none to serve plain HTTP.")
	}
	if cfg.Certificates == CertsHTTP {
		return nil, nil
	}

	// DNS-01.
	if cfg.BaseDomain == "" {
		return nil, errs.New(errs.ValidInvalid,
			"A wildcard certificate is for the base domain, and Traefik's settings do not have one.").
			WithRemedy("Set the base domain, such as apps.example.com, or use http certificates instead.")
	}
	if !providerCode.MatchString(cfg.DNSProvider) {
		return nil, errs.New(errs.ValidInvalid,
			"A wildcard certificate needs the DNS provider that hosts the base domain, and Traefik's settings do not name one.").
			WithRemedy("Choose the DNS provider, or choose Other and enter the provider's code as Traefik names it, such as gcloud or ovh.")
	}
	env, err := parseCredentials(cfg.Credentials.DNS)
	if err != nil {
		return nil, err
	}
	if p, ok := knownProvider(cfg.DNSProvider); ok {
		if !satisfies(env, p.Needs) {
			return nil, errs.Newf(errs.ValidInvalid,
				"%s needs %s in the DNS provider credentials, and they are not all there.",
				p.Label, describeNeeds(p.Needs)).
				WithRemedy("Enter each as a line of the form NAME=value in the DNS provider credentials.")
		}
	}
	return env, nil
}

// parseCredentials reads KEY=value lines. A malformed line is named by number
// and never quoted: it may be a secret with its name missing.
func parseCredentials(text string) (map[string]secret.Value, error) {
	env := map[string]secret.Value{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envName.MatchString(k) {
			return nil, errs.Newf(errs.ValidInvalid,
				"Line %d of the DNS provider credentials is not of the form NAME=value, with NAME in capital letters, digits and underscores.", i+1)
		}
		env[k] = secret.New(strings.TrimSpace(v))
	}
	return env, nil
}

func satisfies(env map[string]secret.Value, needs [][]string) bool {
	for _, set := range needs {
		all := true
		for _, k := range set {
			if env[k].IsZero() {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}

func describeNeeds(needs [][]string) string {
	sets := make([]string, 0, len(needs))
	for _, set := range needs {
		sets = append(sets, strings.Join(set, " and "))
	}
	return strings.Join(sets, ", or ")
}

func (a *Adapter) tls() bool {
	return a.config.Certificates == CertsHTTP || a.config.Certificates == CertsDNS
}

// entryPoint is where app routers attach.
func (a *Adapter) entryPoint() string {
	if !a.managed() {
		return a.config.EntryPoint
	}
	if a.tls() {
		return entryWebTLS
	}
	return entryWeb
}

// resolver is the ACME resolver app routers ask a certificate of.
func (a *Adapter) resolver() string {
	if !a.managed() {
		return a.config.CertResolver
	}
	if a.tls() {
		return resolverPando
	}
	return ""
}

// wildcardFor names the base domain when a hostname falls under its wildcard,
// so the route asks for that one certificate rather than its own.
func (a *Adapter) wildcardFor(host string) (string, bool) {
	if !a.managed() || a.config.Certificates != CertsDNS || a.config.BaseDomain == "" {
		return "", false
	}
	base := strings.ToLower(a.config.BaseDomain)
	host = strings.ToLower(host)
	if host == base {
		return base, true
	}
	// One level only: *.example.com covers a.example.com, not a.b.example.com.
	if rest, ok := strings.CutSuffix(host, "."+base); ok && rest != "" && !strings.Contains(rest, ".") {
		return base, true
	}
	return "", false
}

// Edge describes the Traefik Pando runs, and routes every hostname that is not
// an app's to the console. A Traefik somebody else runs has no edge, and the
// console route is withdrawn: that Traefik's other hostnames are not Pando's.
func (a *Adapter) Edge(ctx context.Context, r api.EdgeRequest) (api.EdgePlan, bool, error) {
	// The runtime says how its edges can receive routes (R-254). An edge
	// that cannot receive this adapter's would never learn a route.
	if !api.Offers(r.EdgeConfig, api.EdgeConfig(a.config.Delivery)) {
		return api.EdgePlan{}, false, errs.Newf(errs.PlanCapabilityUnsupported,
			"Traefik is set to deliver its routes as %s, and the runtime that runs the edge cannot deliver them that way.", a.config.Delivery).
			WithRemedy(fmt.Sprintf("Set Traefik's delivery setting to %s, which the runtime offers.", offered(r.EdgeConfig)))
	}
	if a.viaAPI() {
		return a.edgeKubernetes(ctx, r)
	}
	if !a.managed() {
		if err := os.Remove(filepath.Join(a.config.Dir, consoleFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return api.EdgePlan{}, false, errs.Wrap(errs.AdapterFailed, "Pando could not remove its console route from Traefik's configuration.", err)
		}
		return api.EdgePlan{}, false, nil
	}
	if r.ProxyUpstream == "" {
		return api.EdgePlan{}, false, errs.New(errs.AdapterFailed, "Pando did not say where Traefik should send traffic.")
	}
	if err := a.writeConsoleRoute(r.ProxyUpstream); err != nil {
		return api.EdgePlan{}, false, err
	}

	args := []string{
		"--providers.file.directory=" + containerRoutes,
		"--providers.file.watch=true",
		// Deliberately no Docker provider: labels live on a workload's
		// container, which would be this adapter reaching into the runtime's
		// objects, and a route that vanishes when a deploy recreates it.
		"--entrypoints." + entryWeb + ".address=:80",
		"--api.dashboard=false",
		"--log.level=INFO",
	}
	ports := []api.EdgePort{{Host: a.config.HTTPPort, Container: 80}}
	mounts := []api.EdgeMount{
		{Path: containerRoutes, SharedWithPando: a.config.Dir, ReadOnly: true},
	}

	if a.tls() {
		args = append(args,
			"--entrypoints."+entryWebTLS+".address=:443",
			"--entrypoints."+entryWeb+".http.redirections.entrypoint.to="+entryWebTLS,
			"--entrypoints."+entryWeb+".http.redirections.entrypoint.scheme=https",
			"--certificatesresolvers."+resolverPando+".acme.email="+a.config.ACMEEmail,
			"--certificatesresolvers."+resolverPando+".acme.storage="+containerACME+"/acme.json",
		)
		if a.config.Certificates == CertsDNS {
			args = append(args, "--certificatesresolvers."+resolverPando+".acme.dnschallenge.provider="+a.config.DNSProvider)
		} else {
			// The challenge is answered on :80 ahead of the redirect to HTTPS.
			args = append(args, "--certificatesresolvers."+resolverPando+".acme.httpchallenge.entrypoint="+entryWeb)
		}
		ports = append(ports, api.EdgePort{Host: a.config.HTTPSPort, Container: 443})
		mounts = append(mounts, api.EdgeMount{Path: containerACME, Volume: "acme"})
	}

	env := make(map[string]secret.Value, len(a.dnsEnv))
	for k, v := range a.dnsEnv {
		env[k] = v
	}

	return api.EdgePlan{
		Name:       r.Ref,
		Image:      a.config.Image,
		Args:       args,
		Env:        env,
		Ports:      ports,
		Mounts:     mounts,
		ProxyAlias: hostOf(r.ProxyUpstream),
	}, true, nil
}

// writeConsoleRoute sends every hostname that is not an app's to Pando.
//
// Pando answers a hostname it has no app for with the console already
// (proxy.StateResolver.IsAppHostname); this is what lets a request for one
// reach Pando at all. At the lowest priority, so an app's Host rule always
// wins. Rewritten only when it changes: Traefik reloads on every write.
func (a *Adapter) writeConsoleRoute(upstream string) error {
	body := a.renderConsole(upstream)
	path := filepath.Join(a.config.Dir, consoleFile)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, []byte(body)) {
		return nil
	}
	if err := os.MkdirAll(a.config.Dir, 0o755); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Pando could not write Traefik's configuration.", err)
	}
	tmp := path + ".tmp"
	// G306: 0644 because Traefik reads it as its own user. A route, not a
	// credential.
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil { //nolint:gosec
		return errs.Wrap(errs.AdapterFailed, "Pando could not write Traefik's configuration.", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return errs.Wrap(errs.AdapterFailed, "Pando could not write Traefik's configuration.", err)
	}
	return nil
}

func (a *Adapter) renderConsole(upstream string) string {
	var b strings.Builder
	b.WriteString("# Written by Pando. Do not edit: Pando rewrites this file.\n")
	b.WriteString("#\n")
	b.WriteString("# Every hostname that is not an app's reaches Pando, which answers with the\n")
	b.WriteString("# console. Lowest priority, so an app's own route always wins.\n")
	b.WriteString("http:\n")
	b.WriteString("  routers:\n")

	if h := a.config.ConsoleHostname; h != "" {
		b.WriteString("    pando-console:\n")
		fmt.Fprintf(&b, "      rule: %q\n", "Host(`"+h+"`)")
		fmt.Fprintf(&b, "      entryPoints:\n        - %s\n", a.entryPoint())
		b.WriteString("      service: pando-console\n")
		if a.tls() {
			fmt.Fprintf(&b, "      tls:\n        certResolver: %s\n", resolverPando)
		}
	}

	b.WriteString("    pando-fallback:\n")
	fmt.Fprintf(&b, "      rule: %q\n", "PathPrefix(`/`)")
	b.WriteString("      priority: 1\n")
	fmt.Fprintf(&b, "      entryPoints:\n        - %s\n", entryWeb)
	b.WriteString("      service: pando-console\n")
	if a.tls() {
		// Any other hostname over HTTPS, with whatever certificate Traefik
		// has. Asking for one here would ask Let's Encrypt for every name
		// anybody points at this machine.
		b.WriteString("    pando-fallback-tls:\n")
		fmt.Fprintf(&b, "      rule: %q\n", "PathPrefix(`/`)")
		b.WriteString("      priority: 1\n")
		fmt.Fprintf(&b, "      entryPoints:\n        - %s\n", entryWebTLS)
		b.WriteString("      service: pando-console\n")
		b.WriteString("      tls: {}\n")
	}

	b.WriteString("  services:\n")
	b.WriteString("    pando-console:\n")
	b.WriteString("      loadBalancer:\n")
	b.WriteString("        servers:\n")
	fmt.Fprintf(&b, "          - url: %q\n", upstream)
	b.WriteString("        passHostHeader: true\n")
	return b.String()
}

// offered names what a runtime offers, for a remedy.
func offered(list []api.EdgeConfig) string {
	if len(list) == 0 {
		return DeliverySharedMount
	}
	names := make([]string, 0, len(list))
	for _, c := range list {
		names = append(names, string(c))
	}
	return strings.Join(names, " or ")
}

// hostOf is the host part of an upstream URL — the name Traefik dials.
func hostOf(upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// providerOptions are the named DNS providers, for the settings form.
func providerOptions() []api.Option {
	opts := make([]api.Option, 0, len(dnsProviders))
	for _, p := range dnsProviders {
		opts = append(opts, api.Option{Value: p.Code, Label: p.Label})
	}
	return opts
}

// credentialsHelp says what each named provider needs, in the form's words.
func credentialsHelp() string {
	parts := make([]string, 0, len(dnsProviders))
	for _, p := range dnsProviders {
		parts = append(parts, p.Label+": "+describeNeeds(p.Needs))
	}
	return "One NAME=value per line. " + strings.Join(parts, ". ") +
		". For another provider, the variables Traefik's documentation lists for it."
}
