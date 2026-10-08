package registryprobe_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect/registryprobe"
)

// registry stands in for ghcr.io and hub.docker.com. The probe's URLs are
// absolute, so the transport below redirects them here rather than the test
// rewriting the code's idea of where a registry lives.
type registry struct {
	*httptest.Server
	paths map[string]func(http.ResponseWriter)
	seen  []string
}

func newRegistry(t *testing.T) *registry {
	t.Helper()
	r := &registry{paths: map[string]func(http.ResponseWriter){}}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := req.Host + req.URL.Path
		r.seen = append(r.seen, req.Method+" "+key+"?"+req.URL.RawQuery)
		if handler, ok := r.paths[key]; ok {
			handler(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *registry) on(hostPath string, handler func(http.ResponseWriter)) *registry {
	r.paths[hostPath] = handler
	return r
}

func json200(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func status(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

// redirect sends every request to the test server, keeping the original Host
// so the handler can tell ghcr.io from hub.docker.com.
type redirect struct {
	to  string
	err error
}

func (t redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.err != nil {
		return nil, t.err
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = t.to
	return http.DefaultTransport.RoundTrip(clone)
}

func probeAgainst(r *registry, includeHub bool) *registryprobe.Probe {
	p := registryprobe.New()
	p.HTTP = &http.Client{Transport: redirect{to: r.Listener.Addr().String()}}
	p.IncludeDockerHub = includeHub
	return p
}

func git(url string) spec.Source { return spec.Source{Type: spec.SourceGit, URL: url} }

// R-094 tier 1: an image the maintainers publish themselves is their own answer
// to "how is this built", already built and already shipped.
func TestR094_APublishedImageOnGHCRIsFound(t *testing.T) {
	r := newRegistry(t).
		on("ghcr.io/token", json200(`{"token":"anonymous-pull-token"}`)).
		on("ghcr.io/v2/acme/notes/manifests/latest", status(http.StatusOK))

	found, err := probeAgainst(r, false).Published(context.Background(),
		git("https://github.com/acme/notes"))

	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, "ghcr.io/acme/notes:latest", found[0].Ref)
	require.Equal(t, "ghcr.io", found[0].Registry)
}

// Anonymous pulls need a token even for public images, so it is two calls — and
// without the Accept header a multi-architecture image, which is most of them,
// answers 404 rather than returning its index.
func TestTheManifestRequestAsksForAnIndexAsWellAsAManifest(t *testing.T) {
	var accept, authorization string
	r := newRegistry(t)

	// This one inspects the manifest request itself rather than only its path,
	// so it replaces the routing handler outright.
	r.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/token" {
			json200(`{"token":"anonymous-pull-token"}`)(w)
			return
		}
		accept = req.Header.Get("Accept")
		authorization = req.Header.Get("Authorization")
		require.Equal(t, http.MethodHead, req.Method, "a manifest is checked, not downloaded")
		w.WriteHeader(http.StatusOK)
	})

	_, err := probeAgainst(r, false).Published(context.Background(), git("https://github.com/acme/notes"))
	require.NoError(t, err)

	require.Equal(t, "Bearer anonymous-pull-token", authorization)
	require.Contains(t, accept, "application/vnd.oci.image.index.v1+json")
	require.Contains(t, accept, "application/vnd.docker.distribution.manifest.list.v2+json")
}

func TestNoTokenMeansNoLookup(t *testing.T) {
	r := newRegistry(t).on("ghcr.io/token", status(http.StatusUnauthorized))

	found, err := probeAgainst(r, false).Published(context.Background(), git("https://github.com/acme/notes"))
	require.NoError(t, err)
	require.Empty(t, found)

	for _, seen := range r.seen {
		require.NotContains(t, seen, "manifests", "the manifest is not asked for without a token")
	}
}

func TestATokenResponseThatIsNotJSONIsNoToken(t *testing.T) {
	r := newRegistry(t).on("ghcr.io/token", json200(`not json`))

	found, err := probeAgainst(r, false).Published(context.Background(), git("https://github.com/acme/notes"))
	require.NoError(t, err)
	require.Empty(t, found)
}

func TestAnImageThatIsNotThereIsNotProposed(t *testing.T) {
	r := newRegistry(t).
		on("ghcr.io/token", json200(`{"token":"anonymous-pull-token"}`)).
		on("ghcr.io/v2/acme/notes/manifests/latest", status(http.StatusNotFound))

	found, err := probeAgainst(r, false).Published(context.Background(), git("https://github.com/acme/notes"))
	require.NoError(t, err)
	require.Empty(t, found)
}

// Best effort on the path of every app anyone adds: a registry that is down
// must not fail detection.
func TestAnUnreachableRegistryIsNotAnError(t *testing.T) {
	p := registryprobe.New()
	p.HTTP = &http.Client{Transport: redirect{err: errors.New("no route to host")}}
	p.IncludeDockerHub = true

	found, err := p.Published(context.Background(), git("https://github.com/acme/notes"))
	require.NoError(t, err)
	require.Empty(t, found)
}

// On Docker Hub the namespace is an unrelated account: anyone may register the
// user "acme" regardless of who owns github.com/acme, so a match proves only
// that the names are the same. Defaulting it on would mean Pando proposing to
// run a stranger's image, described as the project's own.
func TestDockerHubIsOffUnlessAskedFor(t *testing.T) {
	r := newRegistry(t).
		on("ghcr.io/token", status(http.StatusUnauthorized)).
		on("hub.docker.com/v2/repositories/acme/notes/", json200(`{"is_private":false,"pull_count":9000}`))

	found, err := probeAgainst(r, false).Published(context.Background(), git("https://github.com/acme/notes"))
	require.NoError(t, err)
	require.Empty(t, found)

	for _, seen := range r.seen {
		require.NotContains(t, seen, "hub.docker.com", "Hub is not even asked")
	}

	found, err = probeAgainst(r, true).Published(context.Background(), git("https://github.com/acme/notes"))
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, "docker.io/acme/notes:latest", found[0].Ref)
	require.Equal(t, "docker.io", found[0].Registry)
}

func TestAPrivateOrMissingHubRepositoryIsNotAnImage(t *testing.T) {
	for _, body := range []string{
		`{"is_private":true,"pull_count":9000}`,
		`not json`,
	} {
		r := newRegistry(t).
			on("ghcr.io/token", status(http.StatusUnauthorized)).
			on("hub.docker.com/v2/repositories/acme/notes/", json200(body))

		found, err := probeAgainst(r, true).Published(context.Background(), git("https://github.com/acme/notes"))
		require.NoError(t, err)
		require.Empty(t, found, body)
	}

	// Not there at all.
	r := newRegistry(t).on("ghcr.io/token", status(http.StatusUnauthorized))
	found, err := probeAgainst(r, true).Published(context.Background(), git("https://github.com/acme/notes"))
	require.NoError(t, err)
	require.Empty(t, found)
}

func TestBothRegistriesCanAnswer(t *testing.T) {
	r := newRegistry(t).
		on("ghcr.io/token", json200(`{"token":"t"}`)).
		on("ghcr.io/v2/acme/notes/manifests/latest", status(http.StatusOK)).
		on("hub.docker.com/v2/repositories/acme/notes/", json200(`{"is_private":false}`))

	found, err := probeAgainst(r, true).Published(context.Background(), git("https://github.com/acme/notes"))
	require.NoError(t, err)
	require.Len(t, found, 2)
	require.Equal(t, "ghcr.io", found[0].Registry, "ghcr is the one with a real namespace correspondence")
	require.Equal(t, "docker.io", found[1].Registry)
}

// An unrecognized URL is not an error — it is a repository Pando has no
// registry convention for, and tier 1 is simply skipped.
func TestOwnerAndNameAreReadFromTheFormsPeopleActuallyUse(t *testing.T) {
	r := newRegistry(t).
		on("ghcr.io/token", json200(`{"token":"t"}`)).
		on("ghcr.io/v2/acme/notes/manifests/latest", status(http.StatusOK))
	p := probeAgainst(r, false)

	for _, url := range []string{
		"https://github.com/acme/notes",
		"https://github.com/acme/notes.git",
		"https://github.com/ACME/Notes",
		"https://www.github.com/acme/notes",
		"https://gitlab.com/acme/notes",
		"https://codeberg.org/acme/notes",
		"git@github.com:acme/notes.git",
		"  https://github.com/acme/notes  ",
	} {
		found, err := p.Published(context.Background(), git(url))
		require.NoError(t, err, url)
		require.Len(t, found, 1, url)
		require.Equal(t, "ghcr.io/acme/notes:latest", found[0].Ref, url)
	}
}

func TestAURLWithNoRegistryConventionIsSkipped(t *testing.T) {
	r := newRegistry(t)
	p := probeAgainst(r, true)

	for _, url := range []string{
		"", "   ",
		"https://git.corp.example/acme/notes",
		"https://github.com/acme",
		"https://github.com/",
		"git@github.com/acme/notes",
		"://not a url",
	} {
		found, err := p.Published(context.Background(), git(url))
		require.NoError(t, err, url)
		require.Empty(t, found, url)
	}
	require.Empty(t, r.seen, "nothing unrecognized reaches a registry")
}

// A subdir means the app is one project inside a larger repository, and the
// repository's own published image is not it.
func TestARepositorySubdirectorySkipsTierOne(t *testing.T) {
	r := newRegistry(t).
		on("ghcr.io/token", json200(`{"token":"t"}`)).
		on("ghcr.io/v2/acme/notes/manifests/latest", status(http.StatusOK))

	src := git("https://github.com/acme/notes")
	src.Subdir = "services/api"

	found, err := probeAgainst(r, true).Published(context.Background(), src)
	require.NoError(t, err)
	require.Empty(t, found)
	require.Empty(t, r.seen)
}

// This is a best-effort lookup on the path of every app anyone adds, and a slow
// registry must not hold up detection.
func TestTheDefaultProbeHasAShortTimeout(t *testing.T) {
	p := registryprobe.New()
	require.NotNil(t, p.HTTP)
	require.NotZero(t, p.HTTP.Timeout)
	require.LessOrEqual(t, p.HTTP.Timeout.Seconds(), 10.0)
	require.False(t, p.IncludeDockerHub, "off by default")
}
