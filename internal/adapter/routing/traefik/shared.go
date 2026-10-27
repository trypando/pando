package traefik

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/atomicfile"
	"github.com/trypando/pando/internal/errs"
)

// Certificates shared by several edges (R-417, O-68).
//
// A Traefik Pando runs on one host orders its own certificates and keeps them
// in its acme volume. Once a second control host joins, each control host
// runs a Traefik of its own, and each ordering every hostname's certificate
// would spend Let's Encrypt's duplicate-certificate limit and fail HTTP-01
// whenever the CA's request reached another host. So in a cluster this
// Traefik orders nothing: Pando's leader orders each certificate once and
// keeps it in the database (internal/core/edgecert), every control host's
// runtime writes it into this directory as files, and the file below lists
// them, so every edge serves the same certificate. Any control host's proxy
// answers the HTTP-01 challenge from the database, so the challenge router
// sends the CA's request there ahead of the redirect to HTTPS.

const (
	// certificatesFile lists the certificates Pando issued, for Traefik's
	// certificate store, where a router with tls and no resolver finds one
	// by the name the client asked for.
	certificatesFile = "pando-certificates.yml"

	// tlsHostMarker records, in an app's route file, the hostname its
	// certificate is for, which is how Edge learns what to ask Pando to issue
	// — the file provider's counterpart of annoTLSHost.
	tlsHostMarker = "# pando-tls-hostname: "

	redirectMiddleware = "pando-redirect-https"
)

// pandoIssues reports whether Pando issues this Traefik's certificates rather
// than Traefik itself: always on Kubernetes, where it is several replicas,
// and in a cluster of several control hosts, where it is several edges.
func (a *Adapter) pandoIssues() bool {
	return a.managed() && a.tls() && (a.viaAPI() || a.clustered.Load())
}

// renderTLS writes an app router's tls block.
func (a *Adapter) renderTLS(b *strings.Builder, r api.RouteRequest) {
	if !r.TLS.Enabled {
		return
	}
	if a.pandoIssues() {
		// From the certificate store, which holds what Pando issued.
		b.WriteString("      tls: {}\n")
		return
	}
	resolver := a.resolver()
	if resolver == "" {
		return
	}
	b.WriteString("      tls:\n")
	fmt.Fprintf(b, "        certResolver: %s\n", resolver)
	// One wildcard for every app under the base domain, issued before the
	// first app exists, rather than one certificate per app (R-166).
	if main, ok := a.wildcardFor(r.Hostname); ok {
		b.WriteString("        domains:\n")
		fmt.Fprintf(b, "          - main: %q\n", main)
		fmt.Fprintf(b, "            sans:\n              - %q\n", "*."+main)
	}
}

// consoleTLS is the console router's tls block.
func (a *Adapter) consoleTLS() string {
	if a.pandoIssues() {
		return "      tls: {}\n"
	}
	return fmt.Sprintf("      tls:\n        certResolver: %s\n", resolverPando)
}

// renderChallenge writes port 80's router for the CA's HTTP-01 request when
// Pando issues: it reaches Pando's proxy on any hostname, ahead of the
// fallback's redirect to HTTPS. Traefik's entrypoint redirect cannot be used
// here; it would redirect the challenge too.
func (a *Adapter) renderChallenge(b *strings.Builder) {
	b.WriteString("    pando-acme-challenge:\n")
	fmt.Fprintf(b, "      rule: %q\n", "PathPrefix(`/.well-known/acme-challenge/`)")
	b.WriteString("      priority: 1000\n")
	fmt.Fprintf(b, "      entryPoints:\n        - %s\n", entryWeb)
	b.WriteString("      service: pando-console\n")
}

// renderRedirect writes the redirect middleware the fallback names when
// Pando issues.
func (a *Adapter) renderRedirect(b *strings.Builder) {
	b.WriteString("  middlewares:\n")
	fmt.Fprintf(b, "    %s:\n", redirectMiddleware)
	b.WriteString("      redirectScheme:\n")
	b.WriteString("        scheme: https\n")
	b.WriteString("        permanent: true\n")
	if a.config.HTTPSPort != 443 {
		fmt.Fprintf(b, "        port: %q\n", fmt.Sprint(a.config.HTTPSPort))
	}
}

// issueFromFiles is what Pando is asked to issue for a Traefik reading files:
// the console's hostname and every app route's marked hostname.
func (a *Adapter) issueFromFiles() (*api.CertificateIssue, error) {
	entries, err := os.ReadDir(a.config.Dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errs.Wrap(errs.AdapterFailed, "Pando could not read Traefik's configuration.", err)
	}
	hosts := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "pando-app_") || !strings.HasSuffix(name, ".yml") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(a.config.Dir, name))
		if err != nil {
			continue // removed since it was listed
		}
		for _, line := range strings.Split(string(body), "\n") {
			if h, ok := strings.CutPrefix(line, tlsHostMarker); ok && h != "" {
				hosts[strings.TrimSpace(h)] = true
			}
		}
	}
	return a.certificateIssue(hosts), nil
}

// writeCertificatesFile lists the issued certificates for Traefik, or removes
// the list when Pando does not issue. Rewritten only when it changes.
func (a *Adapter) writeCertificatesFile(issue *api.CertificateIssue) error {
	path := filepath.Join(a.config.Dir, certificatesFile)
	if issue == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return errs.Wrap(errs.AdapterFailed, "Pando could not remove Traefik's certificate list.", err)
		}
		return nil
	}
	names := make([]string, 0, len(issue.Orders))
	for _, o := range issue.Orders {
		names = append(names, o.Name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# Written by Pando. Do not edit: Pando rewrites this file.\n")
	b.WriteString("#\n")
	b.WriteString("# The certificates Pando issued, served by every control host's edge.\n")
	b.WriteString("tls:\n")
	b.WriteString("  certificates:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "    - certFile: %s\n", containerRoutes+"/"+n+".crt")
		fmt.Fprintf(&b, "      keyFile: %s\n", containerRoutes+"/"+n+".key")
	}
	return writeIfChanged(a.config.Dir, certificatesFile, b.String())
}

// writeIfChanged writes a file Traefik watches, atomically, and only when its
// content differs: Traefik reloads on every write.
func writeIfChanged(dir, name, body string) error {
	// G306: 0644 because Traefik reads it as its own user. Routes and paths,
	// not credentials.
	if err := atomicfile.WriteIfChanged(filepath.Join(dir, name), []byte(body), 0o644); err != nil { //nolint:gosec
		return errs.Wrap(errs.AdapterFailed, "Pando could not write Traefik's configuration.", err)
	}
	return nil
}
