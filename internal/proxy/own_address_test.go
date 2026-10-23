package proxy_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/proxy"
)

// proxyFor is a proxy for one app, in front of upstream.
func proxyFor(t *testing.T, app state.App, routing spec.Routing, auth proxy.Authenticator, s *store, upstream string) *proxy.Proxy {
	t.Helper()
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	return &proxy.Proxy{
		Resolver: &resolver{app: app, spec: &spec.AppSpec{
			Routing: routing,
			Workloads: []spec.Workload{{Name: "web", Primary: true,
				Ports: []spec.Port{{Number: 80, Protocol: "http"}}}},
		}},
		Authenticator: auth,
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     fixedUpstream{addr: upstream},
		Logger:        zap.NewNop(),
	}
}

func okUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "the app")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestR166_AnAppOnItsOwnAddressIsNotServedOnPandos asserts that an app given
// a hostname or a port is served there and not also at /<slug> on Pando's own
// address. Served at /<slug>, its pages would run on Pando's origin, where
// nothing tells its script from the console's — the thing a hostname is given
// to prevent. A path-mode app is still found by its slug.
func TestR166_AnAppOnItsOwnAddressIsNotServedOnPandos(t *testing.T) {
	s := newStore()
	s.data[appID] = []string{"usr_alice"}
	upstream := okUpstream(t)
	app := state.App{ID: appID, Slug: "notes", State: state.StateRunning}

	cases := []struct {
		routing spec.Routing
		want    int
	}{
		{spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "notes.example.com"}, http.StatusNotFound},
		{spec.Routing{Mode: spec.RoutingPort, Port: 9001}, http.StatusNotFound},
		{spec.Routing{Mode: spec.RoutingPath, PathPrefix: "/team/notes"}, http.StatusOK},
	}
	for _, tc := range cases {
		p := proxyFor(t, app, tc.routing, staticAuth{principal: activeUser("usr_alice")}, s, upstream.URL)
		req := httptest.NewRequest(http.MethodGet, "http://pando.example.com/notes/", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		require.Equal(t, tc.want, rec.Code, "%s mode at /notes/", tc.routing.Mode)
	}

	// On its own hostname the subdomain app still answers.
	p := proxyFor(t, app, cases[0].routing, staticAuth{principal: activeUser("usr_alice")}, s, upstream.URL)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://notes.example.com/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "the app", rec.Body.String())
}

// TestR070_WhetherAnAppIsRunningIsToldOnlyToSomeoneAllowedIn asserts that the
// proxy decides access before it says an app is stopped, and why: the reason
// says whether its owners still have accounts, which is not a stranger's to
// learn by trying addresses.
func TestR070_WhetherAnAppIsRunningIsToldOnlyToSomeoneAllowedIn(t *testing.T) {
	app := state.App{ID: appID, Slug: "notes", State: state.StateStopped, StoppedForOrphan: true}
	routing := spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "notes.example.com"}

	get := func(p *proxy.Proxy) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://notes.example.com/", nil))
		return rec
	}

	anonymous := get(proxyFor(t, app, routing, staticAuth{principal: authz.Anonymous()}, newStore(), "http://unused"))
	require.Equal(t, http.StatusFound, anonymous.Code, "an anonymous caller is sent to sign in")
	require.NotContains(t, anonymous.Body.String(), "owners")

	stranger := get(proxyFor(t, app, routing, staticAuth{principal: activeUser("usr_mallory")}, newStore(), "http://unused"))
	require.Equal(t, http.StatusForbidden, stranger.Code)
	require.NotContains(t, stranger.Body.String(), "owners")

	s := newStore()
	s.data[appID] = []string{"usr_alice"}
	allowed := get(proxyFor(t, app, routing, staticAuth{principal: activeUser("usr_alice")}, s, "http://unused"))
	require.Equal(t, http.StatusServiceUnavailable, allowed.Code)
	require.Contains(t, allowed.Body.String(), "owners")
}

// streamingUpstream answers with an event stream that sends an event every
// few milliseconds until the request is canceled, and says when it was.
func streamingUpstream(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	ended := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer once.Do(func() { close(ended) })
		w.Header().Set("Content-Type", "text/event-stream")
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				if _, err := fmt.Fprint(w, "data: tick\n\n"); err != nil {
					return
				}
				http.NewResponseController(w).Flush() //nolint:errcheck // a test upstream
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, ended
}

// TestR048_RevokingASessionEndsItsOpenEventStream asserts that a response that
// streams — server-sent events, here — is re-authorized while it is open, as a
// websocket is, and ends when a check fails: the stream is cut and the request
// to the app is canceled. Before, only a hijacked connection was re-checked,
// so an event stream outlived the session it was opened with.
func TestR048_RevokingASessionEndsItsOpenEventStream(t *testing.T) {
	s := newStore()
	s.data[appID] = []string{"usr_alice"}
	auth := &liveAuth{principal: activeUser("usr_alice")}
	upstream, ended := streamingUpstream(t)
	p := proxyFor(t, state.App{ID: appID, Slug: "notes", State: state.StateRunning},
		spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "127.0.0.1"}, auth, s, upstream.URL)
	p.SetReauthInterval(20 * time.Millisecond)
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	resp, err := http.Get(front.URL + "/events")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	events := bufio.NewReader(resp.Body)

	// Still allowed: events keep coming past several checks.
	deadline := time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(deadline) {
		line, err := events.ReadString('\n')
		require.NoError(t, err, "the stream is open while access is")
		_ = line
	}
	require.Greater(t, auth.calls.Load(), int32(2), "the credentials were authenticated again")

	auth.set(authz.Anonymous())
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, events)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the event stream outlived the session")
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the request to the app was not canceled")
	}
}

// TestR048_ARequestStillWaitingOnTheAppIsRefusedOnRevocation asserts that a
// request the app has not answered yet — a long poll — is refused with a 403
// when access is revoked while it waits, not reported as the app failing.
func TestR048_ARequestStillWaitingOnTheAppIsRefusedOnRevocation(t *testing.T) {
	s := newStore()
	s.data[appID] = []string{"usr_alice"}
	suspended := activeUser("usr_alice")
	suspended.Status = "suspended"
	auth := &liveAuth{principal: activeUser("usr_alice")}
	waiting := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		auth.set(suspended)
		<-r.Context().Done()
	}))
	t.Cleanup(waiting.Close)
	p := proxyFor(t, state.App{ID: appID, Slug: "notes", State: state.StateRunning},
		spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "127.0.0.1"}, auth, s, waiting.URL)
	p.SetReauthInterval(20 * time.Millisecond)
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	resp, err := http.Get(front.URL + "/poll")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// TestR173_AnInformationalResponseCarriesOnlyLinks asserts that a 1xx response
// from an app — 103 Early Hints — reaches the browser with its Link headers and
// nothing else. ReverseProxy sends a 1xx's headers on directly, past the
// Set-Cookie filter every final response goes through, so an app could set
// Pando's session cookie on one.
func TestR173_AnInformationalResponseCarriesOnlyLinks(t *testing.T) {
	s := newStore()
	s.data[appID] = []string{"usr_alice"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Link", "</app.css>; rel=preload; as=style")
		w.Header().Add("Set-Cookie", "pando_session=ses_planted; Path=/")
		w.WriteHeader(http.StatusEarlyHints)
		_, _ = fmt.Fprint(w, "the app")
	}))
	t.Cleanup(upstream.Close)
	p := proxyFor(t, state.App{ID: appID, Slug: "notes", State: state.StateRunning},
		spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "127.0.0.1"},
		staticAuth{principal: activeUser("usr_alice")}, s, upstream.URL)
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	var mu sync.Mutex
	var hints []textproto.MIMEHeader
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
		mu.Lock()
		defer mu.Unlock()
		if code == http.StatusEarlyHints {
			hints = append(hints, h)
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, front.URL+"/", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, hints, 1, "the early hints arrived")
	require.Equal(t, []string{"</app.css>; rel=preload; as=style"}, hints[0].Values("Link"))
	for name := range hints[0] {
		require.Equal(t, "Link", name, "only Link is sent on a 1xx")
	}
	for _, c := range resp.Header.Values("Set-Cookie") {
		require.False(t, strings.HasPrefix(c, "pando_session="), "nor on the final response")
	}
}
