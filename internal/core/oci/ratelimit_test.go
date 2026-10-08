package oci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// counted is a registry that counts manifest requests by method, the way
// Docker Hub counts pulls: a GET is a pull, a HEAD is not.
type counted struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *counted) n(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method]
}

func serveCounted(t *testing.T, wrap func(http.Handler) http.Handler) (string, *counted) {
	t.Helper()
	c := &counted{calls: map[string]int{}}
	h := registry.New()
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			c.mu.Lock()
			c.calls[r.Method]++
			c.mu.Unlock()
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), c
}

// TestR120_PinningATagSpendsNoPull asserts R-120's pin is resolved with a HEAD.
//
// Pinning an image deploy only needs the digest its tag names, and Docker Hub
// counts each manifest GET against its pull limit and a HEAD not at all.
func TestR120_PinningATagSpendsNoPull(t *testing.T) {
	host, c := serveCounted(t, nil)
	img := image(t, amd64)
	push(t, host+"/acme/web:1.4", img)
	want, err := img.Digest()
	require.NoError(t, err)
	before := c.n(http.MethodGet)

	got, err := oci.Inspector{Insecure: true}.Inspect(context.Background(), host+"/acme/web:1.4", nil, oci.Platform{})
	require.NoError(t, err)
	require.Equal(t, want.String(), got.Digest)
	require.Equal(t, before, c.n(http.MethodGet), "resolving only the digest must not GET the manifest")
	require.Positive(t, c.n(http.MethodHead))
	require.Nil(t, got.Config)

	// Configuration wanted: the GET is the point.
	_, err = oci.Inspector{Insecure: true}.Inspect(context.Background(), host+"/acme/web:1.4", nil, amd64)
	require.NoError(t, err)
	require.Greater(t, c.n(http.MethodGet), before)
}

// A registry that answers a HEAD without the digest is asked with a GET.
func TestADigestIsReadWithAGetWhenAHeadDoesNotSayIt(t *testing.T) {
	var pushed atomic.Bool
	host, c := serveCounted(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if pushed.Load() && r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/manifests/") {
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				w.Header().Set("Content-Length", "10")
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	img := image(t, amd64)
	push(t, host+"/acme/web:2", img)
	pushed.Store(true)
	want, err := img.Digest()
	require.NoError(t, err)

	got, err := oci.Inspector{Insecure: true}.Inspect(context.Background(), host+"/acme/web:2", nil, oci.Platform{})
	require.NoError(t, err)
	require.Equal(t, want.String(), got.Digest)
	require.Positive(t, c.n(http.MethodGet))
}

// A HEAD refused for a reason a GET shares is not asked again.
func TestAMissingImageIsSaidPlainlyWhenOnlyTheDigestIsWanted(t *testing.T) {
	host, c := serveCounted(t, nil)
	_, err := oci.Inspector{Insecure: true}.Inspect(context.Background(), host+"/acme/nothing:1", nil, oci.Platform{})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "has no image")
	require.Zero(t, c.n(http.MethodGet))
}

func limited(t *testing.T, header http.Header, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		for k, v := range header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

const hubBody = `{"errors":[{"code":"TOOMANYREQUESTS","message":"You have reached your pull rate limit."}]}`

// TestR105_ARegistrysDownloadLimitSaysWhoseLimitAndWhen asserts R-105 for a 429.
//
// A registry's download limit is not "check that the registry is reachable":
// it names the registry, says when it accepts downloads again where the
// registry said, and says what lifts it.
func TestR105_ARegistrysDownloadLimitSaysWhoseLimitAndWhen(t *testing.T) {
	at := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	host := limited(t, http.Header{"Retry-After": {at.Format(http.TimeFormat)}}, http.StatusTooManyRequests, hubBody)

	for _, want := range []oci.Platform{{}, amd64} {
		_, err := oci.Inspector{Insecure: true}.Inspect(context.Background(), host+"/acme/web:1", nil, want)
		e := errs.As(err)
		require.NotNil(t, e)
		require.Equal(t, errs.AdapterRegistryRateLimited, e.Code)
		require.Contains(t, e.Message, "The registry "+host+" is limiting how many images this server may download")
		require.Contains(t, e.Message, "the image "+host+"/acme/web:1")
		require.Contains(t, e.Message, at.Format("2006-01-02 15:04"))
		require.Contains(t, e.Message, "in about 2 hours")
		require.NotContains(t, e.Message, "reachable")
		require.Contains(t, e.Remedy, "Add a registry credential to the app")
		require.Equal(t, at.Format(time.RFC3339), e.Details["retry_at"])
		require.Equal(t, host, e.Details["registry"])
	}

	// Signed in: the limit is the account's, and a credential is not the fix.
	_, err := oci.Inspector{Insecure: true}.Inspect(context.Background(), host+"/acme/web:1",
		&oci.Auth{Username: "ben", Password: secret.New("token")}, oci.Platform{})
	require.Equal(t, errs.AdapterRegistryRateLimited, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "The app's registry credential was used")
}

// Docker Hub's own reset header, in seconds, and its error code arriving
// with another status, are read the same way.
func TestADownloadLimitIsReadFromRateLimitResetAndTheErrorCode(t *testing.T) {
	host := limited(t, http.Header{"Ratelimit-Reset": {"300"}}, http.StatusTooManyRequests, hubBody)
	_, err := oci.Inspector{Insecure: true}.Inspect(context.Background(), host+"/acme/web:1", nil, amd64)
	require.Equal(t, errs.AdapterRegistryRateLimited, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "in about 5 minutes")

	host = limited(t, nil, http.StatusForbidden, hubBody)
	_, err = oci.Inspector{Insecure: true}.Inspect(context.Background(), host+"/acme/web:1", nil, amd64)
	require.Equal(t, errs.AdapterRegistryRateLimited, errs.CodeOf(err))
	require.NotContains(t, errs.As(err).Message, "accepts downloads again")
}
