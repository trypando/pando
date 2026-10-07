package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/httpapi"
)

// marker is a handler that identifies itself in the body, so a test can say
// which of several handlers answered.
func marker(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Handled-By", name)
		_, _ = w.Write([]byte(name + " " + r.URL.Path))
	})
}

// hosts answers whether a hostname belongs to an app.
type hosts struct {
	app map[string]bool
	err error
}

func (h hosts) IsAppHostname(_ context.Context, host string) (bool, error) {
	return h.app[host], h.err
}

type routed struct {
	*httpapi.Server
	handler http.Handler
}

func newRouted(t *testing.T, configure func(*httpapi.Server)) routed {
	t.Helper()
	s := &httpapi.Server{Logger: zap.NewNop(), DB: fakeDB{}}
	if configure != nil {
		configure(s)
	}
	return routed{Server: s, handler: s.Routes()}
}

func (r routed) get(host, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

// R-023: everything that is not one of Pando's own routes is a request to an
// app, and goes through the proxy. Mounting it as the fallback is what makes
// "there is no bypass" structural.
func TestR023_EverythingUnrecognizedGoesThroughTheProxy(t *testing.T) {
	r := newRouted(t, func(s *httpapi.Server) { s.AppProxy = marker("proxy") })

	for _, path := range []string{
		"/notes", "/notes/items/1", "/anything/at/all",
		"/api/v2/apps", "/api", "/wp-admin",
	} {
		got := r.get("", path)
		require.Equal(t, "proxy", got.Header().Get("X-Handled-By"), path)
	}
}

// The console owns "/" and a handful of other paths, which is right at Pando's
// own hostname and wrong the moment an app has one of its own.
func TestTheConsoleOwnsOnlyItsOwnPaths(t *testing.T) {
	r := newRouted(t, func(s *httpapi.Server) {
		s.Console = marker("console")
		s.AppProxy = marker("proxy")
	})

	for _, path := range []string{"/", "/index.html", "/login", "/admin", "/admin/users", "/assets/app.js"} {
		require.Equal(t, "console", r.get("", path).Header().Get("X-Handled-By"), path)
	}

	// Everything else is an app's, including paths that look consoleish.
	for _, path := range []string{"/admins", "/logins", "/asset/app.js", "/dashboard"} {
		require.Equal(t, "proxy", r.get("", path).Header().Get("X-Handled-By"), path)
	}
}

// A request to https://notes.example.com/ matches the console's "/" route and
// would never reach the proxy — so the app's owner gets Pando's console where
// their app should be, and in an install using subdomain addressing every app
// is unreachable at its root.
func TestAnAppsOwnHostnameReachesTheAppAndNotTheConsole(t *testing.T) {
	r := newRouted(t, func(s *httpapi.Server) {
		s.Console = marker("console")
		s.AppProxy = marker("proxy")
		s.AppHosts = hosts{app: map[string]bool{"notes.example.com": true}}
	})

	require.Equal(t, "proxy", r.get("notes.example.com", "/").Header().Get("X-Handled-By"))
	require.Equal(t, "proxy", r.get("notes.example.com:8443", "/").Header().Get("X-Handled-By"),
		"a port on the Host header does not change whose hostname it is")

	// Pando's own hostname still gets the console: it does own "/" there, and
	// in path mode that is the only hostname there is.
	require.Equal(t, "console", r.get("pando.example.com", "/").Header().Get("X-Handled-By"))
}

// A failure here falls back to the console, which is the safe direction:
// someone sees Pando instead of their app, rather than reaching an app without
// passing through the proxy.
func TestAFailedHostnameLookupFallsBackToTheConsole(t *testing.T) {
	r := newRouted(t, func(s *httpapi.Server) {
		s.Console = marker("console")
		s.AppProxy = marker("proxy")
		s.AppHosts = hosts{err: errors.New("the apps table is unreachable")}
	})

	require.Equal(t, "console", r.get("notes.example.com", "/").Header().Get("X-Handled-By"))
}

// Nil AppHosts means the console answers on every hostname, which is correct
// for a path-addressed install.
func TestWithNoHostnameTableTheConsoleAnswersEverywhere(t *testing.T) {
	r := newRouted(t, func(s *httpapi.Server) {
		s.Console = marker("console")
		s.AppProxy = marker("proxy")
	})
	require.Equal(t, "console", r.get("notes.example.com", "/").Header().Get("X-Handled-By"))
}

// R-172: an unauthenticated visit to a subdomain app used to redirect to
// "/login" on the app's own hostname, where consoleOrApp handed it back to the
// proxy, which redirected again — an infinite loop with the query string
// growing on every hop.
func TestR172_TheReservedPrefixIsPandosOnEveryHostname(t *testing.T) {
	r := newRouted(t, func(s *httpapi.Server) {
		s.Console = marker("console")
		s.AppProxy = marker("proxy")
		s.AppHosts = hosts{app: map[string]bool{"notes.example.com": true}}
	})

	// On the app's own hostname, where every other path is the app's.
	for _, path := range []string{httpapi.LoginPath, httpapi.ReservedPrefix + "/", httpapi.ReservedPrefix + "/assets/app.js"} {
		got := r.get("notes.example.com", path)
		require.Equal(t, "console", got.Header().Get("X-Handled-By"), path)
	}

	// The prefix is stripped before the console sees it, so the console serves
	// its own paths rather than ones it does not know.
	require.Equal(t, "console /login", r.get("notes.example.com", httpapi.LoginPath).Body.String())
}

func TestTheBareReservedPrefixRedirectsToItsRoot(t *testing.T) {
	r := newRouted(t, func(s *httpapi.Server) { s.Console = marker("console") })

	got := r.get("", httpapi.ReservedPrefix)
	require.Equal(t, http.StatusMovedPermanently, got.Code)
	require.Equal(t, httpapi.ReservedPrefix+"/", got.Header().Get("Location"))
}

// A slug cannot contain a dot, so ".pando" is a path no app can ever claim.
func TestTheReservedPrefixCannotCollideWithAnAppSlug(t *testing.T) {
	require.True(t, strings.HasPrefix(httpapi.ReservedPrefix, "/."))
	require.True(t, strings.HasPrefix(httpapi.LoginPath, httpapi.ReservedPrefix+"/"))
}

// The reserved mount re-enters the whole router, because a sign-in page needs
// somewhere to post to. Nothing is exposed that the front door does not already
// expose to the same caller.
func TestTheReservedPrefixReachesPandosOwnAPI(t *testing.T) {
	r := newRouted(t, func(s *httpapi.Server) {
		s.Console = marker("console")
		s.AppProxy = marker("proxy")
		s.AppHosts = hosts{app: map[string]bool{"notes.example.com": true}}
	})

	got := r.get("notes.example.com", httpapi.ReservedPrefix+"/healthz")
	require.Equal(t, http.StatusOK, got.Code)
	require.JSONEq(t, `{"status":"ok"}`, got.Body.String())
}

// A port-mode app has a socket of its own whose every path belongs to that app,
// so the router — and with it the sign-in page — is not in front of it.
func TestReservedOrAppSplitsAListenerThatIsNotTheFrontDoor(t *testing.T) {
	h := httpapi.ReservedOrApp(marker("pando"), marker("app"))

	serve := func(path string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Header().Get("X-Handled-By")
	}

	require.Equal(t, "pando", serve(httpapi.ReservedPrefix))
	require.Equal(t, "pando", serve(httpapi.LoginPath))
	require.Equal(t, "app", serve("/"))
	require.Equal(t, "app", serve("/items/1"))

	// A path that merely starts with the same letters is the app's.
	require.Equal(t, "app", serve("/.pandora"))
}

func TestReservedOrAppWithNoPandoHandlerIsJustTheApp(t *testing.T) {
	h := httpapi.ReservedOrApp(nil, marker("app"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, httpapi.LoginPath, nil))
	require.Equal(t, "app", rec.Header().Get("X-Handled-By"))
}

// Built without a console, those paths 404 like any other and the API is
// unaffected: the API is the product (R-261), and the console is one of its
// clients.
func TestABinaryWithNoConsoleStillServesTheAPI(t *testing.T) {
	r := newRouted(t, nil)

	require.Equal(t, http.StatusOK, r.get("", "/healthz").Code)
	require.Equal(t, http.StatusNotFound, r.get("", "/").Code,
		"with no console and no proxy, there is nothing at the root")
}

// R-057: the verification keys are public, and an app must be able to fetch
// them before it has any credential of its own.
func TestR057_TheJWKSEndpointIsUnauthenticatedAndCacheable(t *testing.T) {
	minter, err := assertion.NewMinter("https://pando.example", clock.System{})
	require.NoError(t, err)

	r := newRouted(t, func(s *httpapi.Server) { s.Minter = minter })

	got := r.get("", "/.well-known/jwks.json")
	require.Equal(t, http.StatusOK, got.Code)
	// A minute, shorter than an assertion lives (assertion.Lifetime): a cache
	// in front of an app must not keep serving a list that lacks a replica
	// that has just started signing (issue #72).
	require.Contains(t, got.Header().Get("Cache-Control"), "max-age=60")
	require.Contains(t, got.Body.String(), `"keys"`)
	require.NotContains(t, got.Body.String(), `"d"`, "a private exponent must never be published")
}

func TestJWKSSaysSoWhenSigningIsNotSetUp(t *testing.T) {
	r := newRouted(t, nil)

	got := r.get("", "/.well-known/jwks.json")
	require.Equal(t, http.StatusInternalServerError, got.Code)
	require.Contains(t, got.Body.String(), "INTERNAL")
}

// A panic in one handler must not take the process with it.
func TestAPanicBecomesA500RatherThanACrash(t *testing.T) {
	r := newRouted(t, func(s *httpapi.Server) {
		s.AppProxy = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("the runtime adapter is nil")
		})
	})

	require.NotPanics(t, func() {
		require.Equal(t, http.StatusInternalServerError, r.get("", "/notes").Code)
	})
}

// Every response carries the request ID, so a user reporting a failure hands
// over one string that finds the log line.
func TestTheRequestIDIsOnEveryResponseIncludingTheProxysAndThe404(t *testing.T) {
	r := newRouted(t, func(s *httpapi.Server) { s.AppProxy = marker("proxy") })

	for _, path := range []string{"/healthz", "/readyz", "/notes"} {
		require.Regexp(t, `^req_[0-9A-HJKMNP-TV-Z]{26}$`,
			r.get("", path).Header().Get(httpapi.RequestIDHeader), path)
	}

	bare := newRouted(t, nil)
	require.NotEmpty(t, bare.get("", "/nothing-here").Header().Get(httpapi.RequestIDHeader))
}

// The recorder has to keep Flush and Hijack reachable: the proxy depends on
// both (R-170), and hiding them would break streaming in a way that only shows
// up at runtime.
func TestR170_TheLoggingRecorderDoesNotHideFlush(t *testing.T) {
	var flushed bool
	r := newRouted(t, func(s *httpapi.Server) {
		s.AppProxy = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			require.NoError(t, http.NewResponseController(w).Flush())
			flushed = true
		})
	})

	r.get("", "/notes")
	require.True(t, flushed, "a streaming response could not flush through the middleware")
}
