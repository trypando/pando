package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeDaemon is a stand-in for the Docker Engine API, for paths that depend on
// what the daemon answers — a refused network, a pull that fails inside its
// stream — and that a real daemon cannot be made to produce on demand. It
// touches nothing on the host.
type fakeDaemon struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	calls  []string
}

var apiVersionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

// newFakeDaemon starts a fake daemon and returns an adapter configured to talk
// to it, with extra configuration merged in.
func newFakeDaemon(t *testing.T, config map[string]any) (*fakeDaemon, *Adapter) {
	t.Helper()
	f := &fakeDaemon{t: t, routes: map[string]http.HandlerFunc{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	if config == nil {
		config = map[string]any{}
	}
	config["host"] = "tcp://" + strings.TrimPrefix(srv.URL, "http://")
	raw, err := json.Marshal(config)
	require.NoError(t, err)

	a := New()
	require.NoError(t, a.Configure(context.Background(), raw))
	return f, a
}

// on registers a handler for a method and a path with the API version removed,
// such as "POST /networks/create". A path ending in "*" matches any suffix.
func (f *fakeDaemon) on(route string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = h
}

func (f *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	path := apiVersionPrefix.ReplaceAllString(r.URL.Path, "")
	if path == "/_ping" {
		w.Header().Set("Api-Version", "1.47")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		return
	}
	key := r.Method + " " + path

	f.mu.Lock()
	f.calls = append(f.calls, key)
	h, ok := f.routes[key]
	if !ok {
		for route, candidate := range f.routes {
			if strings.HasSuffix(route, "*") && strings.HasPrefix(key, strings.TrimSuffix(route, "*")) {
				h, ok = candidate, true
				break
			}
		}
	}
	f.mu.Unlock()

	if !ok {
		// Every daemon answers these two; a test that cares registers its own.
		switch key {
		case "GET /version":
			writeJSON(w, http.StatusOK, map[string]any{"Version": "28.0.0", "ApiVersion": "1.47"})
		case "GET /info":
			writeJSON(w, http.StatusOK, map[string]any{"CpuCfsQuota": true, "CpuCfsPeriod": true, "MemoryLimit": true})
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such thing: " + key})
		}
		return
	}
	h(w, r)
}

// called returns how many requests matched a method and path prefix.
func (f *fakeDaemon) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func respond(status int, body any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, body) }
}
