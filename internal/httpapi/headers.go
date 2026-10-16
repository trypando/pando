package httpapi

import (
	"io"
	"net/http"
)

// contentSecurityPolicy is what Pando's own pages may load and who may frame
// them.
//
// frame-ancestors 'none' is the point: an app on a sibling hostname, or on
// Pando's own origin under path routing, could otherwise put the console in an
// invisible frame and have a signed-in administrator click through it. Scripts
// only from Pando's own origin — the console build has no inline script and
// no eval — so an injected <script> or attribute handler does not run. Styles
// are left alone: the console sets them through the DOM, which a style-src
// rule would not block anyway, and a wrong one would blank the page.
const contentSecurityPolicy = "script-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'"

// hstsMaxAge is a year, without includeSubDomains: an app on a subdomain may
// be served by something else entirely, and Pando does not speak for it.
const hstsMaxAge = "max-age=31536000"

// securityHeaders marks Pando's own responses — the console, the API and the
// sign-in page — as not to be framed, sniffed or followed by a full referrer.
//
// Never a response from an app: the proxy copies the app's headers into the
// same writer, and an app's framing and script policy are its own (R-023 puts
// Pando in front of an app, not in charge of its pages). So the headers are
// added when the response is written, by which time a request handed to the
// proxy has said so (toApp).
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&headerWriter{ResponseWriter: w, r: r, hsts: s.ExternalURL != nil && s.ExternalURL.Scheme == "https"}, r)
	})
}

// headerWriter adds the security headers to a response of Pando's own the
// first time it is written.
type headerWriter struct {
	http.ResponseWriter
	r    *http.Request
	hsts bool
	done bool
}

func (h *headerWriter) apply() {
	if h.done {
		return
	}
	h.done = true
	if t, ok := h.r.Context().Value(appTrafficKey{}).(*appTraffic); ok && t.handed {
		return
	}
	hdr := h.Header()
	setIfUnset(hdr, "Content-Security-Policy", contentSecurityPolicy)
	setIfUnset(hdr, "X-Frame-Options", "DENY")
	setIfUnset(hdr, "X-Content-Type-Options", "nosniff")
	setIfUnset(hdr, "Referrer-Policy", "strict-origin-when-cross-origin")
	if h.hsts {
		setIfUnset(hdr, "Strict-Transport-Security", hstsMaxAge)
	}
}

func setIfUnset(h http.Header, key, value string) {
	if h.Get(key) == "" {
		h.Set(key, value)
	}
}

func (h *headerWriter) WriteHeader(code int) {
	h.apply()
	h.ResponseWriter.WriteHeader(code)
}

func (h *headerWriter) Write(b []byte) (int, error) {
	h.apply()
	// An io.Writer, for the reason statusRecorder.Write gives.
	var out io.Writer = h.ResponseWriter
	return out.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and Hijack underneath, which
// the log stream and exec websockets need.
func (h *headerWriter) Unwrap() http.ResponseWriter { return h.ResponseWriter }
