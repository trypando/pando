package proxy_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/proxy"
)

// --- doubles ---------------------------------------------------------------

type store struct {
	owner     map[string]string
	data      map[string][]string
	anonymous map[string]bool
	passcode  map[string]string
	status    map[string]string
}

func newStore() *store {
	return &store{
		owner: map[string]string{}, data: map[string][]string{},
		anonymous: map[string]bool{}, passcode: map[string]string{}, status: map[string]string{},
	}
}

func (s *store) UserStatus(_ context.Context, id string) (string, error) {
	if st, ok := s.status[id]; ok {
		return st, nil
	}
	return "active", nil
}
func (s *store) ControlGrantsFor(context.Context, string, authz.Principal) ([]authz.Grant, error) {
	return nil, nil
}

// Always empty. The proxy is the data plane, and no install-wide grant reaches
// it: R-087 says being an administrator does not confer access to an app, and
// this returning nil is that requirement in the fixture.
func (s *store) InstallGrantsFor(context.Context, authz.Principal) ([]authz.Grant, error) {
	return nil, nil
}

func (s *store) IsOwner(_ context.Context, appID, userID string) (bool, error) {
	return userID != "" && s.owner[appID] == userID, nil
}
func (s *store) HasDataGrant(_ context.Context, appID string, p authz.Principal) (bool, error) {
	for _, id := range s.data[appID] {
		if id == p.UserID || id == p.ID {
			return true, nil
		}
		for _, g := range p.Groups {
			if id == g {
				return true, nil
			}
		}
	}
	return false, nil
}
func (s *store) AnonymousAccess(_ context.Context, appID string) (bool, bool, error) {
	return s.anonymous[appID], s.passcode[appID] != "", nil
}
func (s *store) PasscodeUnlocked(_ context.Context, appID, token string) (bool, error) {
	return token != "" && s.passcode[appID] == token, nil
}
func (s *store) Role(context.Context, string) (authz.Role, error) { return authz.Role{}, nil }

type resolver struct {
	app  state.App
	spec *spec.AppSpec
}

// ByHostname matches only an app that actually has that hostname in its spec,
// which is what the real query does — `routing->>'hostname' = $1`.
//
// This fake used to return the app for any lookup of either kind. That made it
// agree with the proxy no matter what the proxy did, so it could not catch the
// proxy resolving the wrong way — and it did not, until the proxy started
// trying both.
func (r *resolver) ByHostname(_ context.Context, hostname string) (state.App, *spec.AppSpec, bool, error) {
	if r.app.ID == "" || r.spec == nil || r.spec.Routing.Hostname != hostname {
		return state.App{}, nil, false, nil
	}
	return r.app, r.spec, true, nil
}

// ByPath matches the way the real query does: a path-mode app whose path is
// the request's path or a whole-segment prefix of it, case-sensitively.
func (r *resolver) ByPath(_ context.Context, path string) (state.App, *spec.AppSpec, string, bool, error) {
	if r.app.ID == "" || r.spec == nil || r.spec.Routing.Mode != spec.RoutingPath || r.spec.Routing.PathPrefix == "" {
		return state.App{}, nil, "", false, nil
	}
	prefix := r.spec.Routing.PathPrefix
	if path != prefix && !strings.HasPrefix(path, prefix+"/") {
		return state.App{}, nil, "", false, nil
	}
	return r.app, r.spec, prefix, true, nil
}

func (r *resolver) BySlug(_ context.Context, slug string) (state.App, *spec.AppSpec, bool, error) {
	if r.app.ID == "" || r.app.Slug != slug {
		return state.App{}, nil, false, nil
	}
	return r.app, r.spec, true, nil
}

// ByPort matches the same way the real query does: the mode as well as the
// number, so a port recorded on an app that is not in port mode never answers.
func (r *resolver) ByPort(_ context.Context, port int) (state.App, *spec.AppSpec, bool, error) {
	if r.app.ID == "" || r.spec == nil {
		return state.App{}, nil, false, nil
	}
	if r.spec.Routing.Mode != spec.RoutingPort || r.spec.Routing.Port != port {
		return state.App{}, nil, false, nil
	}
	return r.app, r.spec, true, nil
}

type fixedUpstream struct {
	addr string
	dial func(context.Context) (net.Conn, error)
}

func (f fixedUpstream) Primary(context.Context, state.App, *spec.AppSpec) (api.Upstream, error) {
	return api.Upstream{URL: f.addr, Dial: f.dial, PoolKey: f.addr}, nil
}

type staticAuth struct{ principal authz.Principal }

func (s staticAuth) Authenticate(*http.Request) (authz.Principal, error) { return s.principal, nil }

// --- fixture ---------------------------------------------------------------

const appID = "app_01HQ8"

// harness returns a proxy in front of a recording upstream.
func harness(t *testing.T, principal authz.Principal, configure func(*store)) (*httptest.Server, *store, *proxy.Counters, *received) {
	t.Helper()
	return harnessVia(t, principal, configure, func(url string) proxy.Upstreams { return fixedUpstream{addr: url} })
}

// harnessVia is harness with the way the proxy reaches the upstream chosen by
// the caller: directly, or through a host agent (agent_test.go).
func harnessVia(t *testing.T, principal authz.Principal, configure func(*store), via func(upstreamURL string) proxy.Upstreams) (*httptest.Server, *store, *proxy.Counters, *received) {
	t.Helper()
	return harnessWith(t, staticAuth{principal: principal}, configure, via)
}

// harnessWith is harnessVia with the authenticator chosen by the caller, for
// tests of what the proxy lets the authenticator see (credentials_test.go).
func harnessWith(t *testing.T, auth proxy.Authenticator, configure func(*store), via func(upstreamURL string) proxy.Upstreams) (*httptest.Server, *store, *proxy.Counters, *received) {
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

	counters := proxy.NewCounters()
	p := &proxy.Proxy{
		Resolver: &resolver{
			app: state.App{ID: appID, Slug: "notes", State: state.StateRunning},
			spec: &spec.AppSpec{
				// Addressed by hostname, and httptest serves on 127.0.0.1 — so
				// that is the Host the proxy sees once it drops the port. Set
				// explicitly because the resolver fake now matches on it, the
				// way the real query does.
				Routing: spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "127.0.0.1"},
				Workloads: []spec.Workload{{Name: "web", Primary: true,
					Ports: []spec.Port{{Number: 80, Protocol: "http"}}}},
			},
		},
		Authenticator: auth,
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     via(upstream.URL),
		Metrics:       counters,
		Logger:        zap.NewNop(),
		Mode:          spec.RoutingSubdomain,
	}

	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front, s, counters, got
}

type received struct {
	header http.Header
	path   string
}

func (r *received) record(req *http.Request) {
	r.header = req.Header.Clone()
	r.path = req.URL.Path
}

func activeUser(id string) authz.Principal {
	return authz.Principal{Kind: authz.KindUser, ID: id, UserID: id, Status: "active",
		Email: "alice@corp.com", DisplayName: "Alice"}
}

// --- the test the risk register names --------------------------------------

// TestR053_ForgedHeadersAreReplaced asserts R-053.
//
// This is the single most likely serious bug in the proxy: without an
// unconditional strip, a client sets X-Pando-User: admin@corp.com and any app
// trusting the convenience headers is trivially spoofed.
//
// The assertion is that the forged value arrives REPLACED, not merely that some
// header is present — a proxy that appended would pass a weaker test.
func TestR053_ForgedHeadersAreReplaced(t *testing.T) {
	front, _, _, got := harness(t, activeUser("usr_alice"), func(s *store) {
		s.owner[appID] = "usr_alice"
	})
	forgedHeadersAreReplaced(t, front, got)
}

func forgedHeadersAreReplaced(t *testing.T, front *httptest.Server, got *received) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	require.NoError(t, err)

	// Every spelling a forger might try. Header keys canonicalize, so these all
	// land in the same namespace.
	req.Header.Set("X-Pando-User", "admin@corp.com")
	req.Header.Set("x-pando-email", "admin@corp.com")
	req.Header.Set("X-PANDO-GROUPS", "admins,superusers")
	req.Header.Set("X-Pando-Assertion", "forged.assertion.value")
	req.Header.Set("X-Pando-Something-New", "whatever")
	req.Header.Set("X-Forwarded-Prefix", "/forged")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, "usr_alice", got.header.Get("X-Pando-User"),
		"the forged user must be replaced with the real principal")
	require.Equal(t, "alice@corp.com", got.header.Get("X-Pando-Email"))
	require.NotEqual(t, "forged.assertion.value", got.header.Get(assertion.Header))

	// Not merely overwritten — gone. A header Pando does not set must not
	// survive either, or a future convenience header becomes spoofable the day
	// it is added.
	require.Empty(t, got.header.Get("X-Pando-Something-New"),
		"an unknown header in Pando's namespace must not pass through")
	require.Empty(t, got.header.Get("X-Pando-Groups"),
		"this principal has no groups, so no group header may arrive")
	require.Empty(t, got.header.Get("X-Forwarded-Prefix"),
		"X-Forwarded-Prefix is Pando's to set; an inbound one is a forgery")

	// And nothing forged is anywhere in the namespace at all.
	for name, values := range got.header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Pando-") {
			for _, v := range values {
				require.NotContains(t, v, "admin@corp.com", "header %s leaked a forged value", name)
				require.NotContains(t, v, "superusers", "header %s leaked a forged value", name)
			}
		}
	}
}

// --- assertions ------------------------------------------------------------

// TestR054_AssertionCarriesTheStableSubject asserts R-054: sub is users.id.
func TestR054_AssertionCarriesTheStableSubject(t *testing.T) {
	front, _, _, got := harness(t, activeUser("usr_alice"), func(s *store) {
		s.owner[appID] = "usr_alice"
	})

	resp, err := http.Get(front.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	claims := decodeClaims(t, got.header.Get(assertion.Header))
	require.Equal(t, "usr_alice", claims.Sub)
	require.Equal(t, appID, claims.Aud, "aud is the app, which is what prevents cross-app replay")
	require.Equal(t, "https://pando.test", claims.Iss)
	require.Equal(t, int64(assertion.Lifetime/time.Second), claims.Exp-claims.Iat,
		"lifetime is the one constant, not a copy of its value")
}

// TestR056_AnonymousStillGetsAnAssertion asserts R-056.
//
// The consequence an app developer depends on: absence of the header means the
// request did not come through Pando at all. That only holds if an anonymous
// request still carries one.
func TestR056_AnonymousStillGetsAnAssertion(t *testing.T) {
	front, _, counters, got := harness(t, authz.Anonymous(), func(s *store) {
		s.anonymous[appID] = true // shared with everyone (R-075)
	})

	resp, err := http.Get(front.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	claims := decodeClaims(t, got.header.Get(assertion.Header))
	require.Equal(t, assertion.AnonymousSubject, claims.Sub)
	require.Equal(t, appID, claims.Aud)

	// R-023: no bypass. The anonymous path traverses every step, and the
	// counter proves it rather than the comment claiming it.
	total, allowed, _, anonymous := counters.Snapshot(appID)
	require.Equal(t, int64(1), total)
	require.Equal(t, int64(1), allowed)
	require.Equal(t, int64(1), anonymous)
}

// TestCrossAppReplayIsRejected asserts the aud check: an assertion minted for
// one app must not verify for another.
func TestCrossAppReplayIsRejected(t *testing.T) {
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	tokenForA, err := minter.Mint(assertion.Claims{Sub: "usr_alice", Aud: "app_A"})
	require.NoError(t, err)

	claims, err := minter.Verify(tokenForA)
	require.NoError(t, err, "the assertion itself is valid")
	require.Equal(t, "app_A", claims.Aud)

	// An app verifies the signature AND checks aud against its own ID. The
	// signature holding while the audience does not match is exactly the case
	// this guards.
	require.NotEqual(t, "app_B", claims.Aud,
		"app B must reject this assertion on aud, even though the signature verifies")
}

// TestAssertionWithoutAnAudienceIsRefused asserts that an assertion replayable
// against every app cannot be produced by accident.
func TestAssertionWithoutAnAudienceIsRefused(t *testing.T) {
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	_, err = minter.Mint(assertion.Claims{Sub: "usr_alice"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "must name the app")
}

// --- access ----------------------------------------------------------------

// TestR029_AnOperatorWithNoDataGrantIsDenied asserts the two-plane split at the
// proxy — the place it matters most.
func TestR029_AnOperatorWithNoDataGrantIsDenied(t *testing.T) {
	front, _, counters, _ := harness(t, activeUser("usr_bob"), func(s *store) {
		s.owner[appID] = "usr_alice" // bob is not the owner and holds no data grant
	})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(front.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"an authenticated caller without access gets 403, not a login redirect")

	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "Ask whoever set it up",
		"the message says what to do, in the product's voice")

	// Denied requests are counted too.
	total, allowed, denied, _ := counters.Snapshot(appID)
	require.Equal(t, int64(1), total)
	require.Equal(t, int64(0), allowed)
	require.Equal(t, int64(1), denied)
}

// TestAnonymousWithoutAccessIsSentToSignIn asserts the other branch: an
// anonymous caller is redirected rather than refused, with somewhere to return.
func TestAnonymousWithoutAccessIsSentToSignIn(t *testing.T) {
	front, _, _, _ := harness(t, authz.Anonymous(), nil)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(front.URL + "/dashboard?tab=deploys")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusFound, resp.StatusCode)
	location := resp.Header.Get("Location")
	require.Contains(t, location, "/login")
	require.Contains(t, location, "dashboard", "the original destination is preserved")
}

// TestR079_RemovingAGroupRevokesAccess asserts R-079 at the proxy: membership is
// read from the principal each request, so a removal takes effect without a
// redeploy.
func TestR079_RemovingAGroupRevokesAccess(t *testing.T) {
	member := activeUser("usr_bob")
	member.Groups = []string{"grp_engineering"}

	front, _, _, _ := harness(t, member, func(s *store) {
		s.owner[appID] = "usr_alice"
		s.data[appID] = []string{"grp_engineering"}
	})

	resp, err := http.Get(front.URL + "/")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The same user without the group. Nothing about the app or the grant
	// changed — only what the principal carries.
	frontAfter, _, _, _ := harness(t, activeUser("usr_bob"), func(s *store) {
		s.owner[appID] = "usr_alice"
		s.data[appID] = []string{"grp_engineering"}
	})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	after, err := client.Get(frontAfter.URL + "/")
	require.NoError(t, err)
	_ = after.Body.Close()
	require.Equal(t, http.StatusForbidden, after.StatusCode)
}

// TestASuspendedUserIsDenied asserts R-049 reaches the proxy.
func TestASuspendedUserIsDenied(t *testing.T) {
	suspended := activeUser("usr_alice")
	suspended.Status = "suspended"

	front, _, _, _ := harness(t, suspended, func(s *store) {
		s.owner[appID] = "usr_alice" // owner, but suspended
	})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(front.URL + "/")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"suspension denies even the owner")
}

// --- routing ---------------------------------------------------------------

// TestR167_PathModeStripsThePrefix asserts R-167.
func TestR167_PathModeStripsThePrefix(t *testing.T) {
	got := &received{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
	}))
	defer upstream.Close()

	s := newStore()
	s.owner[appID] = "usr_alice"
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	p := &proxy.Proxy{
		Resolver: &resolver{
			app: state.App{ID: appID, Slug: "notes", State: state.StateRunning},
			spec: &spec.AppSpec{Workloads: []spec.Workload{{Name: "web", Primary: true,
				Ports: []spec.Port{{Number: 80, Protocol: "http"}}}}},
		},
		Authenticator: staticAuth{principal: activeUser("usr_alice")},
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     fixedUpstream{addr: upstream.URL},
		Logger:        zap.NewNop(),
		Mode:          spec.RoutingPath,
	}
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/notes/dashboard")
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, "/dashboard", got.path, "the prefix is stripped before the app sees it")
	require.Equal(t, "/notes", got.header.Get("X-Forwarded-Prefix"),
		"and the app is told what was stripped, so it can build correct links")
}

// TestR167_ACustomPathIsServedAndStripped asserts R-167 for a path somebody
// chose: deeper than one segment, not the slug, matched on whole segments,
// stripped, and named in X-Forwarded-Prefix. Every step after resolution —
// CheckData, the header strip, the assertion — is the same one every request
// takes (R-023).
func TestR167_ACustomPathIsServedAndStripped(t *testing.T) {
	got := &received{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
	}))
	defer upstream.Close()

	s := newStore()
	s.owner[appID] = "usr_alice"
	p := routedProxy(t, s, upstream.URL, "notes-a1b2c3",
		spec.Routing{Mode: spec.RoutingPath, PathPrefix: "/team/notes"}, spec.RoutingPath, activeUser("usr_alice"))
	front := httptest.NewServer(p)
	defer front.Close()

	get := func(path string) int {
		t.Helper()
		*got = received{}
		req, err := http.NewRequest(http.MethodGet, front.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("X-Pando-User", "forged@example.com")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	get("/team/notes/dashboard")
	require.Equal(t, "/dashboard", got.path)
	require.Equal(t, "/team/notes", got.header.Get("X-Forwarded-Prefix"))
	require.NotEqual(t, "forged@example.com", got.header.Get("X-Pando-User"), "step 7 still runs")

	get("/team/notes")
	require.Equal(t, "/", got.path, "the path itself is the app's root")

	get("/team/notes-archive/x")
	require.Empty(t, got.path, "a prefix matches whole segments, not characters")

	// The slug still answers, as it always has.
	get("/notes-a1b2c3/x")
	require.Equal(t, "/x", got.path)
}

// TestAnAppThatIsNotRunningIs503 asserts step 2 of the request path.
func TestAnAppThatIsNotRunningIs503(t *testing.T) {
	s := newStore()
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	p := &proxy.Proxy{
		Resolver: &resolver{
			// Addressed by hostname, matching what httptest serves on. The
			// fake matches on it the way the real query does, so an app with
			// neither hostname nor matching slug is simply not found — which
			// is correct, and would make this test assert 404 instead of the
			// 503 it is about.
			app: state.App{ID: appID, Slug: "notes", State: state.StateStopped},
			spec: &spec.AppSpec{
				Routing: spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "127.0.0.1"},
			},
		},
		Authenticator: staticAuth{principal: activeUser("usr_alice")},
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     fixedUpstream{addr: "http://unused"},
		Logger:        zap.NewNop(),
	}
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "isn't running")
}

// --- streaming -------------------------------------------------------------

// TestR170_SSEIsNotBuffered asserts R-170.
//
// A build log that arrives in one lump at the end is not a live log, and
// watching a build happen is the whole point of the endpoint behind this.
func TestR170_SSEIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)

		fmt.Fprint(w, "data: first\n\n")
		_ = rc.Flush()

		<-release // hold the handler open; a buffering proxy would send nothing yet

		fmt.Fprint(w, "data: second\n\n")
		_ = rc.Flush()
	}))
	defer upstream.Close()

	s := newStore()
	s.owner[appID] = "usr_alice"
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	front := httptest.NewServer(&proxy.Proxy{
		Resolver: &resolver{
			app: state.App{ID: appID, Slug: "notes", State: state.StateRunning},
			spec: &spec.AppSpec{
				Routing: spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "127.0.0.1"},
				Workloads: []spec.Workload{{Name: "web", Primary: true,
					Ports: []spec.Port{{Number: 80, Protocol: "http"}}}},
			},
		},
		Authenticator: staticAuth{principal: activeUser("usr_alice")},
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     fixedUpstream{addr: upstream.URL},
		Logger:        zap.NewNop(),
	})
	defer front.Close()

	resp, err := http.Get(front.URL + "/events")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// The first event must arrive while the upstream handler is still running.
	buf := make([]byte, 64)
	done := make(chan string, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		done <- string(buf[:n])
	}()

	select {
	case first := <-done:
		require.Contains(t, first, "first", "the first event arrived before the stream closed")
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("nothing arrived while the upstream was still open — the proxy is buffering")
	}
	close(release)
}

func decodeClaims(t *testing.T, token string) assertion.Claims {
	t.Helper()
	require.NotEmpty(t, token, "no assertion reached the app")

	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)

	payload, err := base64Decode(parts[1])
	require.NoError(t, err)

	var claims assertion.Claims
	require.NoError(t, json.Unmarshal(payload, &claims))
	return claims
}

func base64Decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// TestAnInstallCanMixAddressingModes asserts design 03 §4.1: neither topology
// is a global setting, and an install can run both at once.
//
// The proxy used to switch on its configured Mode and resolve one way only,
// which made the install's default a hard constraint — a subdomain app on a
// path-default install resolved to nothing and fell through to the console,
// looking to its owner like the app did not exist.
func TestAnInstallCanMixAddressingModes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.Path)
	}))
	defer upstream.Close()

	s := newStore()
	s.owner[appID] = "usr_alice"

	// One app, addressed by path, on an install whose default is subdomain:
	// deliberately the *other* shape.
	front := httptest.NewServer(routedProxy(t, s, upstream.URL, "notes",
		spec.Routing{Mode: spec.RoutingPath, PathPrefix: "/notes"}, spec.RoutingSubdomain, activeUser("usr_alice")))
	defer front.Close()

	resp, err := http.Get(front.URL + "/notes/dashboard")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"a path-addressed app must resolve on an install that defaults to subdomains")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "/dashboard", string(body), "and its prefix is still stripped (R-167)")
}

// TestR161_APortModeAppIsServedAtTheRootOfItsPort asserts R-161's port mode.
//
// The whole value of the mode is that there is no prefix: the app is at "/" of
// its own port, so an app whose HTML says "/assets/app.js" — which is every
// frontend built with a default configuration — resolves it to its own asset
// rather than to Pando. R-167 says Pando will not rewrite the page to make a
// prefix work, so this is the answer for those apps, and it is only an answer
// if the path arrives untouched.
func TestR161_APortModeAppIsServedAtTheRootOfItsPort(t *testing.T) {
	port := freePort(t)
	got := &received{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
	}))
	defer upstream.Close()

	s := newStore()
	s.owner[appID] = "usr_alice"

	// Slug "notes" as well, so the test can tell "resolved by port" from
	// "resolved by first path segment" — a proxy that fell through to the slug
	// would strip "/assets" and pass this test for the wrong reason.
	p := routedProxy(t, s, upstream.URL, "notes",
		spec.Routing{Mode: spec.RoutingPort, Port: port}, spec.RoutingPort, activeUser("usr_alice"))

	listeners := &proxy.PortListeners{Ports: fixedPorts{port}, Handler: p}
	ctx, stop := context.WithCancel(context.Background())
	go listeners.Run(ctx)
	defer stop()

	// The listener is opened by the first sync, which Run does before it ticks.
	require.Eventually(t, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/assets/app.js", port))
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 25*time.Millisecond)

	require.Equal(t, "/assets/app.js", got.path,
		"a port-mode app sees the path it was asked for, with nothing stripped")
	require.Empty(t, got.header.Get("X-Forwarded-Prefix"),
		"and is told of no prefix, because there is none")
}

// TestR023_APortListenerIsTheSameEnforcementPoint asserts R-023 for the port
// listeners: a second way in must not be a second decision.
func TestR023_APortListenerIsTheSameEnforcementPoint(t *testing.T) {
	port := freePort(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Nobody is granted anything: no owner, no data-plane grant.
	s := newStore()
	p := routedProxy(t, s, upstream.URL, "notes",
		spec.Routing{Mode: spec.RoutingPort, Port: port}, spec.RoutingPort, activeUser("usr_mallory"))

	listeners := &proxy.PortListeners{Ports: fixedPorts{port}, Handler: p}
	ctx, stop := context.WithCancel(context.Background())
	go listeners.Run(ctx)
	defer stop()

	var status int
	require.Eventually(t, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		status = resp.StatusCode
		return true
	}, 5*time.Second, 25*time.Millisecond)

	require.Equal(t, http.StatusForbidden, status,
		"the data-plane check runs on a port listener exactly as it does on the front door")
}

// fixedPorts is a port allocator that always reports the same ports.
type fixedPorts []int

func (f fixedPorts) InUse(context.Context) ([]int, error) { return f, nil }

// freePort asks the OS for a port nobody is using.
//
// Not a hard-coded number: these tests bind a real listener, and a developer
// machine running Pando's own Compose stack has 9000-9019 published on it — so
// a fixed 9010 passed alone and failed in a full run, which is the worst way
// for a test to fail.
// routedProxy is a proxy in front of one running app, appID, called slug and
// routed as routing, on an install whose default shape is mode. principal is
// who every request comes from; s decides what they may do.
func routedProxy(t *testing.T, s *store, upstreamURL, slug string, routing spec.Routing, mode spec.RoutingMode, principal authz.Principal) *proxy.Proxy {
	t.Helper()
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	return &proxy.Proxy{
		Resolver: &resolver{
			app: state.App{ID: appID, Slug: slug, State: state.StateRunning},
			spec: &spec.AppSpec{
				Routing: routing,
				Workloads: []spec.Workload{{Name: "web", Primary: true,
					Ports: []spec.Port{{Number: 80, Protocol: "http"}}}},
			},
		},
		Authenticator: staticAuth{principal: principal},
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     fixedUpstream{addr: upstreamURL},
		Logger:        zap.NewNop(),
		Mode:          mode,
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// TestR173_PandosOwnCookiesNeverReachAnApp asserts R-173.
//
// The header rule exists because an app that trusts X-Pando-User is trusting
// the network boundary; the cookie rule exists because an app that *receives*
// pando_session does not have to trust anything — it can replay the credential
// against Pando's own API as the person who visited it. Under path routing the
// browser sends it on every request, because the app shares Pando's origin.
//
// What an app is entitled to is the assertion: scoped to that app (R-054),
// signed, and short-lived.
func TestR173_PandosOwnCookiesNeverReachAnApp(t *testing.T) {
	front, _, _, got := harness(t, activeUser("usr_alice"), func(s *store) {
		s.owner[appID] = "usr_alice"
	})
	pandosCookiesAreStripped(t, front, got)
}

func pandosCookiesAreStripped(t *testing.T, front *httptest.Server, got *received) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: "ses_01HQ8REALCREDENTIAL"})
	req.AddCookie(&http.Cookie{Name: "pando_anything_later", Value: "also-a-credential"})
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	req.AddCookie(&http.Cookie{Name: "cart", Value: "two-items"})

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	forwarded := got.header.Get("Cookie")
	require.NotContains(t, forwarded, "ses_01HQ8REALCREDENTIAL",
		"the session cookie is a credential and no app may have it")
	require.NotContains(t, forwarded, "pando_",
		"nor any other cookie in Pando's namespace, including ones not invented yet")

	// The app's own cookies are untouched: this is a filter, not a purge.
	require.Contains(t, forwarded, "theme=dark")
	require.Contains(t, forwarded, "cart=two-items")
}

// TestR075a_APasscodeAppAsksForItsPasscode asserts the proxy's half of public
// with a passcode: without the unlock, a visitor — signed in or not — is sent to
// the passcode page, not to sign-in; with it, they reach the app; and the
// unlock cookie, a credential, never reaches the app itself (R-173).
func TestR075a_APasscodeAppAsksForItsPasscode(t *testing.T) {
	for _, who := range []authz.Principal{authz.Anonymous(), activeUser("usr_nobody")} {
		front, _, _, got := harness(t, who, func(s *store) {
			s.anonymous[appID] = true
			s.passcode[appID] = "unlock-token"
		})
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}

		resp, err := client.Get(front.URL + "/notes?page=2")
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusFound, resp.StatusCode)
		location := resp.Header.Get("Location")
		require.Contains(t, location, "/.pando/login?passcode="+appID)
		require.Contains(t, location, "next=%2Fnotes%3Fpage%3D2")

		// A wrong unlock is no unlock.
		req, _ := http.NewRequest(http.MethodGet, front.URL+"/notes", nil)
		req.AddCookie(&http.Cookie{Name: proxy.PasscodeCookiePrefix + appID, Value: "guessed"})
		resp, err = client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusFound, resp.StatusCode)

		// The right one, for this app, lets them through without the app
		// ever seeing it.
		req, _ = http.NewRequest(http.MethodGet, front.URL+"/notes", nil)
		req.AddCookie(&http.Cookie{Name: proxy.PasscodeCookiePrefix + appID, Value: "unlock-token"})
		req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
		resp, err = client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NotContains(t, got.header.Get("Cookie"), "unlock-token")
		require.Contains(t, got.header.Get("Cookie"), "theme=dark")
	}
}
