package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakePando answers the requests the harness makes, the way Pando behind the
// balancer would: health, setup, sign-in, the console's and API clients'
// reads, and the proxy's answers for seeded and real apps. Anything that has
// to change an install (creating, deploying, deleting apps) is a hook the
// integration test fills in against a real database.
type fakePando struct {
	*httptest.Server

	AdminUser, AdminPassword, UserPassword, TokenSecret string

	// HealthFailures is how many /healthz requests answer 503 first.
	HealthFailures atomic.Int32
	// NeedsSetup is what GET /setup answers; POST /setup clears it.
	NeedsSetup atomic.Bool
	// SetupHook, if set, runs on POST /setup.
	SetupHook func(username, password string) error
	// UserID maps a username to the user ID the proxy judges grants by.
	// Unset, the username is the ID.
	UserID func(username string) string

	// Real apps the proxy serves, by slug. Port is the app served at "/"
	// on this server's own port.
	mu     sync.Mutex
	Real   map[string]fakeReal
	ByPort string

	// API hooks for routes that change the install. A route without a hook
	// answers 404.
	Mutate func(w http.ResponseWriter, r *http.Request, path string) bool

	// Seen counts requests by "METHOD path".
	seenMu sync.Mutex
	Seen   map[string]int
}

type fakeReal struct {
	Anonymous bool
	Members   map[string]bool // user IDs
}

func newFakePando(t *testing.T) *fakePando {
	t.Helper()
	f := &fakePando{
		AdminUser: "admin", AdminPassword: "admin-password",
		UserPassword: "user-password", TokenSecret: "token-secret",
		Real: map[string]fakeReal{}, Seen: map[string]int{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakePando) seen(key string) int {
	f.seenMu.Lock()
	defer f.seenMu.Unlock()
	return f.Seen[key]
}

func (f *fakePando) userOf(r *http.Request) (string, bool) {
	ck, err := r.Cookie("pando_session")
	if err != nil {
		return "", false
	}
	name, ok := strings.CutPrefix(ck.Value, "session-")
	if !ok {
		return "", false
	}
	if f.UserID != nil {
		return f.UserID(name), true
	}
	return name, true
}

func (f *fakePando) serve(w http.ResponseWriter, r *http.Request) {
	f.seenMu.Lock()
	f.Seen[r.Method+" "+r.URL.Path]++
	f.seenMu.Unlock()

	if r.URL.Path == "/healthz" {
		if f.HealthFailures.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if path, ok := strings.CutPrefix(r.URL.Path, "/api/v1"); ok {
		f.api(w, r, path)
		return
	}
	f.proxy(w, r)
}

func (f *fakePando) api(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case path == "/setup" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]bool{"needed": f.NeedsSetup.Load()})
		return
	case path == "/setup" && r.Method == http.MethodPost:
		var body struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !f.NeedsSetup.Load() {
			http.Error(w, `{"code":"VALID_INVALID"}`, http.StatusConflict)
			return
		}
		if f.SetupHook != nil {
			if err := f.SetupHook(body.Username, body.Password); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		f.NeedsSetup.Store(false)
		w.WriteHeader(http.StatusCreated)
		return
	case path == "/sessions" && r.Method == http.MethodPost:
		var body struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		ok := (body.Username == f.AdminUser && body.Password == f.AdminPassword) ||
			(body.Username != f.AdminUser && body.Password == f.UserPassword && !strings.HasPrefix(body.Username, "nobody"))
		if !ok {
			http.Error(w, `{"code":"AUTH_INVALID"}`, http.StatusUnauthorized)
			return
		}
		if body.Username == "no-cookie" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "pando_csrf", Value: "x"})
		http.SetCookie(w, &http.Cookie{Name: "pando_session", Value: "session-" + body.Username})
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet || strings.Contains(path, "/deployments/") {
		if f.Mutate != nil && f.Mutate(w, r, path) {
			return
		}
		http.NotFound(w, r)
		return
	}
	_, signedIn := f.userOf(r)
	bearer := strings.HasSuffix(r.Header.Get("Authorization"), "."+f.TokenSecret)
	if !signedIn && !bearer {
		http.Error(w, `{"code":"AUTH_REQUIRED"}`, http.StatusUnauthorized)
		return
	}
	_, _ = w.Write([]byte(`{"items":[]}`))
}

// proxy answers as Pando's proxy does: a seeded app (no container) is 503,
// a real app lets in its members and, if public, anyone; a visitor who is
// not signed in to a private app is sent to sign in.
func (f *fakePando) proxy(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.Host)
	if host == "" {
		host = r.Host
	}
	if strings.HasPrefix(host, appPrefix) || strings.HasPrefix(r.URL.Path, "/load/") {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	f.mu.Lock()
	slug := strings.Trim(r.URL.Path, "/")
	if slug == "" {
		slug = f.ByPort
	}
	app, ok := f.Real[slug]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	user, signedIn := f.userOf(r)
	switch {
	case app.Anonymous || (signedIn && app.Members[user]):
		_, _ = w.Write([]byte("hello"))
	case !signedIn:
		http.Redirect(w, r, "/sign-in", http.StatusFound)
	default:
		w.WriteHeader(http.StatusForbidden)
	}
}

func TestClientDoSendsCredentialsHostAndBody(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		http.SetCookie(w, &http.Cookie{Name: "a", Value: "b"})
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/api/v1/", time.Second)
	require.Equal(t, srv.URL, c.Root, "the root drops a trailing slash and /api/v1")
	require.Equal(t, srv.URL+"/api/v1/apps", c.API("/apps"))

	resp, err := c.Do(context.Background(), http.MethodPost, c.API("/x"), "app.localtest.me",
		Credential{Cookie: "sess", Bearer: "tok_1.s"}, `{"a":1}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusTeapot, resp.Status)
	require.Equal(t, "short and stout", string(resp.Body))
	require.Positive(t, resp.Latency)
	require.Len(t, resp.Cookies, 1)
	require.Equal(t, "app.localtest.me", got.Host)
	require.Equal(t, "application/json", got.Header.Get("Content-Type"))
	require.Equal(t, "Bearer tok_1.s", got.Header.Get("Authorization"))
	ck, err := got.Cookie("pando_session")
	require.NoError(t, err)
	require.Equal(t, "sess", ck.Value)
	require.Equal(t, `{"a":1}`, body)

	// No body, no credential: neither header is set.
	_, err = c.Do(context.Background(), http.MethodGet, c.API("/x"), "", Credential{}, "")
	require.NoError(t, err)
	require.Empty(t, got.Header.Get("Content-Type"))
	require.Empty(t, got.Header.Get("Authorization"))
	require.Empty(t, got.Cookies())

	_, err = c.Do(context.Background(), "BAD METHOD", c.API("/x"), "", Credential{}, "")
	require.Error(t, err, "a request that cannot be built is an error, not a send")
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/sign-in", http.StatusFound)
	}))
	defer srv.Close()
	resp, err := NewClient(srv.URL, time.Second).Do(context.Background(), http.MethodGet, srv.URL+"/app/", "", Credential{}, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusFound, resp.Status, "a redirect to sign in is the proxy's answer")
}

func TestClientJSONDecodesSuccessAndReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ok":
			_, _ = w.Write([]byte(`{"id":"app_1"}`))
		case "/api/v1/garbled":
			_, _ = w.Write([]byte(`{"id":`))
		case "/api/v1/empty":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "nope", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, time.Second)
	ctx := context.Background()

	var out struct{ ID string }
	status, err := c.JSON(ctx, http.MethodGet, "/ok", Credential{}, "", &out)
	require.NoError(t, err)
	require.Equal(t, 200, status)
	require.Equal(t, "app_1", out.ID)

	status, err = c.JSON(ctx, http.MethodDelete, "/empty", Credential{}, "", &out)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, status)

	status, err = c.JSON(ctx, http.MethodGet, "/garbled", Credential{}, "", &out)
	require.ErrorContains(t, err, "GET /garbled: decoding")
	require.Equal(t, 200, status)

	status, err = c.JSON(ctx, http.MethodPost, "/missing", Credential{}, "{}", nil)
	require.ErrorContains(t, err, "POST /missing: 400 nope")
	require.Equal(t, 400, status)

	srv.Close()
	status, err = c.JSON(ctx, http.MethodGet, "/ok", Credential{}, "", nil)
	require.Error(t, err)
	require.Zero(t, status, "a transport error has no status")
}

func TestAwaitHealthyWaitsThenGivesUp(t *testing.T) {
	f := newFakePando(t)
	f.HealthFailures.Store(1)
	c := NewClient(f.URL, time.Second)
	require.NoError(t, c.AwaitHealthy(context.Background(), 10*time.Second), "one 503, then healthy")
	require.Equal(t, 2, f.seen("GET /healthz"))

	f.HealthFailures.Store(100)
	err := c.AwaitHealthy(context.Background(), 0)
	require.ErrorContains(t, err, "/healthz answered 503")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, c.AwaitHealthy(ctx, time.Hour), context.Canceled)

	dead := NewClient(closedURL(t), time.Second)
	require.ErrorContains(t, dead.AwaitHealthy(context.Background(), 0), "did not become healthy")
}

// closedURL is the address of a server that has stopped listening.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

func TestClaimSetupClaimsOnlyAFreshInstall(t *testing.T) {
	f := newFakePando(t)
	var claimed []string
	f.SetupHook = func(u, p string) error { claimed = append(claimed, u+":"+p); return nil }
	c := NewClient(f.URL, time.Second)

	f.NeedsSetup.Store(true)
	require.NoError(t, c.ClaimSetup(context.Background(), "admin", "pw"))
	require.Equal(t, []string{"admin:pw"}, claimed)

	require.NoError(t, c.ClaimSetup(context.Background(), "admin", "other"), "an install already claimed is left alone")
	require.Len(t, claimed, 1)
	require.Equal(t, 1, f.seen("POST /api/v1/setup"))

	f.NeedsSetup.Store(true)
	f.SetupHook = func(string, string) error { return errors.New("database down") }
	require.ErrorContains(t, c.ClaimSetup(context.Background(), "admin", "pw"), "500")

	require.Error(t, NewClient(closedURL(t), time.Second).ClaimSetup(context.Background(), "admin", "pw"))
}

func TestSignInReturnsTheSessionCookie(t *testing.T) {
	f := newFakePando(t)
	c := NewClient(f.URL, time.Second)
	ctx := context.Background()

	cookie, resp, err := c.SignIn(ctx, "admin", "admin-password")
	require.NoError(t, err)
	require.Equal(t, "session-admin", cookie, "the session cookie, not the first one set")
	require.Equal(t, 200, resp.Status)

	_, resp, err = c.SignIn(ctx, "admin", "wrong")
	require.ErrorContains(t, err, "signing in as admin: 401")
	require.Equal(t, 401, resp.Status)

	_, _, err = c.SignIn(ctx, "no-cookie", "user-password")
	require.ErrorContains(t, err, "set no session cookie")

	_, _, err = NewClient(closedURL(t), time.Second).SignIn(ctx, "admin", "admin-password")
	require.Error(t, err)
}

func TestHostOfAndTimeouts(t *testing.T) {
	require.Equal(t, "127.0.0.1", NewClient("http://127.0.0.1:28080", time.Second).hostOf())
	require.Equal(t, "localhost", (&Client{Root: "http://[::1"}).hostOf(), "an unparseable root falls back")

	require.True(t, isTimeout(context.DeadlineExceeded))
	require.True(t, isTimeout(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)))
	require.True(t, isTimeout(&net.OpError{Op: "dial", Err: timeoutErr{}}))
	require.False(t, isTimeout(errors.New("connection reset")))

	require.Equal(t, "timeout", statusKey(Outcome{Err: context.DeadlineExceeded}))
	require.Equal(t, "transport error", statusKey(Outcome{}))
	require.Equal(t, "404", statusKey(Outcome{Status: 404}))
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestPacedSendsAtTheRateAndCountsWhatItCannotSend(t *testing.T) {
	// Rate zero sends nothing and returns when the step ends.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		paced(ctx, 0, 1, func(context.Context) { t.Error("sent at rate zero") }, func() {})
		close(done)
	}()
	cancel()
	<-done

	// One worker that never finishes: the first request occupies it, the
	// buffer holds one more, and every request after that is dropped.
	ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var sent, dropped atomic.Int64
	paced(ctx, 100, 1, func(ctx context.Context) { sent.Add(1); <-ctx.Done() }, func() { dropped.Add(1) })
	// The worker may take the buffered request as the step ends, and then
	// returns at once; it never takes a third.
	require.GreaterOrEqual(t, sent.Load(), int64(1))
	require.LessOrEqual(t, sent.Load(), int64(2), "the one worker was busy for the whole step")
	require.Greater(t, dropped.Load(), int64(5), "the rest were dropped and counted, not silently skipped")

	// Workers that keep up send everything.
	ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var kept, lost atomic.Int64
	paced(ctx, 100, 8, func(context.Context) { kept.Add(1) }, func() { lost.Add(1) })
	require.Zero(t, lost.Load())
	require.GreaterOrEqual(t, kept.Load(), int64(30), "about half a second at 100/s")
	require.LessOrEqual(t, kept.Load(), int64(51))
}

func TestJitterStaysWithinItsFraction(t *testing.T) {
	for range 1000 {
		d := jitter(10*time.Second, 0.2)
		require.GreaterOrEqual(t, d, 8*time.Second)
		require.LessOrEqual(t, d, 12*time.Second)
	}
	require.Equal(t, time.Second, jitter(time.Second, 0))
}

func TestEveryPollsUntilTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var n atomic.Int32
	every(ctx, 20*time.Millisecond, func(context.Context) { n.Add(1) })
	require.GreaterOrEqual(t, n.Load(), int32(5))
	require.LessOrEqual(t, n.Load(), int32(14))

	// Ended before the first poll was due: nothing is called.
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	n.Store(0)
	every(ctx, time.Hour, func(context.Context) { n.Add(1) })
	require.Zero(t, n.Load())
}

func TestStatsEdges(t *testing.T) {
	require.Equal(t, 0, bucketOf(0))
	require.Equal(t, 0, bucketOf(histMin))
	require.Equal(t, histSize-1, bucketOf(24*time.Hour))

	empty := &ClassStats{}
	require.Zero(t, empty.ErrorRate())
	require.Zero(t, empty.Rate())
}

func TestBreachesJudgeSignInsOnFailuresAlone(t *testing.T) {
	signIns := class("sign-in", "POST /sessions", 100, 10, 5*time.Second)
	got := Breaches(nil, StepResult{Classes: []*ClassStats{signIns}}, DefaultThresholds)
	require.Equal(t, []Breach{{Class: "POST /sessions", Reason: "10.0% of sign-ins failed"}}, got)
}

func TestSummarizeIsOneLine(t *testing.T) {
	s := StepResult{
		Online: 30,
		Classes: []*ClassStats{
			class("console", "GET /me", 100, 2, 20*time.Millisecond),
			class("proxy", "seeded app by path", 50, 0, 300*time.Millisecond),
		},
		PG: PGSample{PeakTotal: 12, MaxConnections: 100},
	}
	line := summarize(s)
	require.Equal(t, "held — 150 requests, 2 errors, slowest p95 300 ms (seeded app by path), 30 online, Postgres peak 12/100 connections", line)
	require.NotContains(t, line, "\n")

	s.Breaches = []Breach{{Class: "GET /me", Reason: "x"}}
	require.True(t, strings.HasPrefix(summarize(s), "BROKE (1 breaches) — "))
}

func TestReportCoversEveryKindOfStep(t *testing.T) {
	steps := Ramp(Tiers["vm"], []float64{1}, time.Minute)
	broke := StepResult{
		Step: steps[0], Online: 12, SignInFailures: 3,
		Classes: []*ClassStats{
			class("sign-in", "POST /sessions", 30, 0, 2*time.Second),
			class("console", "GET /me", 100, 50, 10*time.Millisecond),
		},
		Dropped:    map[string]uint64{"api": 0},
		TopQueries: []Query{{Query: "SELECT `x` FROM " + strings.Repeat("t", 300), Seen: 7}},
	}
	broke.Breaches = []Breach{{Class: "GET /me", Reason: "50.0% errors"}}
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var b strings.Builder
	require.NoError(t, WriteReport(&b, []Results{
		{Tier: Tiers["vm"], StartedAt: start, FinishedAt: start.Add(90 * time.Second), Thresholds: DefaultThresholds,
			Steps: []StepResult{broke}},
		{Tier: Tiers["cluster"], Thresholds: DefaultThresholds},
	}))
	out := b.String()
	require.Contains(t, out, "| Commit | — |")
	require.Contains(t, out, "| Duration | 1m30s |")
	require.Contains(t, out, "**Broke at step 1** (300 console users, 50 API req/s, 200 proxy req/s); no step held.")
	require.Contains(t, out, "3 sign-ins have failed so far in this run")
	require.NotContains(t, out, "dropped:", "a class that dropped nothing is not reported as dropping")
	require.Contains(t, out, "| sign-in | POST /sessions | 30 | — |", "sign-ins have no hold rate")
	require.Contains(t, out, "Queries most often caught running")
	require.Contains(t, out, "| 7 | `SELECT 'x' FROM ttt", "backticks cannot end the cell's code span")
	require.Contains(t, out, "…` |", "a long query is cut short")
	require.Contains(t, out, "## Tier: cluster")
	require.Contains(t, out, "No step completed.")

	require.Equal(t, "—", pct(0, 0))
	require.Equal(t, 0.0, surfaceRate(StepResult{}, "api"))
	w, c := slowest(StepResult{Classes: []*ClassStats{signIn()}})
	require.Zero(t, w)
	require.Equal(t, "—", c, "sign-ins are not the slowest class")
}

func signIn() *ClassStats { return class("sign-in", "POST /sessions", 1, 0, time.Minute) }

func TestWriteReportReportsAWriteError(t *testing.T) {
	require.Error(t, WriteReport(failingWriter{}, nil))
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestPGHelpers(t *testing.T) {
	got := topSeen(map[string]int{"b": 2, "a": 2, "c": 5, "d": 1}, 3)
	require.Equal(t, []Query{{Query: "c", Seen: 5}, {Query: "a", Seen: 2}, {Query: "b", Seen: 2}}, got,
		"most seen first, ties by text, cut to n")
	qs := []Query{{Query: "x", TotalMS: 1}, {Query: "y", TotalMS: 9}}
	sortQueries(qs)
	require.Equal(t, "y", qs[0].Query)
	require.Equal(t, "SELECT 1 FROM t", normalizeQuery("SELECT 1\n\t FROM   t "))
}

func TestPlanHelpers(t *testing.T) {
	require.Zero(t, pick("x", 1, 0), "nothing to pick from is zero")
	require.Equal(t, "load-g00007", groupName(7))
	require.Equal(t, "load-real-003", realName(3))
	require.Equal(t, "load-token-00012", tokenName(12))

	var s map[string]any
	require.NoError(t, json.Unmarshal([]byte(realAppSpec()), &s), "a real app's spec is a JSON body")
	require.Equal(t, "path", s["routing"].(map[string]any)["mode"],
		"in path mode; seeding moves it to port mode through the routing endpoint")
}

func TestTierValidateNamesEachProblem(t *testing.T) {
	base := Tier{Users: 10, Apps: 5, Groups: 2, Admins: 1, Tokens: 1}
	require.NoError(t, base.Validate())
	for _, c := range []struct {
		mut  func(*Tier)
		want string
	}{
		{func(t *Tier) { t.Users = 0 }, "at least one user"},
		{func(t *Tier) { t.Apps = 0 }, "at least one app"},
		{func(t *Tier) { t.Groups = 0 }, "at least one group"},
		{func(t *Tier) { t.Admins = 11 }, "admins (11)"},
		{func(t *Tier) { t.Tokens = -1 }, "tokens (-1)"},
		{func(t *Tier) { t.RealApps = -1 }, "real apps cannot be negative"},
	} {
		tier := base
		c.mut(&tier)
		require.ErrorContains(t, tier.Validate(), c.want)
	}
}

func TestCommonFlagsOverlayTheTier(t *testing.T) {
	c := common{tier: "vm", db: "postgres://x", users: 7, apps: 3, groups: 2, admins: 1, tokens: 2,
		realApps: 0, consoleUsers: 4, apiRate: 1.5, proxyRate: 2.5}
	got, err := c.resolve()
	require.NoError(t, err)
	require.Equal(t, Tier{Name: "vm", Users: 7, Apps: 3, Groups: 2, Admins: 1, Tokens: 2, RealApps: 0,
		ConsoleUsers: 4, APIRate: 1.5, ProxyRate: 2.5}, got)

	c = common{tier: "vm", db: "postgres://x", realApps: -1}
	got, err = c.resolve()
	require.NoError(t, err)
	require.Equal(t, Tiers["vm"], got, "zero and -1 keep the tier's own numbers")

	_, err = (&common{tier: "vm", realApps: -1}).resolve()
	require.ErrorContains(t, err, "-db is required")
	_, err = (&common{tier: "huge", db: "x"}).resolve()
	require.ErrorContains(t, err, `"huge" is not a tier`)
	_, err = (&common{tier: "vm", db: "x", tokens: 5000, realApps: -1}).resolve()
	require.ErrorContains(t, err, "tokens (5000)", "flags laid over a tier are validated")
}

func TestSubcommandsRefuseIncompleteFlags(t *testing.T) {
	t.Setenv("LOAD_DATABASE_URL", "")
	t.Setenv("LOAD_ADMIN_PASSWORD", "")
	ctx := context.Background()
	require.ErrorContains(t, seedCmd(ctx, nil), "-db is required")
	require.ErrorContains(t, seedCmd(ctx, []string{"-db", "postgres://x"}), "-admin-password is required")
	require.ErrorContains(t, runCmd(ctx, nil), "-db is required")
	require.ErrorContains(t, runCmd(ctx, []string{"-db", "postgres://x", "-steps", "0.5,lots"}), `"lots" is not a share`)
	require.ErrorContains(t, cleanupCmd(ctx, nil), "-db is required")
}

func TestReportCommandRendersResultsFiles(t *testing.T) {
	dir := t.TempDir()
	steps := Ramp(Tiers["vm"], []float64{1}, time.Minute)
	res := Results{Tier: Tiers["vm"], Commit: "abc1234", Thresholds: DefaultThresholds,
		Steps: []StepResult{{Step: steps[0], Classes: []*ClassStats{class("api", "GET /apps", 100, 0, time.Millisecond)}}}}
	in := filepath.Join(dir, "results.json")
	require.NoError(t, writeResults(in, &res))
	_, err := os.Stat(in + ".tmp")
	require.True(t, os.IsNotExist(err), "the results are written through a temporary file and renamed")
	require.NoError(t, writeResults("", &res), "no path writes nothing")

	out := filepath.Join(dir, "report.md")
	require.NoError(t, reportCmd([]string{"-in", in + ", ," + in, "-out", out}))
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(b), "## Tier: vm"), "one section per results file")
	require.Contains(t, string(b), "| Commit | abc1234 |")

	require.Error(t, reportCmd([]string{"-in", filepath.Join(dir, "missing.json")}))
	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte("{"), 0o600))
	require.ErrorContains(t, reportCmd([]string{"-in", bad}), bad)
	require.Error(t, reportCmd([]string{"-in", in, "-out", filepath.Join(dir, "no", "such", "dir.md")}))

	// Through main, as `go run ./test/load report` runs it.
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	out2 := filepath.Join(dir, "main.md")
	os.Args = []string{"load", "report", "-in", in, "-out", out2}
	main()
	b2, err := os.ReadFile(out2)
	require.NoError(t, err)
	require.Equal(t, string(b)[:200], string(b2)[:200])
	os.Args = []string{"load", "help"}
	main()
}

// runnerFor is a runner against f with a working set the test sets.
func runnerFor(f *fakePando, ws *workingSet) *runner {
	r := &runner{
		o:  RunOptions{UserPassword: f.UserPassword, TokenSecret: f.TokenSecret, Navigate: time.Hour},
		c:  NewClient(f.URL, 5*time.Second),
		ws: ws, rec: NewRecorder(), sessions: map[string]string{},
	}
	r.rec.Hold()
	return r
}

func classesOf(stats []*ClassStats) map[string]*ClassStats {
	out := map[string]*ClassStats{}
	for _, c := range stats {
		out[c.Class] = c
	}
	return out
}

func TestConsoleUsersLoadTheirPersonasScreens(t *testing.T) {
	f := newFakePando(t)
	ws := &workingSet{
		Users: []wsUser{
			{ID: "launcher", Name: "launcher"},
			{ID: "owner", Name: "owner", Owns: []string{"app_1"}},
			{ID: "admin-user", Name: "admin-user", Admin: true},
			{ID: "nobody", Name: "nobody"},
		},
		Seeded: []wsApp{{ID: "app_2"}},
	}
	r := runnerFor(f, ws)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var ready sync.WaitGroup
	var wg sync.WaitGroup
	for _, u := range ws.Users {
		ready.Add(1)
		wg.Add(1)
		go func() { defer wg.Done(); r.consoleUser(ctx, u, ready.Done) }()
	}
	ready.Wait()
	wg.Wait()

	require.Equal(t, 3, r.onlineCount(), "three signed in")
	require.Equal(t, int64(1), r.signInFailures.Load(), "and one could not")
	got := classesOf(r.rec.Take(time.Second))
	signIns := got["POST /sessions"]
	require.Equal(t, uint64(4), signIns.Requests)
	require.Equal(t, uint64(1), signIns.Errors)
	for class, want := range map[string]uint64{
		"GET /me":                    3,
		"GET /me/apps":               1, // the launcher
		"GET /users":                 1, // the administrator
		"GET /apps/{id}":             2, // the owner's app, and the one the administrator opened
		"GET /apps/{id}/deployments": 2,
		"GET /apps/{id}/specs":       2,
		"GET /approvals":             2,
	} {
		require.NotNil(t, got[class], class)
		require.GreaterOrEqual(t, got[class].Requests, want, class)
		require.Zero(t, got[class].Errors, "%s answered as a signed-in console expects", class)
	}
	require.Equal(t, 1, f.seen("GET /api/v1/apps/app_1"))
	require.Equal(t, 1, f.seen("GET /api/v1/apps/app_2"))
}

func TestAPIRequestsUseTheTokensOwnApps(t *testing.T) {
	f := newFakePando(t)
	ws := &workingSet{Tokens: []wsToken{
		{Bearer: "tok_1." + f.TokenSecret, Owns: []string{"app_1"}},
		{Bearer: "tok_2." + f.TokenSecret},
	}}
	r := runnerFor(f, ws)
	for range 300 {
		r.apiRequest(context.Background())
	}
	got := classesOf(r.rec.Take(time.Second))
	for _, class := range []string{"GET /apps", "GET /apps/{id}", "GET /apps/{id}/deployments", "GET /me/apps"} {
		require.NotNil(t, got[class], class)
		require.Zero(t, got[class].Errors, class)
		require.Equal(t, "api", got[class].Surface)
	}
	require.Zero(t, f.seen("GET /api/v1/apps/"), "a token that owns nothing never asks for an app by an empty ID")

	empty := runnerFor(f, &workingSet{})
	empty.apiRequest(context.Background())
	require.Empty(t, empty.rec.Take(time.Second), "no tokens, no API traffic")
}

func TestProxyRequestsExpectWhatPandoShouldAnswer(t *testing.T) {
	f := newFakePando(t)
	port := f.Listener.Addr().(*net.TCPAddr).Port
	f.Real["load-real-000"] = fakeReal{Members: map[string]bool{"member": true}}
	f.Real["load-real-001"] = fakeReal{Anonymous: true}
	f.ByPort = "load-real-000"
	ws := &workingSet{
		Seeded: []wsApp{
			{ID: "app_a", Slug: "load-a00000", Hostname: "load-a00000.localtest.me"},
			{ID: "app_b", Slug: "load-a00001", Path: "/load/load-a00001"},
		},
		Real: []wsReal{
			{ID: "app_r0", Slug: "load-real-000", Port: port, Members: map[string]bool{"member": true}},
			{ID: "app_r1", Slug: "load-real-001", Anonymous: true, Members: map[string]bool{}},
		},
	}
	r := runnerFor(f, ws)

	// Nobody online yet: every real-app request is anonymous.
	for range 50 {
		r.proxyRequest(context.Background())
	}
	r.sessions = map[string]string{"member": "session-member", "stranger": "session-stranger"}
	r.online = []string{"member", "stranger"}
	for range 600 {
		r.proxyRequest(context.Background())
	}
	got := classesOf(r.rec.Take(time.Second))
	for _, class := range []string{
		"seeded app by hostname", "seeded app by path",
		"real app by slug, anonymous", "real app by port, anonymous",
		"real app by slug, signed in, granted", "real app by port, signed in, granted",
		"real app by slug, signed in, not granted", "real app by port, signed in, not granted",
		"real app by slug, signed in, not granted, public app",
	} {
		require.NotNil(t, got[class], class)
		require.Zero(t, got[class].Errors, "%s: the harness expects the answer Pando gives (%v)", class, got[class].Statuses)
	}
	require.Equal(t, map[string]uint64{"503": got["seeded app by path"].Requests}, got["seeded app by path"].Statuses)
	require.Equal(t, map[string]uint64{"403": got["real app by slug, signed in, not granted"].Requests},
		got["real app by slug, signed in, not granted"].Statuses)
}

func TestHitRecordsNothingOnceTheStepHasEnded(t *testing.T) {
	f := newFakePando(t)
	r := runnerFor(f, &workingSet{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.hit(ctx, "api", "GET /apps", http.MethodGet, r.c.API("/apps"), "", Credential{}, is2xx)
	require.Empty(t, r.rec.Take(time.Second), "a request cut off by the step's end measured nothing")

	r.rec.Hold()
	r.hit(context.Background(), "api", "GET /apps", http.MethodGet, r.c.API("/apps"), "", Credential{}, is2xx)
	got := r.rec.Take(time.Second)
	require.Len(t, got, 1)
	require.Equal(t, uint64(1), got[0].Errors, "a 401 is not the 2xx an API client expects")

	require.True(t, expect(200, 302)(302))
	require.False(t, expect(200)(503))
}

func TestDropsAreCountedPerSurface(t *testing.T) {
	r := &runner{}
	r.drop("api")
	r.drop("api")
	r.drop("proxy")
	require.Equal(t, map[string]uint64{"api": 2, "proxy": 1}, r.drops())
	r.dropped.Clear()
	require.Empty(t, r.drops())

	u, c := r.someSession()
	require.Empty(t, u+c, "nobody online")
}
