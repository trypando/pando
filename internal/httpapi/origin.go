package httpapi

import (
	"context"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// Keeping an app's script away from Pando's API (issue #78).
//
// A browser attaches the session cookie to any request for the hostname it was
// set on, whoever's script makes the request. So the question is never only
// "is the cookie valid" but "whose page is asking", and there are two ways an
// app's page could be the one asking:
//
//   - From the app's own origin. Pando answers /.pando on every hostname it
//     serves (R-172), and signing in there sets the session cookie on that
//     hostname. Were the whole API behind /.pando, an app's script could call
//     it same-origin with its visitor's cookie and read the answer: mint a
//     token, read a secret, open exec. So on an app's hostname or port,
//     /.pando answers signing in and nothing else (signInRoutes), and every
//     other path is the app's, including the API's own root
//     (appHostnamesAreTheApps). R-173 strips
//     the cookie so an app cannot replay it; this is the same rule for the
//     app's page.
//
//   - From another origin on the same site. notes.example.com and
//     pando.example.com are one site, so a SameSite=Lax cookie goes with a
//     request from one to the other. The browser keeps the answer from the
//     page, but a write has already happened. sameOriginWrites refuses a
//     cookie-authenticated write that a browser says came from elsewhere.
//
// Under path routing, apps are served from Pando's own origin, and neither of
// these can tell an app's script from the console's. That is accepted with
// path routing and warned about wherever it is chosen (R-166).

// appListenerKey marks a request that arrived on a listener belonging to an
// app — a port-mode app's own socket (ReservedOrApp).
type appListenerKey struct{}

// signInRoutes are what /.pando answers on an app's hostname or port: the
// sign-in page and its assets, and the endpoints that page calls (R-172).
// Paths are after the prefix is removed; "{}" is one path segment and a
// trailing "*" is the rest of the path. Listed rather than derived so adding
// one is a decision a reviewer sees, as with consoleRoutes.
var signInRoutes = []struct{ method, pattern string }{
	{http.MethodGet, "/"},
	{http.MethodGet, "/index.html"},
	{http.MethodGet, "/login"},
	{http.MethodGet, "/assets/*"},
	{http.MethodGet, "/api/v1/setup"},
	{http.MethodGet, "/api/v1/auth/options"},
	{http.MethodGet, "/api/v1/auth/providers/{}/start"},
	{http.MethodGet, "/api/v1/auth/complete"},
	{http.MethodGet, "/api/v1/auth/failures/{}"},
	{http.MethodPost, sessionsPath},
	{http.MethodDelete, sessionsPath},
	{http.MethodGet, "/api/v1/me"},
	// R-046: a first-run password must be changed before anything else, on
	// whichever hostname the person signed in at. It needs the current one.
	{http.MethodPost, "/api/v1/me/password"},
	// Not two-factor setup (R-405). Its answers are an authenticator key and
	// recovery codes, which the app's own script could read here with its
	// visitor's cookie; GET /me's two_factor.setup_here tells the page to send
	// the person to Pando's own address for it instead.
	{http.MethodGet, "/api/v1/apps/{}/passcode"},
	{http.MethodPost, "/api/v1/apps/{}/passcode"},
}

func isSignInRoute(method, path string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	for _, route := range signInRoutes {
		if route.method == method && matchRoute(route.pattern, path) {
			return true
		}
	}
	return false
}

func matchRoute(pattern, path string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(path, prefix)
	}
	want := strings.Split(pattern, "/")
	got := strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] == "{}" {
			if got[i] == "" {
				return false
			}
			continue
		}
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

// onAppAddress reports whether a request is addressed to an app rather than
// to Pando: it came in on an app's own port, or its Host is an app's
// hostname. An error looking the hostname up counts as an app's, so a
// database fault narrows /.pando rather than widening it.
func (s *Server) onAppAddress(r *http.Request) bool {
	if onListener, _ := r.Context().Value(appListenerKey{}).(bool); onListener {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if s.AppHosts != nil {
		if isApp, err := s.AppHosts.IsAppHostname(r.Context(), host); err != nil || isApp {
			return true
		}
	}
	// A hostname that is neither Pando's nor an app's is treated as an
	// app's here: it gets the sign-in page and nothing else (R-166).
	return s.unknownHost(r.Context(), host)
}

// sessionsPath is where a password sign-in is posted, and a session ended.
const sessionsPath = "/api/v1/sessions"

// signsIn reports whether a request starts a session from a password: sign-in
// and first-run setup. The SSO flow has its own bind cookie (design 06 §3.2).
func signsIn(r *http.Request) bool {
	return r.Method == http.MethodPost && (r.URL.Path == sessionsPath || r.URL.Path == "/api/v1/setup")
}

// formContentType reports whether a request body is one an HTML form on
// another site can send without the browser asking Pando first: the three
// CORS-safelisted types. Sign-in refuses them, so a cross-site form cannot post
// one even from a browser that sends neither Origin nor Sec-Fetch-Site.
func formContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	switch mediaType {
	case "application/x-www-form-urlencoded", "multipart/form-data", "text/plain":
		return true
	}
	return false
}

// refuseFormBody answers a sign-in sent as a form body, and reports whether it
// did (formContentType).
func refuseFormBody(w http.ResponseWriter, r *http.Request) bool {
	if !formContentType(r) {
		return false
	}
	Error(w, r, errs.New(errs.ValidInvalid,
		"Pando signs in only from a JSON request body, and this one was sent as a form. "+
			"A form is what a page on another site can send on a visitor's behalf.").
		WithRemedy("Send the request with the header Content-Type: application/json."))
	return true
}

// appHostnamesAreTheApps hands every request on an app's hostname to the
// proxy, except /.pando and a pending ACME answer, before any of Pando's own
// routes can match it.
//
// On an app's hostname every path belongs to the app (R-172). /.pando was
// narrowed to signing in for issue #78, but /api/v1, /healthz and
// /.well-known are mounted at the root, and they answered on the app's
// hostname too: the app's script could call POST /api/v1/tokens with its
// visitor's session cookie, same-origin, and read back a token carrying every
// verb that visitor holds. This is the same rule as the /.pando guard, applied
// to the root, and it also stops Pando shadowing an app's own /api/v1 or
// /healthz.
//
// Pando's own external hostname is never an app's, so it is answered without a
// lookup. A lookup that fails refuses the API rather than guessing: serving it
// to what may be an app's origin is the direction that is not safe. Everything
// else falls back to Pando as before (consoleOrApp) — the console's pages and
// the health checks carry no authority of their own, and a health check that
// fails when Postgres blips gets the process killed.
func (s *Server) appHostnamesAreTheApps(next http.Handler) http.Handler {
	if s.AppHosts == nil || s.AppProxy == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if s.pandosOnEveryHostname(r, host) {
			next.ServeHTTP(w, r)
			return
		}
		isApp, err := s.AppHosts.IsAppHostname(r.Context(), host)
		switch {
		case err != nil && !underPath(r.URL.Path, "/api/v1"):
			next.ServeHTTP(w, r)
		case err != nil:
			Error(w, r, errs.Wrap(errs.Internal,
				"Pando could not tell whether this address belongs to an app, so it did not answer. "+
					"Pando looks this up in its database, which did not respond.", err).
				WithRemedy("Try again in a moment. If this continues, check that Pando's database is running."))
		case isApp:
			if s.answerACMEChallenge(w, r) {
				return
			}
			toApp(r)
			s.AppProxy.ServeHTTP(w, r)
		case s.unknownHost(r.Context(), host) && !answeredOnAnyHost(r.URL.Path):
			if s.answerACMEChallenge(w, r) {
				return
			}
			s.notPandosAddress(w, r, host)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// unknownHost reports whether a hostname is neither Pando's own nor one that
// can only be reached from nearby, once the install has said where it is
// (PANDO_SERVER_EXTERNAL_URL). The caller has already asked whether it is an
// app's.
//
// Such a hostname gets no console and no API (R-166). With wildcard DNS every
// name under a domain reaches Pando; an app's former hostname is one of them,
// and a service worker the app registered there while it was the app's still
// runs. Serving the console and API there would hand that script a visitor's
// Pando sign-in.
//
// An IP address, localhost, and a name without a dot are not wildcard DNS:
// they are how an operator, a health check, or a container on Pando's own
// network reaches it, and they are answered as before. So is every hostname
// on an install that has not said where it is, which cannot tell its own
// address from another.
func (s *Server) unknownHost(ctx context.Context, host string) bool {
	if s.ExternalURL == nil || s.ExternalURL.Hostname() == "" || host == "" {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if strings.EqualFold(host, s.ExternalURL.Hostname()) || net.ParseIP(host) != nil ||
		host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") {
		return false
	}
	// The console hostname a routing adapter serves Pando on, which may
	// differ from the external URL's.
	if s.PandoHostnames != nil {
		for _, h := range s.PandoHostnames(ctx) {
			if strings.EqualFold(strings.TrimSuffix(h, "."), host) {
				return false
			}
		}
	}
	return true
}

// answeredOnAnyHost reports whether a path is answered on every hostname:
// the health checks, which an orchestrator calls by whatever name it has, and
// the assertion keys, which are public and which an app fetches from wherever
// it can reach Pando (R-057). None carries any authority.
func answeredOnAnyHost(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/.well-known/jwks.json":
		return true
	}
	return false
}

// notPandosAddress answers a request for a hostname that is neither Pando's
// nor an app's.
func (s *Server) notPandosAddress(w http.ResponseWriter, r *http.Request, host string) {
	Error(w, r, errs.Newf(errs.NotFound,
		"There is no app at %s, and Pando's console and API are not served there. Pando's own address is %s.",
		host, s.ExternalURL.Host).
		WithRemedy("Open "+s.ExternalURL.Scheme+"://"+s.ExternalURL.Host+" instead, or check the address."))
}

// pandosOnEveryHostname reports whether a request is Pando's whatever its
// Host: the reserved prefix, a request re-entering from it, or Pando's own
// external hostname.
func (s *Server) pandosOnEveryHostname(r *http.Request, host string) bool {
	if reserved, _ := r.Context().Value(reservedKey{}).(bool); reserved || underPath(r.URL.Path, ReservedPrefix) {
		return true
	}
	return s.ExternalURL != nil && s.ExternalURL.Hostname() != "" && strings.EqualFold(host, s.ExternalURL.Hostname())
}

// underPath reports whether path is prefix or below it, segment-wise.
func underPath(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// sameOriginWrites refuses a state-changing request carried by the session
// cookie when the browser says it came from another origin.
//
// Only the cookie is ambient: a bearer token is sent by code that chose to
// send it, so a request carrying one is let through. A request with neither
// Sec-Fetch-Site nor Origin is not from a browser that would have attached
// the cookie on another page's behalf, and is let through too.
func (s *Server) sameOriginWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.crossOriginWrite(r) {
			Error(w, r, errs.New(errs.PermCrossOrigin,
				"Pando refused this request because it came from a page on another site, "+
					"and was sent with your Pando sign-in. Pando accepts a change made with "+
					"your sign-in only from Pando's own pages.").
				WithRemedy("Make the change in the Pando console, or send it from a script with an API token in the Authorization header."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) crossOriginWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if r.Header.Get("Authorization") != "" {
		return false
	}
	// Signing in is checked with or without a cookie: a page on another site
	// that posts its own username and password signs the visitor in as that
	// account (login CSRF), and nothing ambient is needed for it to work.
	if !hasSessionCookie(r) && !signsIn(r) {
		return false
	}
	// A SAML provider posts the response to the callback from its own site
	// (design 06 §3.2). The flow's state and bind cookie are its CSRF
	// defence, and SameSite=Lax keeps the session cookie off that post anyway.
	if strings.HasPrefix(r.URL.Path, "/api/v1/auth/providers/") && strings.HasSuffix(r.URL.Path, "/callback") {
		return false
	}

	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "same-origin", "none":
		return false
	case "":
		// Older browsers send Origin but not Sec-Fetch-Site.
	default:
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return true // "null", from a sandboxed frame or a privacy redirect
	}
	// The external URL as well as Host: a proxy in front of Pando may
	// rewrite Host to its upstream's name.
	if s.ExternalURL != nil && strings.EqualFold(u.Host, s.ExternalURL.Host) {
		return false
	}
	return !strings.EqualFold(u.Host, r.Host)
}
