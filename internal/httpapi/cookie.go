package httpapi

import (
	"net/http"
	"net/url"

	"github.com/trypando/pando/internal/errs"
)

// SessionCookie carries ses_<id>.<secret> (design 02 §2.7), on an install
// reached over plain HTTP.
const SessionCookie = "pando_session"

// SecureSessionCookie is the session cookie on an install reached over HTTPS.
//
// The __Host- prefix is a promise the browser keeps: a cookie by this name was
// set Secure, by this exact host, for path "/", with no Domain. So an app on a
// sibling subdomain cannot plant one for Pando's hostname to fix a visitor's
// session or sign them in as someone else — which a Domain cookie named
// pando_session could. Plain HTTP cannot use the prefix (it requires Secure),
// and keeps the plain name.
const SecureSessionCookie = "__Host-" + SessionCookie

// CookieScheme decides how the session cookie is set and which one is read.
//
// One place, so the name a cookie is set under and the name it is read back by
// cannot disagree. The zero value trusts the request, which is right when Pando
// serves TLS itself and on a localhost install.
type CookieScheme struct {
	// ExternalURL is how a browser reaches this installation (secureCookie).
	ExternalURL *url.URL
}

// Secure reports whether the session cookie is marked Secure (O-19).
//
// The request alone cannot answer this. Behind a TLS-terminating reverse proxy
// — the topology SECURITY.md describes — r.TLS is nil on every request even
// though the browser's connection is encrypted, so trusting it alone ships the
// session cookie without Secure and one plaintext request to the hostname puts
// it on the wire.
//
// So the operator states the scheme, in PANDO_SERVER_EXTERNAL_URL, and Pando
// believes the operator rather than the request. Not X-Forwarded-Proto: any
// client that can reach Pando directly can set it, which is the same reason
// R-053 strips inbound X-Pando-* headers unconditionally.
//
// Unset falls back to the request, which is correct in both topologies where
// the request does tell the truth — Pando terminating its own TLS, and the
// plain-HTTP localhost install the README documents, where an unconditional
// Secure would stop sign-in working.
func (c CookieScheme) Secure(r *http.Request) bool {
	if c.ExternalURL != nil {
		return c.ExternalURL.Scheme == "https"
	}
	return r.TLS != nil
}

// Name is the session cookie's name for this request: the __Host- one when it
// is Secure.
func (c CookieScheme) Name(r *http.Request) string {
	if c.Secure(r) {
		return SecureSessionCookie
	}
	return SessionCookie
}

// SessionID reads the session cookie's whole value — ID and secret, a
// credential — by the name this install sets it under,
// and only that name: on an HTTPS install a plain pando_session is one that
// Pando did not set, whoever did.
//
// More than one is refused rather than choosing between them. A browser sends
// every cookie that matches the request, so two means someone else set one —
// a sibling subdomain or another port on the same host — and whichever Pando
// picked might be theirs.
func (c CookieScheme) SessionID(r *http.Request) (string, error) {
	name := c.Name(r)
	found := ""
	for _, ck := range r.Cookies() {
		if ck.Name != name || ck.Value == "" {
			continue
		}
		if found != "" && found != ck.Value {
			return "", errs.New(errs.AuthInvalid,
				"This browser sent Pando two different sign-in cookies, so Pando could not tell which one it set. "+
					"Another site on the same domain or host may have set one.").
				WithRemedy("Sign out, clear this site's cookies in the browser, and sign in again.")
		}
		found = ck.Value
	}
	return found, nil
}

// hasSessionCookie reports whether the request carries a session cookie by
// either name, for the checks that ask whether the browser attached ambient
// credentials at all (crossOriginWrite).
func hasSessionCookie(r *http.Request) bool {
	for _, ck := range r.Cookies() {
		if (ck.Name == SessionCookie || ck.Name == SecureSessionCookie) && ck.Value != "" {
			return true
		}
	}
	return false
}

// sessionCookie is the cookie that carries a session, set or cleared with the
// same attributes every time: a browser treats a cookie set with different
// ones as a different cookie, and clearing would leave the original in place.
func (c CookieScheme) sessionCookie(r *http.Request, value string) *http.Cookie {
	//nolint:gosec // G124: Secure is decided by Secure, not left unset.
	return &http.Cookie{
		Name:     c.Name(r),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   c.Secure(r),
		SameSite: http.SameSiteLaxMode,
	}
}
