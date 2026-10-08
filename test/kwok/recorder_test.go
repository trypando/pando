//go:build kwokscale

package kwok_test

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// recorder times every request the adapters make, by verb and resource:
// "create pods", "get namespaces", "list services". It sits in the client's
// transport, below client-go's rate limiter, so a time is the API server's
// answer plus the network, never time spent waiting for a token.
type recorder struct {
	mu    sync.Mutex
	calls map[string][]time.Duration
	fails map[string]int
}

func newRecorder() *recorder {
	return &recorder{calls: map[string][]time.Duration{}, fails: map[string]int{}}
}

func (r *recorder) wrap(rt http.RoundTripper) http.RoundTripper {
	return roundTripper(func(req *http.Request) (*http.Response, error) {
		start := time.Now()
		resp, err := rt.RoundTrip(req)
		key := opOf(req.Method, req.URL.Path)
		r.mu.Lock()
		r.calls[key] = append(r.calls[key], time.Since(start))
		if err != nil || resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			r.fails[key]++
		}
		r.mu.Unlock()
		return resp, err
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// take returns what was recorded since the last take, and starts afresh.
func (r *recorder) take() snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := snapshot{calls: r.calls, fails: r.fails}
	r.calls = map[string][]time.Duration{}
	r.fails = map[string]int{}
	return s
}

type snapshot struct {
	calls map[string][]time.Duration
	fails map[string]int
}

func (s snapshot) total() int {
	n := 0
	for _, d := range s.calls {
		n += len(d)
	}
	return n
}

func (s snapshot) failures() int {
	n := 0
	for _, f := range s.fails {
		n += f
	}
	return n
}

// all is every call's time, whatever its kind.
func (s snapshot) all() []time.Duration {
	var out []time.Duration
	for _, d := range s.calls {
		out = append(out, d...)
	}
	return out
}

// table is one row per kind of call, the most frequent first.
func (s snapshot) table() string {
	keys := make([]string, 0, len(s.calls))
	for k := range s.calls {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(s.calls[keys[i]]) != len(s.calls[keys[j]]) {
			return len(s.calls[keys[i]]) > len(s.calls[keys[j]])
		}
		return keys[i] < keys[j]
	})
	var b strings.Builder
	b.WriteString("| call | count | failed | p50 | p95 | p99 | max |\n|---|---:|---:|---:|---:|---:|---:|\n")
	for _, k := range keys {
		q := quantiles(s.calls[k])
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %s | %s | %s |\n", k, len(s.calls[k]), s.fails[k], ms(q.p50), ms(q.p95), ms(q.p99), ms(q.max))
	}
	return b.String()
}

type quants struct{ p50, p95, p99, max time.Duration }

func quantiles(d []time.Duration) quants {
	if len(d) == 0 {
		return quants{}
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(q float64) time.Duration { return s[min(len(s)-1, int(q*float64(len(s))))] }
	return quants{p50: at(0.50), p95: at(0.95), p99: at(0.99), max: s[len(s)-1]}
}

func ms(d time.Duration) string {
	switch {
	case d == 0:
		return "–"
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
	case d < 10*time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	default:
		return d.Round(time.Second).String()
	}
}

// opOf names a request by verb and resource, from its path:
//
//	/api/v1/namespaces/{ns}/{resource}[/{name}[/{subresource}]]
//	/api/v1/namespaces[/{name}]
//	/apis/{group}/{version}/...      the same, under a group
//	/api/v1/{resource}               a cluster-wide list
func opOf(method, path string) string {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(seg) >= 2 && seg[0] == "api":
		seg = seg[2:]
	case len(seg) >= 3 && seg[0] == "apis":
		seg = seg[3:]
	default:
		return strings.ToLower(method) + " " + path
	}
	resource, named, scope := "", false, ""
	switch {
	case len(seg) == 0:
		return strings.ToLower(method) + " " + path
	case seg[0] == "namespaces" && len(seg) <= 2:
		resource, named = "namespaces", len(seg) == 2
	case seg[0] == "namespaces":
		resource, named = seg[2], len(seg) >= 4
		if len(seg) >= 5 {
			resource += "/" + seg[4]
		}
	default:
		resource, named, scope = seg[0], len(seg) >= 2, " (cluster-wide)"
	}
	verb := map[string]string{"POST": "create", "PUT": "update", "PATCH": "patch", "DELETE": "delete"}[method]
	if verb == "" {
		verb = "get"
		if !named {
			verb = "list"
		}
	}
	if !named || resource == "namespaces" {
		return verb + " " + resource + scope
	}
	return verb + " " + resource
}
