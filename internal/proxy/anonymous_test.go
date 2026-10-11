package proxy_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/clientaddr"
	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/proxy"
)

// limitedProxy is a proxy in front of a public app, on a clock that only moves
// when the test moves it.
func limitedProxy(t *testing.T, principal authz.Principal, now *time.Time) (*proxy.Proxy, *proxy.Counters) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "upstream ok")
	}))
	t.Cleanup(upstream.Close)

	s := newStore()
	s.anonymous[appID] = true
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	counters := proxy.NewCounters()
	p := &proxy.Proxy{
		Resolver: &resolver{
			app: state.App{ID: appID, Slug: "notes", State: state.StateRunning},
			spec: &spec.AppSpec{
				Routing: spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "notes.test"},
				Workloads: []spec.Workload{{Name: "web", Primary: true,
					Ports: []spec.Port{{Number: 80, Protocol: "http"}}}},
			},
		},
		Authenticator: staticAuth{principal: principal},
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     fixedUpstream{addr: upstream.URL},
		Metrics:       counters,
		Logger:        zap.NewNop(),
		Mode:          spec.RoutingSubdomain,
	}
	p.SetClock(func() time.Time { return *now })
	return p, counters
}

// visitFrom makes one request to the app from addr.
func visitFrom(p *proxy.Proxy, addr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://notes.test/", nil)
	req.RemoteAddr = addr + ":40000"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestR075_AnonymousRequestsAreRateLimited asserts R-075's "rate-limited":
// one address sending anonymous requests to a public app faster than the limit
// is refused with a 429 that says when to try again, while another address,
// and a signed-in caller, are not; and the refused request is still counted,
// so the limit is not a path around the proxy's accounting (R-023).
func TestR075_AnonymousRequestsAreRateLimited(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	p, counters := limitedProxy(t, authz.Anonymous(), &now)

	// A burst far beyond any page and its assets, from one address, with no
	// time passing.
	var limited *httptest.ResponseRecorder
	served := 0
	for range 1000 {
		rec := visitFrom(p, "198.51.100.7")
		if rec.Code == http.StatusTooManyRequests {
			limited = rec
			break
		}
		require.Equal(t, http.StatusOK, rec.Code)
		served++
	}
	require.NotNil(t, limited, "an address flooding a public app is limited")
	require.GreaterOrEqual(t, served, 100, "a page and its assets load at once without meeting the limit")
	require.Equal(t, "1", limited.Header().Get("Retry-After"))
	require.Contains(t, limited.Body.String(), "Too many requests to this app")

	total, _, _, anonymous := counters.Snapshot(appID)
	require.Equal(t, int64(served+1), total, "the refused request is counted like any other")
	require.Equal(t, int64(served+1), anonymous)

	// Another address has its own allowance.
	require.Equal(t, http.StatusOK, visitFrom(p, "203.0.113.9").Code)

	// The allowance comes back with time.
	now = now.Add(time.Second)
	require.Equal(t, http.StatusOK, visitFrom(p, "198.51.100.7").Code)
}

// TestR075_TheAnonymousLimitIsPerClientNotPerProxy asserts that the limit is
// kept per client address as the audit log resolves it (design 12 §3.2): two
// visitors arriving through one trusted proxy do not share an allowance.
func TestR075_TheAnonymousLimitIsPerClientNotPerProxy(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	p, _ := limitedProxy(t, authz.Anonymous(), &now)
	trusted, err := clientaddr.Parse("10.0.0.5")
	require.NoError(t, err)
	front := trusted.Middleware(p)

	via := func(client string) int {
		req := httptest.NewRequest(http.MethodGet, "http://notes.test/", nil)
		req.RemoteAddr = "10.0.0.5:40000"
		req.Header.Set("X-Forwarded-For", client)
		rec := httptest.NewRecorder()
		front.ServeHTTP(rec, req)
		return rec.Code
	}
	for via("198.51.100.7") != http.StatusTooManyRequests {
	}
	require.Equal(t, http.StatusOK, via("203.0.113.9"),
		"a second visitor behind the same proxy is not limited by the first")
}

// TestR075_SignedInRequestsAreNotHeldToTheAnonymousLimit asserts the limit is
// on anonymous requests only, as R-075 states it.
func TestR075_SignedInRequestsAreNotHeldToTheAnonymousLimit(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	alice := activeUser("usr_alice")
	p, _ := limitedProxy(t, alice, &now)
	for range 1000 {
		require.NotEqual(t, http.StatusTooManyRequests, visitFrom(p, "198.51.100.7").Code)
	}
}
