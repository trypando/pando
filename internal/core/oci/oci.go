// Package oci reads what a registry says about an image, before anything is
// pulled.
//
// An app deployed from a prebuilt image (R-101) has no repository for detection
// to look at, so the registry is the only place its port, its storage and its
// health check can be read from ahead of running it. The same read answers two
// questions the deploy path needs settled before it starts anything: which
// digest a tag names right now, so that is what the revision pins (R-120's
// rule for commits, applied to images), and whether there is a build for the
// host's CPU at all, so a mismatch is a plan-time error rather than a
// container that exits on start.
//
// Read-only and side-effect free: manifests and config blobs, never layers.
package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/registrylimit"
)

// Platform is an operating system and CPU architecture, as a registry names
// them: linux/amd64, linux/arm64.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// ParsePlatform reads os/arch[/variant]. Docker's own spellings of the
// architecture (x86_64, aarch64) are accepted, because a runtime reports the
// host's that way.
func ParsePlatform(s string) (Platform, bool) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return Platform{}, false
	}
	p := Platform{OS: strings.ToLower(parts[0]), Architecture: NormalizeArch(parts[1])}
	if len(parts) > 2 {
		p.Variant = strings.ToLower(parts[2])
	}
	return p, true
}

// NormalizeArch maps the kernel's names for an architecture to the registry's.
func NormalizeArch(arch string) string {
	switch strings.ToLower(arch) {
	case "x86_64", "x86-64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armhf", "arm":
		return "arm"
	case "i386", "i686", "386":
		return "386"
	}
	return strings.ToLower(arch)
}

// Runs reports whether an image built for p runs on a host that is host.
//
// The variant is compared only when both sides name one: an arm64 image
// without a variant is v8, which is every arm64 host Pando will meet.
func (p Platform) Runs(host Platform) bool {
	if p.OS != host.OS || NormalizeArch(p.Architecture) != NormalizeArch(host.Architecture) {
		return false
	}
	return p.Variant == "" || host.Variant == "" || p.Variant == host.Variant
}

// Config is what an image's configuration declares, for one platform.
type Config struct {
	// ExposedPorts are EXPOSE'd TCP ports, lowest first.
	ExposedPorts []int    `json:"exposed_ports,omitempty"`
	Volumes      []string `json:"volumes,omitempty"`
	Entrypoint   []string `json:"entrypoint,omitempty"`
	Cmd          []string `json:"cmd,omitempty"`
	WorkingDir   string   `json:"working_dir,omitempty"`
	User         string   `json:"user,omitempty"`
	Healthcheck  *Health  `json:"healthcheck,omitempty"`
}

// Health is a HEALTHCHECK, as the image declares it.
type Health struct {
	// Command is the check to run, without Docker's CMD/CMD-SHELL marker: a
	// CMD-SHELL check arrives as ["/bin/sh", "-c", script].
	Command  []string      `json:"command"`
	Interval time.Duration `json:"interval,omitempty"`
	Timeout  time.Duration `json:"timeout,omitempty"`
	Retries  int           `json:"retries,omitempty"`
}

// Inspection is what one read of a reference found.
type Inspection struct {
	// Reference is what was asked for, as given.
	Reference string `json:"reference"`
	// Digest is the content digest the reference resolved to — of the index
	// for a multi-platform image, so the runtime still picks its own platform
	// from it.
	Digest string `json:"digest"`
	// Platforms is every platform the image is published for.
	Platforms []Platform `json:"platforms"`
	// Config is the configuration of the build for the requested platform,
	// and nil when there is none.
	Config *Config `json:"config,omitempty"`
	// Matched is the platform Config was read from.
	Matched *Platform `json:"matched,omitempty"`
}

// Pinned is the reference with its digest in place of any tag.
func (i Inspection) Pinned() string { return Pin(i.Reference, i.Digest) }

// Supports reports whether the image has a build that runs on host.
func (i Inspection) Supports(host Platform) bool {
	for _, p := range i.Platforms {
		if p.Runs(host) {
			return true
		}
	}
	return false
}

// PlatformMismatch is the refusal for an image with no build for the host:
// said before anything is pulled, rather than as a container that exits on
// start with "exec format error".
func PlatformMismatch(image string, host Platform, published []Platform) error {
	names := make([]string, 0, len(published))
	for _, p := range published {
		names = append(names, p.String())
	}
	listed := "no platform Pando recognizes"
	if len(names) > 0 {
		listed = strings.Join(names, ", ")
	}
	return errs.Newf(errs.PlanImagePlatformUnsupported,
		"The image %s is published for %s, and this server runs %s, so it cannot run here.",
		image, listed, host.String()).
		WithRemedy("Use a tag of the image that is built for "+host.String()+", or ask whoever publishes it for a multi-platform build.").
		WithDetail("host_platform", host.String()).
		WithDetail("image_platforms", names)
}

// Inspector reads images from registries.
type Inspector struct {
	// Transport is the HTTP transport registry calls go through. Nil means the
	// default; tests point it at an in-memory registry.
	Transport http.RoundTripper
	// Insecure allows plain HTTP, for a registry on localhost in tests.
	Insecure bool
}

// Inspect resolves a reference and reads the configuration for want.
//
// auth is nil for an anonymous read. A zero want reads no configuration and
// only resolves the digest, with a HEAD where the registry allows: Platforms
// is then empty.
func (in Inspector) Inspect(ctx context.Context, reference string, auth *Auth, want Platform) (Inspection, error) {
	ref, err := in.parse(reference)
	if err != nil {
		return Inspection{}, err
	}

	base := in.Transport
	if base == nil {
		base = remote.DefaultTransport
	}
	watch := &limitWatch{base: base}
	opts := []remote.Option{
		remote.WithContext(ctx), remote.WithAuth(auth.authenticator()), remote.WithTransport(watch),
		// The client's own list less 429: a download limit lasts hours, and
		// asking again seconds later only spends more of it.
		remote.WithRetryStatusCodes(http.StatusRequestTimeout, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout),
	}
	readable := func(err error) error { return readable(reference, ref, auth, watch.refused(), err) }

	// Only the digest wanted: a HEAD answers it, and Docker Hub counts a
	// manifest GET against its pull limit and a HEAD not at all. A registry
	// that answers a HEAD without the digest, or refuses the method, is asked
	// again with a GET; one that said no for a reason a GET shares is not.
	if want.OS == "" {
		head, err := remote.Head(ref, opts...)
		if err == nil {
			return Inspection{Reference: reference, Digest: head.Digest.String()}, nil
		}
		if final(err) {
			return Inspection{}, readable(err)
		}
	}

	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return Inspection{}, readable(err)
	}

	out := Inspection{Reference: reference, Digest: desc.Digest.String()}

	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return Inspection{}, readable(err)
		}
		manifest, err := idx.IndexManifest()
		if err != nil {
			return Inspection{}, readable(err)
		}
		var chosen *v1.Descriptor
		for i, m := range manifest.Manifests {
			// Attestations ride along in an index as platform unknown/unknown.
			if m.Platform == nil || m.Platform.OS == "unknown" {
				continue
			}
			p := Platform{OS: m.Platform.OS, Architecture: m.Platform.Architecture, Variant: m.Platform.Variant}
			out.Platforms = append(out.Platforms, p)
			if chosen == nil && want.OS != "" && p.Runs(want) {
				chosen = &manifest.Manifests[i]
			}
		}
		if chosen != nil {
			img, err := idx.Image(chosen.Digest)
			if err != nil {
				return Inspection{}, readable(err)
			}
			cfg, err := img.ConfigFile()
			if err != nil {
				return Inspection{}, readable(err)
			}
			c := configOf(cfg)
			out.Config = &c
			p := Platform{OS: chosen.Platform.OS, Architecture: chosen.Platform.Architecture, Variant: chosen.Platform.Variant}
			out.Matched = &p
		}
		return out, nil
	}

	img, err := desc.Image()
	if err != nil {
		return Inspection{}, readable(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return Inspection{}, readable(err)
	}
	p := Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant}
	out.Platforms = []Platform{p}
	if want.OS != "" && p.Runs(want) {
		c := configOf(cfg)
		out.Config = &c
		out.Matched = &p
	}
	return out, nil
}

func (in Inspector) parse(reference string) (name.Reference, error) {
	var opts []name.Option
	if in.Insecure {
		opts = append(opts, name.Insecure)
	}
	ref, err := name.ParseReference(strings.TrimSpace(reference), opts...)
	if err != nil {
		return nil, errs.Newf(errs.ValidInvalid,
			"%q is not an image reference Pando can read.", reference).
			WithRemedy("Give the image as a registry, a repository and a tag, for example ghcr.io/acme/web:1.4 or nginx:1.27.")
	}
	return ref, nil
}

func configOf(cfg *v1.ConfigFile) Config {
	c := Config{
		Entrypoint: cfg.Config.Entrypoint,
		Cmd:        cfg.Config.Cmd,
		WorkingDir: cfg.Config.WorkingDir,
		User:       cfg.Config.User,
	}
	for port := range cfg.Config.ExposedPorts {
		number, proto, _ := strings.Cut(port, "/")
		if proto != "" && proto != "tcp" {
			continue
		}
		if n, err := strconv.Atoi(number); err == nil && n > 0 && n < 65536 {
			c.ExposedPorts = append(c.ExposedPorts, n)
		}
	}
	sort.Ints(c.ExposedPorts)
	for v := range cfg.Config.Volumes {
		c.Volumes = append(c.Volumes, v)
	}
	sort.Strings(c.Volumes)

	if h := cfg.Config.Healthcheck; h != nil && len(h.Test) > 0 {
		var command []string
		switch h.Test[0] {
		case "NONE":
		case "CMD":
			command = h.Test[1:]
		case "CMD-SHELL":
			command = append([]string{"/bin/sh", "-c"}, strings.Join(h.Test[1:], " "))
		default:
			command = h.Test
		}
		if len(command) > 0 {
			c.Healthcheck = &Health{Command: command, Interval: h.Interval, Timeout: h.Timeout, Retries: h.Retries}
		}
	}
	return c
}

// limitWatch keeps the headers of a registry's last 429, which say when it
// accepts downloads again and which the client's error does not carry.
type limitWatch struct {
	base http.RoundTripper
	mu   sync.Mutex
	last http.Header
}

func (w *limitWatch) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := w.base.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		w.mu.Lock()
		w.last = resp.Header.Clone()
		w.mu.Unlock()
	}
	return resp, err
}

func (w *limitWatch) refused() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// now is the clock a refusal's retry time is read against.
var now = time.Now

// readable turns a registry failure into something a person can act on
// (R-105). The four that matter are the image not existing, the registry
// wanting credentials, the registry limiting downloads, and the registry not
// answering. limited is the headers of the registry's 429, if it sent one.
func readable(reference string, ref name.Reference, auth *Auth, limited http.Header, err error) error {
	registry := ref.Context().RegistryStr()
	repo := ref.Context().RepositoryStr()

	var terr *transport.Error
	if errors.As(err, &terr) && rateLimited(terr) {
		signed := registrylimit.Anonymous
		if auth != nil {
			signed = registrylimit.SignedIn
		}
		at := now()
		return registrylimit.Refusal{
			Registry: registry, Image: reference, Signed: signed, RetryAt: registrylimit.RetryAt(limited, at),
		}.Error(at, err)
	}
	if errors.As(err, &terr) {
		switch terr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			if auth == nil {
				return errs.Newf(errs.ValidInvalid,
					"The registry %s would not let Pando read the image %s without signing in. "+
						"Either the image is private or it does not exist.", registry, reference).
					WithRemedy("If the image is private, add a registry credential to the app: a username and a token "+
						"that can read "+repo+", or AWS access keys for an ECR repository.").
					WithDetail("registry", registry)
			}
			return errs.Newf(errs.ValidInvalid,
				"The registry %s refused the app's registry credential for the image %s.", registry, reference).
				WithRemedy("Check that the credential is current and can read "+repo+", then replace it in the app's settings.").
				WithDetail("registry", registry)
		case http.StatusNotFound:
			return errs.Newf(errs.ValidInvalid,
				"The registry %s has no image %s.", registry, reference).
				WithRemedy("Check the repository name and the tag. A tag is case-sensitive, and an image that was deleted or never pushed cannot be run.").
				WithDetail("registry", registry)
		}
	}
	return errs.Wrap(errs.AdapterFailed,
		fmt.Sprintf("Pando could not read the image %s from the registry %s.", reference, registry), err).
		WithRemedy("Check that the registry is reachable from the Pando server, then try again.").
		WithDetail("registry", registry)
}

// rateLimited reports a registry's refusal for its download limit: a 429, or
// the distribution error code a registry may send with another status.
func rateLimited(terr *transport.Error) bool {
	if terr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	for _, d := range terr.Errors {
		if d.Code == transport.TooManyRequestsErrorCode {
			return true
		}
	}
	return false
}

// final reports a HEAD refusal a GET would get too: credentials, a missing
// image, the download limit.
func final(err error) bool {
	var terr *transport.Error
	if !errors.As(err, &terr) {
		return false
	}
	switch terr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return rateLimited(terr)
}

// Pin replaces a reference's tag with a digest:
// ghcr.io/acme/web:1.4 and sha256:ab… make ghcr.io/acme/web@sha256:ab….
//
// The registry and repository are kept as the user wrote them rather than as
// a parser normalizes them, so a Docker Hub image stays nginx@sha256:… and not
// index.docker.io/library/nginx@sha256:….
func Pin(reference, digest string) string {
	reference = strings.TrimSpace(reference)
	if digest == "" {
		return reference
	}
	if at := strings.Index(reference, "@"); at >= 0 {
		reference = reference[:at]
	}
	slash := strings.LastIndex(reference, "/")
	if colon := strings.LastIndex(reference, ":"); colon > slash {
		reference = reference[:colon]
	}
	return reference + "@" + digest
}

// Registry is the registry host a reference names, with Docker Hub spelled
// index.docker.io as the Docker daemon expects for credentials.
func Registry(reference string) (string, error) {
	ref, err := name.ParseReference(strings.TrimSpace(reference))
	if err != nil {
		return "", errs.Newf(errs.ValidInvalid, "%q is not an image reference Pando can read.", reference)
	}
	return ref.Context().RegistryStr(), nil
}

// authenticator adapts resolved credentials for the registry client.
func (a *Auth) authenticator() authn.Authenticator {
	if a == nil || (a.Username == "" && a.IdentityToken.IsZero()) {
		return authn.Anonymous
	}
	return authn.FromConfig(authn.AuthConfig{
		Username: a.Username, Password: a.Password.Reveal(), IdentityToken: a.IdentityToken.Reveal(),
	})
}
