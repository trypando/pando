package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// HeaderPrefix is Pando's header namespace.
//
// Every inbound header starting with this is stripped unconditionally before
// anything is set (R-053). See stripInbound — that loop is the single most
// security-critical piece of code in the system.
const HeaderPrefix = "X-Pando-"

// CookiePrefix is Pando's cookie namespace.
//
// Every cookie starting with this is removed from the request before it is
// forwarded to an app (see stripCookies). A prefix rather than one name, for
// the same reason HeaderPrefix is: the rule has to cover the cookie nobody has
// added yet.
const CookiePrefix = "pando_"

// PasscodeCookiePrefix names the cookie that proves a visitor entered an app's
// passcode (R-075a): PasscodeCookiePrefix + the app's ID. In Pando's namespace,
// so stripCookies removes it before anything reaches the app — it is a
// credential for that app, and the app is not entitled to hold it.
const PasscodeCookiePrefix = CookiePrefix + "pass_"

// Convenience headers, sent alongside the assertion and documented as
// UNVERIFIED (R-053). An app that trusts them is trusting the network boundary,
// which is a legitimate choice a developer must know they are making.
const (
	HeaderUser   = "X-Pando-User"
	HeaderEmail  = "X-Pando-Email"
	HeaderGroups = "X-Pando-Groups"
)

// Resolver finds which app a request is for.
type Resolver interface {
	// ByHostname resolves an app from the Host header.
	ByHostname(ctx context.Context, hostname string) (state.App, *spec.AppSpec, bool, error)
	// ByPath resolves the path-mode app whose path is the longest whole-segment
	// prefix of the request's path, and returns that path to strip (R-167).
	ByPath(ctx context.Context, path string) (state.App, *spec.AppSpec, string, bool, error)
	// BySlug resolves an app from the first path segment, for proxy mode.
	BySlug(ctx context.Context, slug string) (state.App, *spec.AppSpec, bool, error)
	// ByPort resolves an app from the port the request arrived on, for port
	// mode (design 03 §4.2).
	ByPort(ctx context.Context, port int) (state.App, *spec.AppSpec, bool, error)
}

// Authenticator resolves a request's credentials to a principal.
type Authenticator interface {
	Authenticate(r *http.Request) (authz.Principal, error)
}

// Upstreams says how to reach an app's primary workload.
type Upstreams interface {
	Primary(ctx context.Context, app state.App, s *spec.AppSpec) (api.Upstream, error)
}

// Metrics counts what passed through, so "there is no bypass" is observable
// rather than merely intended.
type Metrics interface {
	Request(appID string, kind authz.PrincipalKind, allowed bool)
}

// Proxy is the single enforcement point for every request to every app (R-023).
//
// There is no bypass — not for public apps, not for performance, not for
// websockets. If you are adding a fast path, you are adding a security hole.
type Proxy struct {
	Resolver      Resolver
	Authenticator Authenticator

	// Cache, when set, keeps what a request reads about its session for a
	// few seconds, emptied on every change (cache.go, issue #93). Give the
	// resolver the same one, and build Authz over Cache.Store.
	Cache     *Cache
	Authz     *authz.Authorizer
	Minter    *assertion.Minter
	Upstreams Upstreams
	Auditor   AuditWriter
	Metrics   Metrics
	Logger    *zap.Logger

	// UsePolicy says whether anonymous use is recorded (R-227). Nil records
	// it, which is the default.
	UsePolicy UsePolicy

	// ExternalURL is how a browser reaches this installation, which decides
	// whether the visit cookie is marked Secure — the same rule as the session
	// cookie (O-19). Nil falls back to whether the request arrived over TLS.
	ExternalURL *url.URL

	visitsOnce sync.Once
	visits     *visits
	clock      func() time.Time

	// upstream is the one transport every forwarded request shares, so a
	// connection to an app is reused rather than dialed per request. It was
	// built per request, which reused nothing and left each request's idle
	// socket open for IdleConnTimeout.
	upstreamOnce sync.Once
	upstream     *http.Transport

	// The transport for upstreams the runtime dials (dialed), and each one's
	// Dial by the host dialed gives it.
	dialsOnce     sync.Once
	dialTransport *http.Transport
	dials         sync.Map

	// reauthEvery is how often a long-lived connection is re-authorized. Zero
	// is assertion.Lifetime, which is what it is everywhere but in a test.
	reauthEvery time.Duration

	// LoginPath is where an unauthenticated caller is sent.
	//
	// It has to be a path Pando answers on *every* hostname, not only its own.
	// On an app's own hostname the console does not serve "/login" — the proxy
	// does, because every path there belongs to the app — so sending somebody
	// to "/login" there redirected them to the page that had just redirected
	// them, forever. See httpapi.LoginPath.
	LoginPath string

	// Mode is the install's default shape, for building URLs to show people
	// (design 03 §4.1). It is NOT how a request is resolved: resolution tries
	// both, because an install can mix the two and a request does not care what
	// the default was.
	Mode spec.RoutingMode
}

// ServeHTTP runs the request path from design 06 §4.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. Resolve the app.
	app, appSpec, prefix, found, err := p.resolve(r)
	if err != nil {
		p.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	if !found {
		p.fail(w, r, http.StatusNotFound, "There is no app at this address.")
		return
	}

	ctx = log.With(ctx, zap.String("app_id", app.ID))
	ctx = context.WithValue(ctx, ctxKeyAppID{}, app.ID)
	r = r.WithContext(ctx)

	// 2. Is it running?
	if app.State != state.StateRunning && app.State != state.StateDegraded {
		p.fail(w, r, http.StatusServiceUnavailable,
			"This app isn't running right now. Try again in a moment.")
		return
	}

	// 3-4. Authenticate, or fall through as anonymous.
	//
	// A bad credential is not fatal here: it resolves to anonymous and the data
	// check decides. That keeps one code path for every caller rather than a
	// separate one for "authentication failed".
	// Only a Pando-shaped bearer is Pando's here; any other Authorization
	// header is the app's own login and is left to it (credentials.go).
	principal, err := p.authenticate(r)
	if err != nil {
		principal = authz.Anonymous()
	}
	// Any passcode this browser has entered, for CheckData to check (R-075a).
	// Carried on the principal, signed in or not: someone with an account but
	// no grant on a passcode app gets in the same way a stranger does.
	principal.Passcodes = passcodesFrom(r)

	// 5. CheckData. The only authorization decision on this path.
	denial := p.Authz.CheckData(ctx, principal, app.ID)
	allowed := denial == nil

	// Counted before the branch, so the anonymous path is provably not a bypass:
	// every request to every app increments this, whatever the outcome.
	if p.Metrics != nil {
		p.Metrics.Request(app.ID, principal.Kind, allowed)
	}

	if !allowed {
		if errs.CodeOf(denial) == errs.PermPasscodeRequired {
			p.redirectToPasscode(w, r, app.ID)
			return
		}
		if principal.Kind == authz.KindAnonymous {
			// Send them to sign in, with somewhere to come back to.
			p.redirectToLogin(w, r)
			return
		}
		p.auditDenial(r, principal, app.ID)
		p.fail(w, r, http.StatusForbidden,
			"You don't have access to this app. Ask whoever set it up to share it with you.")
		return
	}

	// Who used the app, once per visit (R-227). After the decision, so only an
	// allowed use is recorded as one, and before the request is forwarded, so
	// it is recorded whatever the app does with it (design 06 §6).
	ctx = context.WithValue(ctx, ctxKeyPrefix{}, prefix)
	r = r.WithContext(ctx)
	p.visitsOnce.Do(func() { p.visits = newVisits() })
	visit := p.recordUse(r, principal, app.ID)

	// 6. Mint the assertion.
	token, err := p.Minter.Mint(assertion.Claims{
		Sub:    subjectOf(principal),
		Email:  principal.Email,
		Name:   principal.DisplayName,
		Groups: principal.Groups,
		Aud:    app.ID,
	})
	if err != nil {
		p.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}

	upstream, err := p.Upstreams.Primary(ctx, app, appSpec)
	if err != nil || upstream.URL == "" {
		p.fail(w, r, http.StatusServiceUnavailable,
			"This app isn't reachable right now. Try again in a moment.")
		return
	}

	target, err := url.Parse(upstream.URL)
	if err != nil {
		p.fail(w, r, http.StatusServiceUnavailable, "This app isn't reachable right now.")
		return
	}

	// 11. Forward. The decision and the assertion are done above, and steps
	// 7, 8 and 10 happen in forward's Rewrite on the outbound request
	// whichever way it travels. An upstream with a Dial is reached through
	// the forwarding agent on its host (O-45, design 06 §4): only the
	// connection differs.
	transport := http.RoundTripper(p.transport())
	if upstream.Dial != nil {
		target, transport = p.dialed(target, upstream)
	}

	p.forward(w, r, target, transport, token, principal, prefix, visit)
}

// forward sets the headers and proxies the request. visit, when set, is the
// cookie marking this browser's visit, added to the app's response.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, target *url.URL, transport http.RoundTripper, token string, principal authz.Principal, prefix string, visit *http.Cookie) {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host

			// 7. STRIP every inbound header in Pando's namespace.
			//
			// This is a security requirement, not hygiene. Without it a client
			// sets X-Pando-User: admin@corp.com and any app trusting the
			// convenience headers is trivially spoofed.
			//
			// Done on the OUTBOUND request, unconditionally, before anything is
			// set — so there is no ordering in which a forged value survives.
			stripInbound(pr.Out.Header)

			// And Pando's own cookies, which are credentials.
			//
			// The session cookie was being forwarded verbatim: every app Pando
			// hosts received `pando_session=ses_…` on every request, and could
			// replay it against Pando's API as the person visiting it. Under
			// path routing the browser sends it because the app shares Pando's
			// origin; under subdomain routing it would be whatever the cookie's
			// domain covers. Either way an app is not entitled to it — the
			// assertion below is what an app is given, and it is scoped to that
			// app (R-054), signed, and short-lived, which a session cookie is
			// none of.
			stripCookies(pr.Out)

			// And a Pando API token, for the same reason, whether or not it
			// authenticated. Any other Authorization value is the app's own
			// and goes through (credentials.go).
			stripPandoTokens(pr.Out.Header)

			// 8. Set the assertion and the convenience headers.
			pr.Out.Header.Set(assertion.Header, token)
			pr.Out.Header.Set(HeaderUser, subjectOf(principal))
			if principal.Email != "" {
				pr.Out.Header.Set(HeaderEmail, principal.Email)
			}
			if len(principal.Groups) > 0 {
				pr.Out.Header.Set(HeaderGroups, strings.Join(principal.Groups, ","))
			}

			// 9. Path mode: strip the prefix and say what was stripped (R-167).
			if prefix != "" {
				pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
				if !strings.HasPrefix(pr.Out.URL.Path, "/") {
					pr.Out.URL.Path = "/" + pr.Out.URL.Path
				}
				pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
			}

			pr.SetXForwarded()
		},

		// The visit cookie rides on the app's response, in Pando's namespace:
		// the browser returns it, and stripCookies takes it off again before
		// the app sees a request.
		ModifyResponse: func(resp *http.Response) error {
			if visit != nil {
				resp.Header.Add("Set-Cookie", visit.String())
			}
			return nil
		},

		// R-170: streaming must work. FlushInterval -1 disables response
		// buffering entirely, so SSE arrives as it is produced rather than in
		// one lump at the end. Websocket upgrades are hijacked by
		// ReverseProxy itself and are unaffected by this.
		FlushInterval: -1,

		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.From(r.Context()).Warn("upstream failed", zap.Error(err))
			p.fail(w, r, http.StatusBadGateway,
				"This app didn't respond. It may still be starting up.")
		},

		// No response body limit, deliberately (R-170): large uploads and
		// downloads must pass through.
		Transport: transport,
	}

	// Long-lived connections are re-authorized for as long as they stay open
	// (O-13). Wrapping here rather than inside ReverseProxy keeps the decision
	// with the rest of the authorization logic.
	rp.ServeHTTP(&reauthorizing{
		ResponseWriter: w,
		proxy:          p,
		request:        r,
		appID:          appIDOf(r),
	}, r)
}

type ctxKeyAppID struct{}

func appIDOf(r *http.Request) string {
	id, _ := r.Context().Value(ctxKeyAppID{}).(string)
	return id
}

// stripInbound removes every header in Pando's namespace.
//
// Iterating the map and deleting by prefix — rather than deleting a known list —
// is deliberate: a header added to the namespace later is stripped without
// anyone remembering to update this. Header keys are canonicalized by net/http,
// so a lowercase or mixed-case forgery is caught by the same comparison.
func stripInbound(h http.Header) {
	for name := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), HeaderPrefix) {
			h.Del(name)
		}
	}
	// X-Forwarded-Prefix is Pando's to set, so an inbound one is also a forgery.
	h.Del("X-Forwarded-Prefix")
}

// stripCookies removes Pando's own cookies from a request bound for an app.
//
// Rebuilt rather than edited: there is no "delete one cookie" on a header that
// holds them all in one line, and a regexp over that line is how a cookie whose
// value contains a semicolon survives the deletion.
func stripCookies(r *http.Request) {
	var kept []*http.Cookie
	for _, c := range r.Cookies() {
		if !strings.HasPrefix(c.Name, CookiePrefix) {
			kept = append(kept, c)
		}
	}

	r.Header.Del("Cookie")
	for _, c := range kept {
		r.AddCookie(c)
	}
}

// resolve finds the app and, for a path-addressed one, the prefix to strip.
//
// Both addressing modes are tried, always, because design 03 §4.1 says neither
// is a global setting: the topology is the aggregate of each app's
// Routing.Mode, and an install can mix them — an internal tool on a path and a
// customer-facing app on its own hostname, on one Pando.
//
// This used to switch on p.Mode and resolve one way only, which made the
// install-wide default a hard constraint and quietly broke the other half of
// every mixed install: a subdomain app on a path-mode install resolved to
// nothing and fell through to the console, looking like the app did not exist.
//
// Hostname first. A request whose Host names an app is unambiguous, and a
// path-addressed app never has a hostname to be found under — so the order
// costs nothing and avoids mistaking a path segment for an app when the real
// answer was the Host.
func (p *Proxy) resolve(r *http.Request) (state.App, *spec.AppSpec, string, bool, error) {
	ctx := r.Context()

	// Port mode first, and unconditionally: a request that arrived on an app's
	// own port is that app's, whatever Host it carries and whatever its path
	// begins with. The port is the address (design 03 §4.2), and an app served
	// at the root of one gets no prefix — which is the point of the mode, and
	// the only way an app that writes "/assets/app.js" into its own HTML can
	// work at all (R-167).
	if port, ok := localPort(ctx); ok {
		app, s, found, err := p.Resolver.ByPort(ctx, port)
		return app, s, "", found, err
	}

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != "" {
		app, s, found, err := p.Resolver.ByHostname(ctx, host)
		if err != nil {
			return state.App{}, nil, "", false, err
		}
		if found {
			return app, s, "", true, nil
		}
	}

	// A path-mode app's own path, which may be deeper than one segment and is
	// not necessarily its slug. The longest match wins, and the address
	// check at pin time (state.checkAddress) keeps two apps from nesting, so
	// "longest" never has to choose between two apps' claims.
	if app, s, prefix, found, err := p.Resolver.ByPath(ctx, r.URL.Path); err != nil {
		return state.App{}, nil, "", false, err
	} else if found {
		return app, s, prefix, true, nil
	}

	// Then the slug, as every app has always answered at /<slug> on Pando's
	// own address. The address check refuses a path that takes another app's
	// slug, so this cannot reach an app some other path already claimed.
	segment := firstSegment(r.URL.Path)
	if segment == "" {
		return state.App{}, nil, "", false, nil
	}
	app, s, found, err := p.Resolver.BySlug(ctx, segment)
	return app, s, "/" + segment, found, err
}

// redirectToLogin sends an anonymous caller to sign in.
func (p *Proxy) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	loginPath := p.LoginPath
	if loginPath == "" {
		loginPath = "/.pando/login"
	}
	target := loginPath + "?next=" + url.QueryEscape(r.URL.RequestURI())
	http.Redirect(w, r, target, http.StatusFound)
}

// redirectToPasscode sends a visitor to enter an app's passcode: the sign-in
// page, in its passcode mode, on the same reserved path (R-172) — so it works
// on an app's own hostname for the same reason sign-in does, and no new path
// is taken from the apps.
func (p *Proxy) redirectToPasscode(w http.ResponseWriter, r *http.Request, appID string) {
	loginPath := p.LoginPath
	if loginPath == "" {
		loginPath = "/.pando/login"
	}
	target := loginPath + "?passcode=" + url.QueryEscape(appID) + "&next=" + url.QueryEscape(r.URL.RequestURI())
	http.Redirect(w, r, target, http.StatusFound)
}

// passcodesFrom reads the passcode unlocks a request carries, by app ID.
func passcodesFrom(r *http.Request) map[string]string {
	var out map[string]string
	for _, c := range r.Cookies() {
		if appID, ok := strings.CutPrefix(c.Name, PasscodeCookiePrefix); ok && appID != "" && c.Value != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[appID] = c.Value
		}
	}
	return out
}

// auditDenial records a refused request.
//
// Only for authenticated callers. An anonymous caller reaching a private app is
// the ordinary case — every crawler and stray link does it — and auditing those
// would bury the denials that mean something.
func (p *Proxy) auditDenial(r *http.Request, principal authz.Principal, appID string) {
	if p.Auditor == nil {
		return
	}
	_ = p.Auditor.Write(r.Context(), audit.Event{
		PrincipalKind: audit.PrincipalKind(principal.Kind),
		PrincipalID:   principal.ID,
		OnBehalfOf:    principal.UserID,
		Action:        "app.use.denied",
		AppID:         appID,
		Detail:        map[string]any{"path": r.URL.Path},
	})
}

// fail writes an error page in the product's voice.
//
// Plain text rather than the API's JSON envelope: the caller here is a browser
// showing a page to a person, not a client parsing a response.
func (p *Proxy) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	log.From(r.Context()).Info("proxy refused", zap.Int("status", status))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, message)
}

// subjectOf returns the assertion subject for a principal.
//
// An anonymous request still gets an assertion, with the constant
// "anonymous" (R-056). The consequence, which the app-developer documentation
// must state: absence of the assertion header means the request did not come
// through Pando at all, and an app may reject on that basis.
func subjectOf(p authz.Principal) string {
	if p.Kind == authz.KindAnonymous || p.UserID == "" {
		if p.Kind == authz.KindToken && p.ID != "" {
			return p.ID
		}
		return assertion.AnonymousSubject
	}
	return p.UserID
}

func firstSegment(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}

// transport returns the transport shared by every request this proxy forwards.
func (p *Proxy) transport() *http.Transport {
	p.upstreamOnce.Do(func() { p.upstream = newTransport() })
	return p.upstream
}

// newTransport is how the proxy reaches apps. One per Proxy: an app's
// connections are kept and reused across requests, up to
// maxIdleConnsPerHost each, because every request to an app goes to the same
// address and a busy app would otherwise dial for each one. Go's default of
// two idle connections per host is what an HTTP client talking to many hosts
// wants, not a reverse proxy talking to a few hosts a great deal.
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,

		// Streaming depends on this: with compression enabled the transport
		// buffers to decompress, which defeats FlushInterval for SSE.
		DisableCompression: true,
	}
}

// maxIdleConnsPerHost is how many idle connections to one app are kept for
// reuse. A host here is one app's primary workload.
const maxIdleConnsPerHost = 64

// dialedHostSuffix ends the host the proxy gives an upstream it reaches
// through the runtime's Dial. .invalid cannot resolve (RFC 2606), so nothing
// addressed to one can leave by any route but the Dial registered for it.
const dialedHostSuffix = ".dial.pando.invalid"

// dialed returns the URL and transport for an upstream the runtime dials
// itself (api.Upstream.Dial).
//
// The transport pools connections by URL host, and a connection the Dial
// opened is a stream to one workload. So the URL's host is replaced by one
// derived from the PoolKey, which the runtime promises means one destination,
// and the Dial is registered under it; the transport's DialContext looks it up
// there. The Host header is unaffected: forward sets it from the request.
//
// A separate transport from the direct one, with no HTTP proxy from the
// environment: a dialed request goes through the agent or nowhere.
func (p *Proxy) dialed(target *url.URL, upstream api.Upstream) (*url.URL, http.RoundTripper) {
	key := upstream.PoolKey
	if key == "" {
		key = target.Host
	}
	sum := sha256.Sum256([]byte(key))
	port := target.Port()
	if port == "" {
		port = "80"
	}
	host := hex.EncodeToString(sum[:12]) + dialedHostSuffix
	p.dialsOnce.Do(func() { p.dialTransport = newDialTransport(&p.dials) })
	p.dials.Store(net.JoinHostPort(host, port), upstream.Dial)

	out := *target
	out.Scheme = "http"
	out.Host = net.JoinHostPort(host, port)
	return &out, p.dialTransport
}

func newDialTransport(dials *sync.Map) *http.Transport {
	t := newTransport()
	t.Proxy = nil
	t.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		dial, ok := dials.Load(addr)
		if !ok {
			return nil, fmt.Errorf("no route to %s", addr)
		}
		return dial.(func(context.Context) (net.Conn, error))(ctx)
	}
	return t
}
