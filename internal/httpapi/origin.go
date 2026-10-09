package httpapi

import (
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
//     /.pando answers signing in and nothing else (signInRoutes). R-173 strips
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
	{http.MethodPost, "/api/v1/sessions"},
	{http.MethodDelete, "/api/v1/sessions"},
	{http.MethodGet, "/api/v1/me"},
	// R-046: a first-run password must be changed before anything else, on
	// whichever hostname the person signed in at. It needs the current one.
	{http.MethodPost, "/api/v1/me/password"},
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
	if s.AppHosts == nil {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	isApp, err := s.AppHosts.IsAppHostname(r.Context(), host)
	return err != nil || isApp
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
	if c, err := r.Cookie(SessionCookie); err != nil || c.Value == "" {
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
