package multidocker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

// fakeDaemon stands in for one host's Docker Engine API, answering only the
// paths a test registers. It touches nothing on the machine running the test.
type fakeDaemon struct {
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	calls  []string
	bodies map[string][]byte
	cli    *client.Client
}

var apiVersionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	f := &fakeDaemon{routes: map[string]http.HandlerFunc{}, bodies: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	cli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(srv.URL, "http://")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	f.cli = cli
	return f
}

// on registers a handler for "METHOD /path" with the API version removed. A
// route ending in "*" matches any suffix.
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
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.bodies[key] = body
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
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such thing: " + key})
		return
	}
	h(w, r)
}

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

func (f *fakeDaemon) body(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func respond(status int, body any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, body) }
}

func noContent(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
