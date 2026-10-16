package httpapi

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR173_AnAppCannotPlantASessionCookieOnAnHTTPSInstall asserts R-173's
// rule that an app cannot set Pando's session for a visitor.
//
// An app on a sibling subdomain can set a cookie for the parent domain, and a
// browser sends it to Pando's hostname too. Named pando_session, it would fix
// the visitor's session or sign them in as the app owner's account. On HTTPS
// the session cookie is __Host-pando_session, which a browser accepts only from
// the exact host, Secure, for "/", with no Domain — and the plain name is not
// read at all.
func TestR173_AnAppCannotPlantASessionCookieOnAnHTTPSInstall(t *testing.T) {
	external, err := url.Parse("https://pando.example.com")
	require.NoError(t, err)
	secure := CookieScheme{ExternalURL: external}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	require.Equal(t, SecureSessionCookie, secure.Name(r))
	set := secure.sessionCookie(r, "ses_real")
	require.True(t, strings.HasPrefix(set.Name, "__Host-"))
	require.True(t, set.Secure, "__Host- requires Secure")
	require.Equal(t, "/", set.Path, "__Host- requires Path=/")
	require.Empty(t, set.Domain, "__Host- forbids Domain")

	planted := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	planted.AddCookie(&http.Cookie{Name: SessionCookie, Value: "ses_planted"})
	got, err := secure.SessionID(planted)
	require.NoError(t, err)
	require.Empty(t, got, "a plain pando_session on an HTTPS install is not one Pando set")

	both := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	both.AddCookie(&http.Cookie{Name: SessionCookie, Value: "ses_planted"})
	both.AddCookie(&http.Cookie{Name: SecureSessionCookie, Value: "ses_real"})
	got, err = secure.SessionID(both)
	require.NoError(t, err)
	require.Equal(t, "ses_real", got)

	// Plain HTTP cannot use the prefix, and keeps the name it always had.
	plain := CookieScheme{}
	require.Equal(t, SessionCookie, plain.Name(r))
	tlsReq := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	require.Equal(t, SecureSessionCookie, plain.Name(tlsReq), "Pando serving TLS itself is HTTPS too")
}

// TestR173_TwoSessionCookiesAreRefused asserts that when a browser sends two
// different session cookies by the name Pando reads — one set by Pando, one by
// another site on the same host or domain — Pando does not choose between them.
func TestR173_TwoSessionCookiesAreRefused(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "ses_planted"})
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "ses_real"})

	_, err := CookieScheme{}.SessionID(r)
	require.Error(t, err)
	require.Contains(t, err.Error(), "two different sign-in cookies")

	var nilAuth *Authenticator
	_, err = nilAuth.Authenticate(r)
	require.Error(t, err, "the Authenticator refuses it too, so no handler sees either session")

	// The same value twice is one session, not two.
	same := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	same.AddCookie(&http.Cookie{Name: SessionCookie, Value: "ses_real"})
	same.AddCookie(&http.Cookie{Name: SessionCookie, Value: "ses_real"})
	got, err := CookieScheme{}.SessionID(same)
	require.NoError(t, err)
	require.Equal(t, "ses_real", got)
}

// TestR173_ASignInPostedFromAnotherSiteIsRefused asserts R-173 for login CSRF.
//
// A page on another site that posts its own username and password to Pando
// signs its visitor in as that account — no cookie is needed for that, so the
// origin check cannot wait for one. A form body is refused as well, for a
// browser that sends neither Origin nor Sec-Fetch-Site.
func TestR173_ASignInPostedFromAnotherSiteIsRefused(t *testing.T) {
	passed := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { passed = true })
	h := (&Server{}).sameOriginWrites(next)

	for _, path := range []string{"/api/v1/sessions", "/api/v1/setup"} {
		passed = false
		req := httptest.NewRequest(http.MethodPost, "http://pando.example.com"+path, nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.False(t, passed, "%s from another site, without a cookie", path)
		require.Equal(t, http.StatusForbidden, rec.Code)

		passed = false
		req = httptest.NewRequest(http.MethodPost, "http://pando.example.com"+path, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		h.ServeHTTP(httptest.NewRecorder(), req)
		require.True(t, passed, "%s from Pando's own page", path)

		passed = false
		req = httptest.NewRequest(http.MethodPost, "http://pando.example.com"+path, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
		require.True(t, passed, "%s from the CLI, which is not a browser", path)
	}

	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"text/plain;charset=UTF-8",
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"username":"a","password":"b"}`))
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		(&Server{}).handleLogin(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, ct)
		require.Contains(t, rec.Body.String(), "Content-Type: application/json")

		rec = httptest.NewRecorder()
		(&Server{}).handlePostSetup(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, ct)
	}
}
