package proxy_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/trypando/pando/internal/secret"
)

const (
	hookKey    = "a-webhook-secret-of-some-length"
	hookPath   = "/api/v1/webhook/noodle"
	hookHeader = "X-Hub-Signature-256"
	hookBody   = `{"delivery":1}`
)

// hookStore is the database's part of a WebhookStore, in memory.
type hookStore struct {
	mu       sync.Mutex
	secrets  map[string]string
	seen     map[string]state.WebhookClaim
	claimErr error

	secretErr, settleErr error
}

func (h *hookStore) Get(_ context.Context, _, route string) (secret.Value, bool, error) {
	key, ok := h.secrets[route]
	return secret.New(key), ok, h.secretErr
}

func (h *hookStore) Claim(_ context.Context, _, route string, digest []byte, _, _ time.Time) (state.WebhookClaim, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.claimErr != nil {
		return 0, h.claimErr
	}
	key := route + string(digest)
	if held, ok := h.seen[key]; ok {
		return held, nil
	}
	h.seen[key] = state.WebhookInFlight
	return state.WebhookClaimed, nil
}

func (h *hookStore) Accept(_ context.Context, _, route string, digest []byte, _ time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen[route+string(digest)] = state.WebhookAccepted
	return h.settleErr
}

func (h *hookStore) Release(_ context.Context, _, route string, digest []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.seen, route+string(digest))
	return nil
}

type hookPolicy struct{ disabled, quiet bool }

func (p hookPolicy) WebhookRoutesDisabled(context.Context) bool  { return p.disabled }
func (p hookPolicy) AuditsAnonymousDenials(context.Context) bool { return !p.quiet }

type hookAudit struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (a *hookAudit) Write(_ context.Context, e audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return a.err
}

func (a *hookAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []string{}
	for _, e := range a.events {
		out = append(out, e.Action)
	}
	return out
}

// hookApp is a private app with webhook routes, and what its workload saw.
type hookApp struct {
	proxy  *proxy.Proxy
	store  *hookStore
	audit  *hookAudit
	status int
	mu     sync.Mutex
	got    []*http.Request
	bodies []string
}

func (a *hookApp) forwarded() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.got)
}

func hmacRoute() spec.WebhookRoute {
	return spec.WebhookRoute{Name: "noodle", Method: http.MethodPost, Path: hookPath,
		Verification: spec.WebhookVerification{Kind: spec.WebhookHMACSHA256, Header: hookHeader, Prefix: "sha256="}}
}

func headerRoute() spec.WebhookRoute {
	return spec.WebhookRoute{Name: "plain", Method: http.MethodPost, Path: "/hooks/plain",
		Verification: spec.WebhookVerification{Kind: spec.WebhookHeader, Header: "X-Token"}}
}

func newHookApp(t *testing.T, principal authz.Principal, routes ...spec.WebhookRoute) *hookApp {
	t.Helper()
	app := &hookApp{
		store: &hookStore{secrets: map[string]string{"noodle": hookKey, "plain": hookKey, "repeats": hookKey},
			seen: map[string]state.WebhookClaim{}},
		audit:  &hookAudit{},
		status: http.StatusOK,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		app.mu.Lock()
		app.got = append(app.got, r)
		app.bodies = append(app.bodies, string(body))
		status := app.status
		app.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(upstream.Close)

	s := newStore()
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	app.proxy = &proxy.Proxy{
		Resolver: &resolver{
			app: state.App{ID: appID, Slug: "notes", State: state.StateRunning},
			spec: &spec.AppSpec{
				Routing: spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "notes.test"},
				Workloads: []spec.Workload{{Name: "web", Primary: true,
					Ports: []spec.Port{{Number: 80, Protocol: "http"}}}},
				WebhookRoutes: routes,
			},
		},
		Authenticator: staticAuth{principal: principal},
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     fixedUpstream{addr: upstream.URL},
		Auditor:       app.audit,
		Metrics:       proxy.NewCounters(),
		Logger:        zap.NewNop(),
		LoginPath:     "/.pando/login",
		Mode:          spec.RoutingSubdomain,
		Webhooks:      app.store,
	}
	return app
}

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(hookKey))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (a *hookApp) send(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://notes.test"+path, strings.NewReader(body))
	req.RemoteAddr = "198.51.100.7:40000"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.proxy.ServeHTTP(rec, req)
	return rec
}

func (a *hookApp) deliver(body string) *httptest.ResponseRecorder {
	return a.send(http.MethodPost, hookPath, body, map[string]string{hookHeader: sign(body)})
}

// TestR425_AVerifiedDeliveryReachesOnePathOfAPrivateApp asserts the feature: a
// signed POST to the route gets through a private app with its body intact, and
// everything else about the app stays private.
func TestR425_AVerifiedDeliveryReachesOnePathOfAPrivateApp(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())

	require.Equal(t, http.StatusOK, app.deliver(hookBody).Code)
	require.Equal(t, 1, app.forwarded())
	require.Equal(t, hookBody, app.bodies[0], "the body read to verify it is what the app receives")

	for _, try := range []struct{ method, path string }{
		{http.MethodGet, hookPath},
		{http.MethodPut, hookPath},
		{http.MethodPost, "/"},
		{http.MethodPost, "/api/v1/webhook"},
		{http.MethodPost, hookPath + "/more"},
		{http.MethodPost, hookPath + "/"},
	} {
		rec := app.send(try.method, try.path, hookBody, map[string]string{hookHeader: sign(hookBody)})
		require.Equal(t, http.StatusFound, rec.Code, "%s %s is the private app's sign-in", try.method, try.path)
	}
	require.Equal(t, 1, app.forwarded())
}

// TestR425_AnUncleanPathMatchesNoRoute asserts a path is compared as sent, not
// cleaned up into one that matches.
func TestR425_AnUncleanPathMatchesNoRoute(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	for _, path := range []string{
		"/api/v1/webhook/x/../noodle",
		"/api/v1//webhook/noodle",
		"/api/v1/webhook%2Fnoodle",
		"/api/v1/webhook/%6Eoodle",
		"/./api/v1/webhook/noodle",
	} {
		rec := app.send(http.MethodPost, path, hookBody, map[string]string{hookHeader: sign(hookBody)})
		require.Equal(t, http.StatusFound, rec.Code, path)
	}
	require.Zero(t, app.forwarded())
}

// TestR425_PathRoutingMatchesThePathTheAppReceives asserts a route is written
// without the app's prefix (R-167).
func TestR425_PathRoutingMatchesThePathTheAppReceives(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	app.proxy.Resolver = &prefixResolver{Resolver: app.proxy.Resolver, prefix: "/notes"}
	rec := app.send(http.MethodPost, "/notes"+hookPath, hookBody, map[string]string{hookHeader: sign(hookBody)})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, hookPath, app.got[0].URL.Path)
}

type prefixResolver struct {
	proxy.Resolver
	prefix string
}

func (r *prefixResolver) ByHostname(context.Context, string) (state.App, *spec.AppSpec, bool, error) {
	return state.App{}, nil, false, nil
}

func (r *prefixResolver) ByPath(ctx context.Context, _ string) (state.App, *spec.AppSpec, string, bool, error) {
	app, s, _, err := r.Resolver.ByHostname(ctx, "notes.test")
	return app, s, r.prefix, true, err
}

// TestR426_AHeaderRouteComparesTheSecret asserts the second kind of
// verification, which forwards the header and has no replay check.
func TestR426_AHeaderRouteComparesTheSecret(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), headerRoute())

	rec := app.send(http.MethodPost, "/hooks/plain", "{}", map[string]string{"X-Token": "wrong"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	for range 2 {
		rec = app.send(http.MethodPost, "/hooks/plain", "{}", map[string]string{"X-Token": hookKey})
		require.Equal(t, http.StatusOK, rec.Code)
	}
	require.Equal(t, 2, app.forwarded(), "a header route has no replay check (R-433)")
	require.Equal(t, hookKey, app.got[0].Header.Get("X-Token"), "the proof is forwarded (R-428)")
	require.Empty(t, app.store.seen)
}

// TestR426_ABadSignatureIsRefused asserts what does not verify.
func TestR426_ABadSignatureIsRefused(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	good := sign(hookBody)
	for name, proof := range map[string]string{
		"wrong body":  sign("something else"),
		"not hex":     "sha256=zz",
		"no prefix":   strings.TrimPrefix(good, "sha256="),
		"short":       good[:20],
		"other kind":  "sha1=" + strings.TrimPrefix(good, "sha256="),
		"just prefix": "sha256=",
	} {
		rec := app.send(http.MethodPost, hookPath, hookBody, map[string]string{hookHeader: proof})
		require.Equal(t, http.StatusUnauthorized, rec.Code, name)
	}
	require.Zero(t, app.forwarded())
}

// TestR428_ADeliveryIsStillAnOrdinaryProxiedRequest asserts forged identity
// headers and Pando's cookies are stripped, and use is recorded once per route.
func TestR428_ADeliveryIsStillAnOrdinaryProxiedRequest(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	rec := app.send(http.MethodPost, hookPath, hookBody, map[string]string{
		hookHeader: sign(hookBody), "X-Pando-User": "usr_forged", "Cookie": "pando_session=stolen; theirs=1",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	got := app.got[0]
	require.Equal(t, "webhook:noodle", got.Header.Get(proxy.HeaderUser))
	require.Equal(t, "theirs=1", got.Header.Get("Cookie"))
	require.Equal(t, sign(hookBody), got.Header.Get(hookHeader), "an app that also checks keeps working")

	require.Equal(t, http.StatusOK, app.deliver(`{"delivery":2}`).Code)
	require.Equal(t, []string{"app.use"}, app.audit.actions(), "once per visit, and a route is its own visit")
	use := app.audit.events[0]
	require.Equal(t, audit.KindAnonymous, use.PrincipalKind, "audit_events has no webhook kind")
	require.Equal(t, "webhook:noodle", use.PrincipalID)
}

// TestR428_ABodyOverTheLimitIsRefused asserts an HMAC route's bound.
func TestR428_ABodyOverTheLimitIsRefused(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	rec := app.deliver(strings.Repeat("x", 8<<20+1))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Zero(t, app.forwarded())
}

// TestR428_TheLimitIsTakenBeforeTheBodyIsRead asserts a flood meets the
// anonymous limit, and that a verified delivery is charged once, not twice.
func TestR428_TheLimitIsTakenBeforeTheBodyIsRead(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	app.proxy.SetClock(func() time.Time { return now })
	served := 0
	for i := range 1000 {
		rec := app.deliver(fmt.Sprintf(`{"delivery":%d}`, i))
		if rec.Code == http.StatusTooManyRequests {
			break
		}
		require.Equal(t, http.StatusOK, rec.Code)
		served++
	}
	require.Equal(t, 300, served, "the burst, one charge each")
	bad := app.send(http.MethodPost, hookPath, hookBody, map[string]string{hookHeader: "sha256=00"})
	require.Equal(t, http.StatusTooManyRequests, bad.Code, "guessing is limited the same way")
}

// TestR429_ADeliveryHasItsOwnIdentity asserts the assertion's subject, and that
// the principal is good for this app and no other, on neither plane beyond it.
func TestR429_ADeliveryHasItsOwnIdentity(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	require.Equal(t, http.StatusOK, app.deliver(hookBody).Code)

	claims, err := app.proxy.Minter.Verify(app.got[0].Header.Get(assertion.Header))
	require.NoError(t, err)
	require.Equal(t, "webhook:noodle", claims.Sub)
	require.Empty(t, claims.Email)
	require.Empty(t, claims.Name)
	require.Empty(t, claims.Groups)
	require.Empty(t, app.got[0].Header.Get(proxy.HeaderEmail))

	s := newStore()
	az := authz.New(s, nil, nil)
	p := authz.Webhook(appID, "noodle")
	require.NoError(t, az.CheckData(context.Background(), p, appID))
	require.Error(t, az.CheckData(context.Background(), p, "app_other"))
	require.Error(t, az.CheckControl(context.Background(), p, appID, authz.AppView))
}

// TestR429_AnUpgradeMatchesNoRoute asserts a delivery cannot become a
// connection that outlives the request it was verified for.
func TestR429_AnUpgradeMatchesNoRoute(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	rec := app.send(http.MethodPost, hookPath, hookBody, map[string]string{
		hookHeader: sign(hookBody), "Connection": "Upgrade", "Upgrade": "websocket",
	})
	require.Equal(t, http.StatusFound, rec.Code)
	require.Zero(t, app.forwarded())
}

// TestR430_AFailedDeliveryIsRefusedAndAudited asserts a 401, not sign-in, on a
// private app and a public one, with the refusal audited under the anonymous
// rule, and never naming the proof.
func TestR430_AFailedDeliveryIsRefusedAndAudited(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	rec := app.send(http.MethodPost, hookPath, hookBody, map[string]string{hookHeader: "sha256=00"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "could not be verified")
	require.Equal(t, []string{"app.webhook.refused"}, app.audit.actions())
	refused := app.audit.events[0]
	require.Equal(t, "bad_signature", refused.Detail["reason"])
	require.Equal(t, "webhook:noodle", refused.PrincipalID)
	require.Equal(t, "198.51.100.7", refused.Detail["remote_addr"])
	require.NotContains(t, fmt.Sprint(refused.Detail), "sha256=00")

	require.Equal(t, http.StatusUnauthorized, app.send(http.MethodPost, hookPath, hookBody, nil).Code, "no proof at all")

	app.proxy.WebhookPolicy = hookPolicy{quiet: true}
	before := len(app.audit.actions())
	require.Equal(t, http.StatusUnauthorized, app.send(http.MethodPost, hookPath, hookBody, nil).Code)
	require.Len(t, app.audit.actions(), before, "disable_anonymous_denial_audit covers it")
}

// TestR430_APublicAppStillVerifiesItsRoutes asserts a route is checked whatever
// else the app allows.
func TestR430_APublicAppStillVerifiesItsRoutes(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	s := newStore()
	s.anonymous[appID] = true
	app.proxy.Authz = authz.New(s, nil, nil)

	require.Equal(t, http.StatusUnauthorized, app.send(http.MethodPost, hookPath, hookBody, nil).Code)
	require.Zero(t, app.forwarded())
	require.Equal(t, http.StatusOK, app.send(http.MethodGet, "/", "", nil).Code)
}

// TestR430_ARouteWithNoSecretRefusesEveryDelivery asserts a removed route stops
// at once, before the deploy that takes it out of the running spec.
func TestR430_ARouteWithNoSecretRefusesEveryDelivery(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	delete(app.store.secrets, "noodle")
	require.Equal(t, http.StatusUnauthorized, app.deliver(hookBody).Code)
	require.Equal(t, "no_secret", app.audit.events[0].Detail["reason"])
}

// TestR430_ACredentialDecidesAloneForItsHolder asserts a signed-in caller is
// never held to a route: no proof needed, and none accepted in place of a grant.
func TestR430_ACredentialDecidesAloneForItsHolder(t *testing.T) {
	user := activeUser("usr_1")
	app := newHookApp(t, user, hmacRoute())
	s := newStore()
	s.status["usr_1"] = "active"
	app.proxy.Authz = authz.New(s, nil, nil)

	rec := app.deliver(hookBody)
	require.Equal(t, http.StatusForbidden, rec.Code, "a good signature gives a signed-in stranger nothing")
	require.Zero(t, app.forwarded())
}

// TestR431_HostPolicyMakesTheProxyIgnoreRoutes asserts a delivery then meets the
// app's ordinary access.
func TestR431_HostPolicyMakesTheProxyIgnoreRoutes(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	app.proxy.WebhookPolicy = hookPolicy{disabled: true}
	require.Equal(t, http.StatusFound, app.deliver(hookBody).Code)
	require.Zero(t, app.forwarded())

	app.proxy.WebhookPolicy = nil
	app.proxy.Webhooks = nil
	require.Equal(t, http.StatusFound, app.deliver(hookBody).Code, "no store is no routes")
}

// TestR433_AnAcceptedSignatureIsNotForwardedAgain asserts the replay check: 200,
// so the sender stops, not forwarded, and audited.
func TestR433_AnAcceptedSignatureIsNotForwardedAgain(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	require.Equal(t, http.StatusOK, app.deliver(hookBody).Code)

	again := app.deliver(hookBody)
	require.Equal(t, http.StatusOK, again.Code)
	require.Equal(t, 1, app.forwarded())
	require.Contains(t, app.audit.actions(), "app.webhook.replayed")

	// The same signature in another case is the same delivery.
	upper := "sha256=" + strings.ToUpper(strings.TrimPrefix(sign(hookBody), "sha256="))
	require.Equal(t, http.StatusOK, app.send(http.MethodPost, hookPath, hookBody, map[string]string{hookHeader: upper}).Code)
	require.Equal(t, 1, app.forwarded(), "changing the hex's case is not a new delivery")

	require.Equal(t, http.StatusOK, app.deliver(`{"delivery":2}`).Code)
	require.Equal(t, 2, app.forwarded())
}

// TestR433_ARetryAfterAFailureGetsThrough asserts only a 2xx is accepted.
func TestR433_ARetryAfterAFailureGetsThrough(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	app.status = http.StatusInternalServerError
	require.Equal(t, http.StatusInternalServerError, app.deliver(hookBody).Code)

	app.status = http.StatusOK
	require.Equal(t, http.StatusOK, app.deliver(hookBody).Code)
	require.Equal(t, 2, app.forwarded())
}

// TestR433_ALostConnectionReleasesTheClaim asserts the app answering nothing
// counts as a failure.
func TestR433_ALostConnectionReleasesTheClaim(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	app.proxy.Upstreams = fixedUpstream{addr: "http://127.0.0.1:1"}
	require.Equal(t, http.StatusBadGateway, app.deliver(hookBody).Code)
	require.Empty(t, app.store.seen)
}

// TestR433_ACopyInFlightIsToldToComeBack asserts a signature not yet accepted is
// not reported as done, because the first copy may still fail.
func TestR433_ACopyInFlightIsToldToComeBack(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	digest := sha256.Sum256(mustDecode(t, strings.TrimPrefix(sign(hookBody), "sha256=")))
	app.store.seen["noodle"+string(digest[:])] = state.WebhookInFlight

	rec := app.deliver(hookBody)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "30", rec.Header().Get("Retry-After"))
	require.Zero(t, app.forwarded())
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

// TestR433_NoRecordNoForward asserts a claim that cannot be made refuses the
// delivery: forwarding it could not be undone, and the sender can retry.
func TestR433_NoRecordNoForward(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	app.store.claimErr = errors.New("database gone")
	rec := app.deliver(hookBody)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Zero(t, app.forwarded())
}

// TestR433_AllowRepeatsTurnsTheCheckOff asserts a sender of identical bodies can
// opt out.
func TestR433_AllowRepeatsTurnsTheCheckOff(t *testing.T) {
	route := hmacRoute()
	route.Name, route.AllowRepeats = "repeats", true
	app := newHookApp(t, authz.Anonymous(), route)
	require.Equal(t, http.StatusOK, app.deliver(hookBody).Code)
	require.Equal(t, http.StatusOK, app.deliver(hookBody).Code)
	require.Equal(t, 2, app.forwarded())
	require.Empty(t, app.store.seen)
}

// TestR433_AStoppedAppLeavesNoClaim asserts a 503 does not use the signature up.
func TestR433_AStoppedAppLeavesNoClaim(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	r := app.proxy.Resolver.(*resolver)
	r.app.State = state.StateStopped
	require.Equal(t, http.StatusServiceUnavailable, app.deliver(hookBody).Code)
	require.Empty(t, app.store.seen)
}

// TestR430_ASecretThatCannotBeReadRefusesTheDelivery asserts a failure to read
// fails closed: nothing reaches the app, and the answer names nothing.
func TestR430_ASecretThatCannotBeReadRefusesTheDelivery(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	app.store.secretErr = errors.New("database gone")
	rec := app.deliver(hookBody)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "database")
	require.Zero(t, app.forwarded())
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// TestR428_ABodyThatStopsArrivingIsRefused asserts a read that fails is refused,
// and an empty body is verified as what it is.
func TestR428_ABodyThatStopsArrivingIsRefused(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	req := httptest.NewRequest(http.MethodPost, "http://notes.test"+hookPath, brokenBody{})
	req.Header.Set(hookHeader, sign(hookBody))
	rec := httptest.NewRecorder()
	app.proxy.ServeHTTP(rec, req)
	require.Equal(t, http.StatusRequestTimeout, rec.Code)
	require.Zero(t, app.forwarded())

	req = httptest.NewRequest(http.MethodPost, "http://notes.test"+hookPath, nil)
	req.Body = nil
	req.Header.Set(hookHeader, sign(""))
	rec = httptest.NewRecorder()
	app.proxy.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestR428_AnAuditOrSettleFailureDoesNotChangeTheAnswer asserts what the app
// answered stands when recording it fails.
func TestR428_AnAuditOrSettleFailureDoesNotChangeTheAnswer(t *testing.T) {
	app := newHookApp(t, authz.Anonymous(), hmacRoute())
	app.audit.err = errors.New("audit gone")
	app.store.settleErr = errors.New("database gone")
	require.Equal(t, http.StatusOK, app.deliver(hookBody).Code)
	require.Equal(t, http.StatusUnauthorized, app.send(http.MethodPost, hookPath, hookBody, nil).Code)

	app.proxy.Auditor = nil
	require.Equal(t, http.StatusUnauthorized, app.send(http.MethodPost, hookPath, hookBody, nil).Code)
}
