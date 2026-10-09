package proxy

import (
	"net/http"
	"strings"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/id"
)

// Which Authorization header is Pando's (issue #93).
//
// On Pando's API every Authorization header is Pando's, and one it cannot
// read is refused. In front of an app it is not: an app with its own login
// sends its own credential from the browser — a Supabase or Firebase JWT, a
// Basic header — and the proxy used to read that as a Pando token, fail, and
// treat the visitor as signed out despite a good session cookie. A private
// app then answered its own API calls with Pando's sign-in page.
//
// So in front of an app, Pando claims only what has the shape of its own
// token, `Bearer tok_<ULID>.<secret>`, and leaves everything else to the app:
// the Authenticator never sees it, the session cookie decides who this is,
// and the header is forwarded untouched. A Pando-shaped token that does not
// authenticate is a failed Pando credential, anonymous as before, and does not
// fall back to the cookie.
//
// And a Pando token never reaches the app, valid or not, for the reason the
// session cookie does not (R-173): an app holding one can replay it against
// Pando's API as whoever sent it. The assertion is what an app is given.

// isPandoToken reports whether an Authorization value has the shape of a
// Pando API token. The scheme is matched without regard to case, as RFC 9110
// has it, so `bearer tok_…` is still Pando's to strip.
func isPandoToken(value string) bool {
	scheme, credential, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	tokenID, _, ok := strings.Cut(strings.TrimSpace(credential), ".")
	return ok && id.Is(id.Token, tokenID)
}

// authenticate resolves a request in front of an app: the Authenticator sees
// a Pando token if the request carries one, and no Authorization header
// otherwise.
func (p *Proxy) authenticate(r *http.Request) (authz.Principal, error) {
	// A session cookie alone, from the cache when it can be (issue #93).
	if principal, ok, err := p.Cache.principal(r, p.Authenticator); ok {
		return principal, err
	}
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return p.Authenticator.Authenticate(r)
	}

	view := r.Clone(r.Context())
	view.Header.Del("Authorization")
	for _, v := range values {
		if isPandoToken(v) {
			view.Header.Set("Authorization", v)
			break
		}
	}
	return p.Authenticator.Authenticate(view)
}

// stripPandoTokens removes every Pando-shaped Authorization value and keeps
// the rest, in order, for the app.
func stripPandoTokens(h http.Header) {
	values := h.Values("Authorization")
	if len(values) == 0 {
		return
	}
	h.Del("Authorization")
	for _, v := range values {
		if !isPandoToken(v) {
			h.Add("Authorization", v)
		}
	}
}
