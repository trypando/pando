// Package registrylimit says what to do when a registry refuses an image
// because this server has downloaded too many.
//
// Docker Hub limits downloads: 100 every 6 hours from one IPv4 address (or
// IPv6 /64) without signing in, 200 for a signed-in Personal account, none on
// a paid plan. A manifest GET counts; a HEAD does not; a multi-platform image
// counts once per platform pulled. Other registries limit too, with a 429 and
// the OCI distribution code TOOMANYREQUESTS.
//
// The refusal reaches Pando four ways — the registry read before a deploy
// (core/oci), the Docker daemon's pull, a Kubernetes node's pull and a
// BuildKit build's base image — and each says the same thing (R-105), so the
// wording lives here, in a leaf package core and the adapters can both import.
package registrylimit

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/trypando/pando/internal/errs"
)

// Signed says whose limit was reached.
type Signed int

const (
	// Unknown: the caller cannot tell whether a credential was used.
	Unknown Signed = iota
	// Anonymous: no credential, so the limit is this server's address.
	Anonymous
	// SignedIn: the app's registry credential was used, so the limit is that
	// account's.
	SignedIn
)

// Refusal is one registry refusing a download for its rate limit.
type Refusal struct {
	// Registry is the registry's host, as the reference or the error named it.
	// Empty when neither said.
	Registry string
	// Image is the reference refused, empty when it is not known.
	Image string
	// RetryAt is when the registry said it accepts downloads again; zero when
	// it did not say.
	RetryAt time.Time
	Signed  Signed
	// Build is a base image a build needed. BuildKit pulls those without the
	// app's credential, so adding one does not help.
	Build bool
}

// Mentioned reports whether an error's text is a registry's rate-limit
// refusal: the distribution error code, or the HTTP status as Docker, the
// kubelet and BuildKit print it.
func Mentioned(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "toomanyrequests") || strings.Contains(t, "429 too many requests")
}

var (
	registryURL = regexp.MustCompile(`https?://([^/\s"]+)/v2/`)
	sourceImage = regexp.MustCompile(`(?i)(?:source metadata for|pull and unpack image|pull image) "?([^\s"]+?)"?:\s`)
)

// FromText reads a refusal out of an error's text. image is the reference the
// caller pulled, or empty for a build, where the text names the base image.
func FromText(text, image string) Refusal {
	r := Refusal{Image: image}
	if r.Image == "" {
		if m := sourceImage.FindStringSubmatch(text); m != nil {
			r.Image = m[1]
		}
	}
	if r.Image != "" {
		r.Registry = Host(r.Image)
	} else if m := registryURL.FindStringSubmatch(text); m != nil {
		r.Registry = m[1]
	}
	return r
}

// Host is the registry an image reference names, docker.io when it names
// none.
func Host(image string) string {
	first, _, found := strings.Cut(strings.TrimSpace(image), "/")
	if found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return "docker.io"
}

// DockerHub reports whether a registry host is Docker Hub, under any of the
// names it answers to.
func DockerHub(host string) bool {
	switch strings.ToLower(host) {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return true
	}
	return false
}

// RetryAt reads when a 429 response says to try again: Retry-After, in
// seconds or as a date, then the ratelimit-reset header (seconds from now, or
// a Unix time). Zero when neither is there.
func RetryAt(h http.Header, now time.Time) time.Time {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return now.Add(time.Duration(n) * time.Second).UTC()
		}
		if t, err := http.ParseTime(v); err == nil {
			return t.UTC()
		}
	}
	if v := strings.TrimSpace(h.Get("Ratelimit-Reset")); v != "" {
		v, _, _ = strings.Cut(v, ";")
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
			// A value this large is a moment, not a number of seconds.
			if n > 1_000_000_000 {
				return time.Unix(n, 0).UTC()
			}
			return now.Add(time.Duration(n) * time.Second).UTC()
		}
	}
	return time.Time{}
}

// Error is the refusal as a person reads it (R-105). now places RetryAt.
func (r Refusal) Error(now time.Time, cause error) *errs.Error {
	hub := DockerHub(r.Registry)
	who := "The registry"
	switch {
	case hub:
		who = "Docker Hub"
	case r.Registry != "":
		who = "The registry " + r.Registry
	}
	what := "an image"
	switch {
	case r.Build && r.Image != "":
		what = "the base image " + r.Image + " that the build starts from"
	case r.Build:
		what = "a base image the build starts from"
	case r.Image != "":
		what = "the image " + r.Image
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s is limiting how many images this server may download, and refused to send %s.", who, what)
	if hub && (r.Signed == Anonymous || r.Build) {
		b.WriteString(" Without signing in, Docker Hub allows 100 downloads every 6 hours from one internet address, and this server has used them.")
	}
	switch {
	case !r.RetryAt.IsZero():
		fmt.Fprintf(&b, " It accepts downloads again after %s, %s.", r.RetryAt.UTC().Format("2006-01-02 15:04 MST"), until(now, r.RetryAt))
	case hub:
		b.WriteString(" Docker Hub counts downloads over the last 6 hours, so the limit lifts within 6 hours.")
	}

	e := errs.Wrap(errs.AdapterRegistryRateLimited, b.String(), cause).WithRemedy(r.remedy(hub))
	if r.Registry != "" {
		e = e.WithDetail("registry", r.Registry)
	}
	if !r.RetryAt.IsZero() {
		e = e.WithDetail("retry_at", r.RetryAt.UTC().Format(time.RFC3339))
	}
	return e
}

func (r Refusal) remedy(hub bool) string {
	switch {
	case r.Build && hub:
		return "Pando's builder downloads base images without signing in, so the limit is this server's address. " +
			"Wait for the limit to lift and deploy again, or change the Dockerfile's FROM line to a copy of the image " +
			"on a registry without this limit: Docker's official images are also published at public.ecr.aws/docker/library."
	case r.Build:
		return "Pando's builder downloads base images without signing in, so the limit is this server's address. " +
			"Wait for the limit to lift and deploy again, or change the Dockerfile's FROM line to a copy of the image on another registry."
	case r.Signed == SignedIn && hub:
		return "The app's registry credential was used, so the limit is that Docker Hub account's. " +
			"Wait for the limit to lift and deploy again, or move the account to a paid Docker Hub plan, which has no download limit."
	case r.Signed == SignedIn:
		return "The app's registry credential was used, so the limit is that account's. " +
			"Wait for the limit to lift and deploy again, or ask whoever runs the registry to raise it for that account."
	case hub:
		return "Add a registry credential to the app, a Docker Hub username and access token, so its downloads count " +
			"against that account instead of this server's address. A free Personal account allows 200 downloads every 6 hours, " +
			"and a paid Docker Hub plan has no limit. Then deploy again."
	default:
		return "Add a registry credential to the app so its downloads count against that account instead of this server's address, " +
			"or wait for the limit to lift. Then deploy again."
	}
}

// until says how far off t is, roughly: "in about 2 hours".
func until(now, t time.Time) string {
	d := t.Sub(now)
	switch {
	case d < time.Minute:
		return "in under a minute"
	case d < 90*time.Minute:
		m := int((d + 30*time.Second) / time.Minute)
		if m == 1 {
			return "in about 1 minute"
		}
		return fmt.Sprintf("in about %d minutes", m)
	default:
		return fmt.Sprintf("in about %d hours", int((d+30*time.Minute)/time.Hour))
	}
}
