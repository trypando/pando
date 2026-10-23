package proxy_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/proxy"
)

// auditLog records what the proxy writes.
type auditLog struct {
	mu     sync.Mutex
	events []audit.Event
	fail   bool
}

func (a *auditLog) Write(_ context.Context, e audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return errors.New("database unavailable")
	}
	a.events = append(a.events, e)
	return nil
}

func (a *auditLog) uses() []audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.Event
	for _, e := range a.events {
		if e.Action == "app.use" {
			out = append(out, e)
		}
	}
	return out
}

type usePolicy struct{ anonymous bool }

func (u usePolicy) RecordsAnonymousUse(context.Context) bool { return u.anonymous }

// useProxy is the proxy in front of a recording upstream, with an audit log
// and a clock a test moves.
func useProxy(t *testing.T, principal authz.Principal, configure func(*store)) (*proxy.Proxy, *auditLog, *time.Time, *received) {
	t.Helper()
	got := &received{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		fmt.Fprint(w, "upstream ok")
	}))
	t.Cleanup(upstream.Close)

	s := newStore()
	if configure != nil {
		configure(s)
	}
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	log := &auditLog{}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
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
		Auditor:       log,
		Logger:        zap.NewNop(),
		Mode:          spec.RoutingSubdomain,
	}
	p.SetClock(func() time.Time { return now })
	return p, log, &now, got
}

// response is what a test reads from one: its status and the cookies it set.
type response struct {
	StatusCode int
	cookies    []*http.Cookie
	setCookie  []string
}

// visit sends one request, carrying cookies, and returns the response.
func visit(p *proxy.Proxy, path string, cookies ...*http.Cookie) response {
	r := httptest.NewRequest(http.MethodGet, "http://notes.test"+path, nil)
	r.RemoteAddr = "203.0.113.7:51234"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return serve(p, r)
}

func serve(p *proxy.Proxy, r *http.Request) response {
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	return response{StatusCode: res.StatusCode, cookies: res.Cookies(), setCookie: res.Header.Values("Set-Cookie")}
}

func visitCookieOf(t *testing.T, resp response) *http.Cookie {
	t.Helper()
	for _, c := range resp.cookies {
		if c.Name == proxy.VisitCookie+appID {
			return c
		}
	}
	t.Fatalf("no visit cookie on the response; got %v", resp.setCookie)
	return nil
}

// TestR227_AppUseIsRecordedOncePerVisit asserts R-227: using an app writes
// app.use once for a browser's visit, not for every request in it, and a new
// visit — a new browser session — is recorded again.
func TestR227_AppUseIsRecordedOncePerVisit(t *testing.T) {
	alice := activeUser("usr_alice")
	p, log, _, _ := useProxy(t, alice, func(s *store) { s.data[appID] = []string{"usr_alice"} })

	first := visit(p, "/dashboard")
	require.Equal(t, http.StatusOK, first.StatusCode)
	cookie := visitCookieOf(t, first)
	require.True(t, cookie.HttpOnly)
	require.Equal(t, "/", cookie.Path)

	for _, path := range []string{"/app.js", "/style.css", "/api/items"} {
		require.Equal(t, http.StatusOK, visit(p, path, cookie).StatusCode)
	}

	uses := log.uses()
	require.Len(t, uses, 1, "a visit is one record, however many requests it makes")
	e := uses[0]
	require.Equal(t, audit.PrincipalKind(authz.KindUser), e.PrincipalKind)
	require.Equal(t, "usr_alice", e.PrincipalID)
	require.Equal(t, appID, e.AppID)
	require.Equal(t, "/dashboard", e.Detail["path"])
	require.Equal(t, cookie.Value, e.Detail["visit"], "the visit ties the record to the cookie")
	require.NotContains(t, e.Detail, "remote_addr", "a signed-in person is named; no address is needed")

	// A new browser session is a new visit.
	visit(p, "/")
	require.Len(t, log.uses(), 2)
}

// TestR173_TheVisitCookieNeverReachesTheApp asserts R-173 for the visit
// cookie: it is in Pando's namespace and stripped before the app sees the
// request.
func TestR173_TheVisitCookieNeverReachesTheApp(t *testing.T) {
	alice := activeUser("usr_alice")
	p, _, _, got := useProxy(t, alice, func(s *store) { s.data[appID] = []string{"usr_alice"} })
	cookie := visitCookieOf(t, visit(p, "/"))
	visit(p, "/next", cookie, &http.Cookie{Name: "app_pref", Value: "dark"})
	require.NotContains(t, got.header.Get("Cookie"), proxy.VisitCookie)
	require.Contains(t, got.header.Get("Cookie"), "app_pref=dark", "the app's own cookies still arrive")
}

// TestR227_AnonymousUseIsRecordedUnlessPolicyTurnsItOff asserts R-227 for
// visitors who are not signed in: recorded by default, with the address Pando
// saw, and not at all when host policy turns it off.
func TestR227_AnonymousUseIsRecordedUnlessPolicyTurnsItOff(t *testing.T) {
	public := func(s *store) { s.anonymous[appID] = true }

	p, log, _, _ := useProxy(t, authz.Anonymous(), public)
	cookie := visitCookieOf(t, visit(p, "/"))
	visit(p, "/more", cookie)
	uses := log.uses()
	require.Len(t, uses, 1)
	require.Equal(t, audit.PrincipalKind(authz.KindAnonymous), uses[0].PrincipalKind)
	require.Empty(t, uses[0].PrincipalID)
	require.Equal(t, "203.0.113.7", uses[0].Detail["remote_addr"])

	p, log, _, _ = useProxy(t, authz.Anonymous(), public)
	p.UsePolicy = usePolicy{anonymous: false}
	resp := visit(p, "/")
	require.Equal(t, http.StatusOK, resp.StatusCode, "the app is still served")
	require.Empty(t, log.uses())
	require.Empty(t, resp.setCookie, "nothing to mark when nothing is recorded")

	// Signed-in use is recorded whatever the anonymous setting.
	alice := activeUser("usr_alice")
	p, log, _, _ = useProxy(t, alice, func(s *store) { s.data[appID] = []string{"usr_alice"} })
	p.UsePolicy = usePolicy{anonymous: false}
	visit(p, "/")
	require.Len(t, log.uses(), 1)
}

// TestR227_ATokenIsOneVisitPerWindow asserts R-227 for tokens: an agent keeps
// no cookies, so the token is the visit, recorded once per window and again
// after it.
func TestR227_ATokenIsOneVisitPerWindow(t *testing.T) {
	agent := authz.Principal{Kind: authz.KindToken, ID: "tok_agent", UserID: "usr_alice", Status: "active"}
	p, log, now, _ := useProxy(t, agent, func(s *store) { s.data[appID] = []string{"usr_alice"} })

	for range 5 {
		resp := visit(p, "/api/items")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Empty(t, resp.setCookie, "no cookie for a client that keeps none")
	}
	uses := log.uses()
	require.Len(t, uses, 1)
	require.Equal(t, "tok_agent", uses[0].PrincipalID)
	require.Equal(t, "usr_alice", uses[0].OnBehalfOf, "whose token it was")

	*now = now.Add(proxy.UseWindow + time.Minute)
	visit(p, "/api/items")
	require.Len(t, log.uses(), 2, "a new window is a new visit")
}

// TestR227_ADeniedRequestIsNotAUse asserts that only allowed use is recorded:
// a refusal is app.use.denied, never app.use.
func TestR227_ADeniedRequestIsNotAUse(t *testing.T) {
	p, log, _, _ := useProxy(t, activeUser("usr_mallory"), nil)
	resp := visit(p, "/")
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Empty(t, log.uses())
	require.Len(t, log.events, 1)
	require.Equal(t, "app.use.denied", log.events[0].Action)
}

// A client that never keeps cookies is a new visit on every request. Anonymous
// ones are capped per app per minute, and what went unrecorded is counted onto
// the next record rather than lost.
func TestAnonymousUseIsCappedAndTheOverflowCounted(t *testing.T) {
	p, log, now, _ := useProxy(t, authz.Anonymous(), func(s *store) { s.anonymous[appID] = true })
	for range proxy.AnonymousUsePerMinute + 5 {
		require.Equal(t, http.StatusOK, visit(p, "/").StatusCode, "every request is still served")
	}
	require.Len(t, log.uses(), proxy.AnonymousUsePerMinute)

	*now = now.Add(time.Minute)
	visit(p, "/")
	uses := log.uses()
	require.Equal(t, 5, uses[len(uses)-1].Detail["unrecorded_before"])
}

// An audit row that cannot be written does not turn an authorized use into a
// refusal, and the visit is tried again on the next request.
func TestAFailedUseRecordDoesNotBlockTheApp(t *testing.T) {
	alice := activeUser("usr_alice")
	p, log, _, _ := useProxy(t, alice, func(s *store) { s.data[appID] = []string{"usr_alice"} })
	log.fail = true
	first := visit(p, "/")
	require.Equal(t, http.StatusOK, first.StatusCode)
	cookie := visitCookieOf(t, first)

	log.fail = false
	visit(p, "/again", cookie)
	uses := log.uses()
	require.Len(t, uses, 1, "recorded once the log is writable")
	require.Equal(t, "/again", uses[0].Detail["path"])
}

// In path mode the visit cookie is scoped to the app's prefix, so one app's
// visit is not another's.
func TestTheVisitCookieIsScopedToThePathPrefix(t *testing.T) {
	p, _, _, _ := useProxy(t, activeUser("usr_alice"), func(s *store) { s.data[appID] = []string{"usr_alice"} })
	// A path-mode app: only one is served at a path on Pando's address (R-166).
	p.Resolver.(*resolver).spec.Routing = spec.Routing{Mode: spec.RoutingPath}
	r := httptest.NewRequest(http.MethodGet, "http://pando.test/notes/dashboard", nil)
	resp := serve(p, r)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cookie := visitCookieOf(t, resp)
	require.Equal(t, "/notes", cookie.Path)
	require.True(t, strings.HasPrefix(cookie.Value, "vis_"))
}

// The visit cookie is marked Secure by the session cookie's rule (O-19): the
// external URL's scheme, since behind a TLS-terminating proxy the request
// itself arrives as plain HTTP.
func TestTheVisitCookieIsSecureBehindAnHTTPSExternalURL(t *testing.T) {
	p, _, _, _ := useProxy(t, activeUser("usr_alice"), func(s *store) { s.data[appID] = []string{"usr_alice"} })
	require.False(t, visitCookieOf(t, visit(p, "/")).Secure, "plain HTTP with no external URL")

	p, _, _, _ = useProxy(t, activeUser("usr_alice"), func(s *store) { s.data[appID] = []string{"usr_alice"} })
	p.ExternalURL = &url.URL{Scheme: "https", Host: "pando.example.com"}
	require.True(t, visitCookieOf(t, visit(p, "/")).Secure)
}
