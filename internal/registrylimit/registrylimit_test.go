package registrylimit_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/registrylimit"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func TestTheRefusalIsRecognizedInEachWayItIsPrinted(t *testing.T) {
	require.True(t, registrylimit.Mentioned("toomanyrequests: You have reached your pull rate limit"))
	require.True(t, registrylimit.Mentioned("unexpected status code 429 Too Many Requests"))
	require.True(t, registrylimit.Mentioned("TOOMANYREQUESTS"))
	require.False(t, registrylimit.Mentioned("manifest unknown"))
}

func TestTheRegistryAndImageAreReadFromTheText(t *testing.T) {
	r := registrylimit.FromText(`failed to resolve source metadata for ghcr.io/acme/base:1: 429 Too Many Requests`, "")
	require.Equal(t, "ghcr.io/acme/base:1", r.Image)
	require.Equal(t, "ghcr.io", r.Registry)

	r = registrylimit.FromText(`GET https://quay.io/v2/acme/x/manifests/1: toomanyrequests`, "")
	require.Empty(t, r.Image)
	require.Equal(t, "quay.io", r.Registry)

	r = registrylimit.FromText("toomanyrequests", "nginx:1.27")
	require.Equal(t, "docker.io", r.Registry)

	require.Equal(t, "localhost", registrylimit.Host("localhost/acme/web"))
	require.Equal(t, "localhost:5000", registrylimit.Host("localhost:5000/acme/web"))
	require.Equal(t, "docker.io", registrylimit.Host("acme/web"))
	require.True(t, registrylimit.DockerHub("registry-1.docker.io"))
	require.False(t, registrylimit.DockerHub("ghcr.io"))
}

func TestWhenToTryAgainIsReadFromTheHeaders(t *testing.T) {
	require.Equal(t, now.Add(90*time.Second), registrylimit.RetryAt(http.Header{"Retry-After": {"90"}}, now))
	date := now.Add(3 * time.Hour)
	require.Equal(t, date, registrylimit.RetryAt(http.Header{"Retry-After": {date.Format(http.TimeFormat)}}, now))
	require.Equal(t, now.Add(time.Hour), registrylimit.RetryAt(http.Header{"Ratelimit-Reset": {"3600"}}, now))
	require.Equal(t, time.Unix(1_800_000_000, 0).UTC(), registrylimit.RetryAt(http.Header{"Ratelimit-Reset": {"1800000000"}}, now))
	require.True(t, registrylimit.RetryAt(http.Header{"Retry-After": {"soon"}}, now).IsZero())
	require.True(t, registrylimit.RetryAt(nil, now).IsZero())
}

// TestR105_TheRefusalSaysWhoseLimitWhenAndWhatLiftsIt asserts R-105: each
// case names the registry, says when where it is known, and gives a fix that
// applies to it — a credential for an anonymous pull, a paid plan or a wait
// for a signed-in one, another registry for a build's base image.
func TestR105_TheRefusalSaysWhoseLimitWhenAndWhatLiftsIt(t *testing.T) {
	cause := errors.New("toomanyrequests")

	hub := registrylimit.Refusal{Registry: "index.docker.io", Image: "nginx:1.27", Signed: registrylimit.Anonymous}.Error(now, cause)
	require.Equal(t, errs.AdapterRegistryRateLimited, hub.Code)
	require.Equal(t, 502, hub.Status())
	require.Contains(t, hub.Message, "Docker Hub is limiting how many images this server may download, and refused to send the image nginx:1.27.")
	require.Contains(t, hub.Message, "100 downloads every 6 hours")
	require.Contains(t, hub.Message, "lifts within 6 hours")
	require.Contains(t, hub.Remedy, "200 downloads every 6 hours")
	require.Contains(t, hub.Remedy, "paid Docker Hub plan has no limit")
	require.ErrorIs(t, hub, cause)

	signed := registrylimit.Refusal{Registry: "docker.io", Signed: registrylimit.SignedIn, RetryAt: now.Add(45 * time.Minute)}.Error(now, cause)
	require.Contains(t, signed.Message, "refused to send an image.")
	require.Contains(t, signed.Message, "after 2026-10-07 12:45 UTC, in about 45 minutes")
	require.NotContains(t, signed.Message, "100 downloads")
	require.Contains(t, signed.Remedy, "paid Docker Hub plan")
	require.Equal(t, "2026-10-07T12:45:00Z", signed.Details["retry_at"])

	other := registrylimit.Refusal{Registry: "ghcr.io", Image: "ghcr.io/acme/web:1", RetryAt: now.Add(20 * time.Second)}.Error(now, cause)
	require.Contains(t, other.Message, "The registry ghcr.io is limiting")
	require.Contains(t, other.Message, "in under a minute")
	require.NotContains(t, other.Remedy, "Docker Hub")
	require.Contains(t, other.Remedy, "Add a registry credential to the app")

	otherSigned := registrylimit.Refusal{Registry: "ghcr.io", Signed: registrylimit.SignedIn, RetryAt: now.Add(time.Minute)}.Error(now, cause)
	require.Contains(t, otherSigned.Message, "in about 1 minute")
	require.Contains(t, otherSigned.Remedy, "ask whoever runs the registry")

	build := registrylimit.Refusal{Registry: "quay.io", Build: true, RetryAt: now.Add(5 * time.Hour)}.Error(now, cause)
	require.Contains(t, build.Message, "a base image the build starts from")
	require.Contains(t, build.Message, "in about 5 hours")
	require.Contains(t, build.Remedy, "FROM line")
	require.NotContains(t, build.Remedy, "public.ecr.aws")

	unnamed := registrylimit.Refusal{}.Error(now, cause)
	require.Contains(t, unnamed.Message, "The registry is limiting")
	require.NotContains(t, unnamed.Details, "registry")
}
