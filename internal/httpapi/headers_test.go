package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/httpapi"
)

// TestR173_PandosOwnPagesCannotBeFramedByAnApp asserts R-173's rule that an
// app's page may not use a visitor's Pando sign-in, for framing.
//
// An app on a sibling hostname, or on Pando's own origin under path routing,
// could put the console in an invisible frame and have a signed-in
// administrator click through it. Pando's own responses forbid framing and
// sniffing and run only its own scripts; an app's responses are the app's and
// carry none of it.
func TestR173_PandosOwnPagesCannotBeFramedByAnApp(t *testing.T) {
	appProxy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-From", "app")
		_, _ = w.Write([]byte("the app's page"))
	})
	console := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<!doctype html>"))
	})
	external, err := url.Parse("https://pando.example.com")
	require.NoError(t, err)
	handler := (&httpapi.Server{
		Logger: zap.NewNop(), DB: fakeDB{}, ExternalURL: external,
		Console: console, AppProxy: appProxy, AppHosts: appHosts{host: "notes.example.com"},
	}).Routes()

	for _, path := range []string{"/", "/healthz", "/.pando/login", "/api/v1/me"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "pando.example.com"
		handler.ServeHTTP(rec, req)
		h := rec.Header()
		require.Equal(t, "DENY", h.Get("X-Frame-Options"), path)
		require.Contains(t, h.Get("Content-Security-Policy"), "frame-ancestors 'none'", path)
		require.Contains(t, h.Get("Content-Security-Policy"), "script-src 'self'", path)
		require.Equal(t, "nosniff", h.Get("X-Content-Type-Options"), path)
		require.Equal(t, "strict-origin-when-cross-origin", h.Get("Referrer-Policy"), path)
		require.Equal(t, "max-age=31536000", h.Get("Strict-Transport-Security"), "%s: the external URL is https", path)
	}

	// An app's own response is untouched, on its hostname and on Pando's.
	for _, host := range []string{"notes.example.com", "pando.example.com"} {
		req := httptest.NewRequest(http.MethodGet, "/some/page/of/the/app", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, "app", rec.Header().Get("X-From"), host)
		for _, key := range []string{"X-Frame-Options", "Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "Strict-Transport-Security"} {
			require.Empty(t, rec.Header().Get(key), "%s on %s is the app's to decide", key, host)
		}
	}
}

// TestR173_NoStrictTransportSecurityOverPlainHTTP asserts that HSTS is sent
// only when the operator has said the installation is reached over HTTPS: on
// the plain-HTTP localhost install it would make the browser refuse Pando.
func TestR173_NoStrictTransportSecurityOverPlainHTTP(t *testing.T) {
	handler := (&httpapi.Server{Logger: zap.NewNop(), DB: fakeDB{}}).Routes()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Empty(t, rec.Header().Get("Strict-Transport-Security"))
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
}
