package oci_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/secret"
)

// hostsAsked records which hosts a read went to, and answers none but the
// mirror's: nothing in these tests reaches Docker Hub itself.
type hostsAsked struct {
	mu     sync.Mutex
	mirror string
	asked  []string
}

func (h *hostsAsked) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.asked = append(h.asked, r.URL.Host)
	h.mu.Unlock()
	if r.URL.Host != h.mirror {
		return nil, errors.New("not reachable in this test")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (h *hostsAsked) went(host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.asked {
		if a == host {
			return true
		}
	}
	return false
}

// TestR094_DockerHubImagesAreReadThroughTheRuntimesMirror asserts that a read
// of a Docker Hub image goes where the runtime pulls it from. Docker Hub counts
// every manifest read against a limit of 100 an hour for an address, and a
// benchmark whose daemon pulled through a mirror ran out of it on Pando's own
// reads (issue #90). R-094's check of Docker Hub is unchanged; it goes through
// the mirror the operator configured, and to Docker Hub when that fails.
func TestR094_DockerHubImagesAreReadThroughTheRuntimesMirror(t *testing.T) {
	mirrorSrv := httptest.NewServer(registry.New())
	t.Cleanup(mirrorSrv.Close)
	mirror := strings.TrimPrefix(mirrorSrv.URL, "http://")
	img := image(t, amd64)
	push(t, mirror+"/library/nginx:1.27", img)
	want, err := img.Digest()
	require.NoError(t, err)

	inspector := func(asked *hostsAsked, mirrors ...string) oci.Inspector {
		return oci.Inspector{Transport: asked, Mirrors: func(context.Context) []string { return mirrors }}
	}

	t.Run("the mirror answers", func(t *testing.T) {
		asked := &hostsAsked{mirror: mirror}
		got, err := inspector(asked, "http://"+mirror+"/").Inspect(context.Background(), "nginx:1.27", nil, amd64)
		require.NoError(t, err)
		require.Equal(t, want.String(), got.Digest)
		require.Equal(t, "nginx:1.27", got.Reference, "the reference stays as the person gave it")
		require.NotNil(t, got.Config)
		require.False(t, asked.went("index.docker.io"), "Docker Hub was not asked")
	})

	t.Run("a mirror without the image falls back to Docker Hub", func(t *testing.T) {
		asked := &hostsAsked{mirror: mirror}
		_, err := inspector(asked, "http://"+mirror).Inspect(context.Background(), "redis:7", nil, amd64)
		require.Error(t, err)
		require.True(t, asked.went(mirror))
		require.True(t, asked.went("index.docker.io"))
	})

	t.Run("a mirror under a path is passed over", func(t *testing.T) {
		asked := &hostsAsked{mirror: mirror}
		_, err := inspector(asked, "http://"+mirror+"/hub").Inspect(context.Background(), "nginx:1.27", nil, amd64)
		require.Error(t, err)
		require.False(t, asked.went(mirror))
		require.True(t, asked.went("index.docker.io"))
	})

	t.Run("a read with a credential goes to Docker Hub", func(t *testing.T) {
		asked := &hostsAsked{mirror: mirror}
		auth := &oci.Auth{Registry: "index.docker.io", Username: "acme", Password: secret.New("token")}
		_, err := inspector(asked, "http://"+mirror).Inspect(context.Background(), "acme/private:1", auth, amd64)
		require.Error(t, err)
		require.False(t, asked.went(mirror))
		require.True(t, asked.went("index.docker.io"))
	})

	t.Run("another registry is read directly", func(t *testing.T) {
		asked := &hostsAsked{mirror: mirror}
		_, err := inspector(asked, "http://"+mirror).Inspect(context.Background(), "ghcr.io/acme/web:1", nil, amd64)
		require.Error(t, err)
		require.False(t, asked.went(mirror))
		require.True(t, asked.went("ghcr.io"))
	})
}
