package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR173_ACookieWriteFromAnotherOriginIsRefused asserts R-173 for the
// browser's half of it.
//
// Under subdomain routing an app at notes.example.com and Pando at
// pando.example.com are one site, so the SameSite=Lax session cookie goes
// with a request from the app's page to Pando's API. The page cannot read the
// answer, but the write has already happened (issue #78).
func TestR173_ACookieWriteFromAnotherOriginIsRefused(t *testing.T) {
	passed := false
	external, _ := url.Parse("https://pando.example.com")
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { passed = true })
	h := (&Server{ExternalURL: external}).sameOriginWrites(next)

	type request struct {
		method, path string
		cookie       bool
		header       map[string]string
	}
	send := func(r request) *httptest.ResponseRecorder {
		passed = false
		req := httptest.NewRequest(r.method, "http://pando.example.com"+r.path, nil)
		if r.cookie {
			req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "sess_visitor"})
		}
		for k, v := range r.header {
			req.Header.Set(k, v)
		}
		if host := req.Header.Get("X-Test-Host"); host != "" {
			req.Host = host
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for name, r := range map[string]request{
		"same site, another origin": {http.MethodPost, "/api/v1/tokens", true,
			map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://notes.example.com"}},
		"another site": {http.MethodDelete, "/api/v1/apps/app_01", true,
			map[string]string{"Sec-Fetch-Site": "cross-site"}},
		"origin only, another host": {http.MethodPut, "/api/v1/policy", true,
			map[string]string{"Origin": "https://notes.example.com"}},
		"an opaque origin": {http.MethodPatch, "/api/v1/users/usr_01", true,
			map[string]string{"Origin": "null"}},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			rec := send(r)
			require.False(t, passed)
			require.Equal(t, http.StatusForbidden, rec.Code)
			require.Contains(t, rec.Body.String(), "PERM_CROSS_ORIGIN")
		})
	}

	for name, r := range map[string]request{
		"the console, same origin": {http.MethodPost, "/api/v1/tokens", true,
			map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://pando.example.com"}},
		"origin only, this host": {http.MethodPost, "/api/v1/tokens", true,
			map[string]string{"Origin": "http://pando.example.com"}},
		"origin only, the external URL behind a proxy that rewrote Host": {http.MethodPost, "/api/v1/tokens", true,
			map[string]string{"Origin": "https://pando.example.com", "X-Test-Host": "pando:8080"}},
		"a read": {http.MethodGet, "/api/v1/apps", true,
			map[string]string{"Sec-Fetch-Site": "same-site"}},
		"a bearer token, which is not ambient": {http.MethodPost, "/api/v1/tokens", true,
			map[string]string{"Sec-Fetch-Site": "cross-site", "Authorization": "Bearer tok_x"}},
		"no session cookie": {http.MethodPost, "/api/v1/sessions", false,
			map[string]string{"Sec-Fetch-Site": "cross-site"}},
		"not a browser": {http.MethodPost, "/api/v1/tokens", true, nil},
		"a SAML provider posting to the callback": {http.MethodPost, "/api/v1/auth/providers/idp_01/callback", true,
			map[string]string{"Sec-Fetch-Site": "cross-site"}},
	} {
		t.Run("let through: "+name, func(t *testing.T) {
			send(r)
			require.True(t, passed)
		})
	}
}

// The sign-in routes are matched exactly: a path that only starts like one is
// not one, and a method the sign-in page does not use is not let through.
func TestR172_SignInRoutesAreMatchedExactly(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/login", true},
		{http.MethodHead, "/login", true},
		{http.MethodGet, "/assets/index-abc.js", true},
		{http.MethodPost, "/api/v1/sessions", true},
		{http.MethodGet, "/api/v1/auth/providers/idp_01/start", true},
		{http.MethodGet, "/api/v1/apps/app_01/passcode", true},

		{http.MethodGet, "/api/v1/sessions", false},
		{http.MethodGet, "/api/v1/auth/providers//start", false},
		{http.MethodGet, "/api/v1/apps/app_01/passcode/extra", false},
		{http.MethodGet, "/api/v1/me/apps", false},
		{http.MethodPost, "/api/v1/me/favorites/app_01", false},
		{http.MethodPost, "/login", false},
		{http.MethodGet, "/admin", false},
	} {
		require.Equal(t, tc.want, isSignInRoute(tc.method, tc.path), "%s %s", tc.method, tc.path)
	}
}
