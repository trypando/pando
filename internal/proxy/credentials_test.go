package proxy_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/proxy"
)

// realishAuth authenticates the way httpapi.Authenticator does: an
// Authorization header first, and anything in it that is not a good Pando
// token is an error; then the session cookie; then anonymous.
type realishAuth struct {
	goodToken string // "Bearer " + this resolves to usr_bob's delegated token
}

func (a realishAuth) Authenticate(r *http.Request) (authz.Principal, error) {
	if header := r.Header.Get("Authorization"); header != "" {
		raw, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || raw != a.goodToken {
			return authz.Anonymous(), errors.New("not a Pando token")
		}
		bob := activeUser("usr_bob")
		bob.Kind, bob.TokenID = authz.KindToken, "tok_bob"
		return bob, nil
	}
	if c, err := r.Cookie("pando_session"); err == nil && c.Value == "ses_alice" {
		return activeUser("usr_alice"), nil
	}
	return authz.Anonymous(), nil
}

// An app's own credential, as a Supabase client sends it.
const appJWT = "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhcHAtdXNlciJ9.c2lnbmF0dXJl"

func credentialHarness(t *testing.T, configure func(*store)) (*httptest.Server, *received, string) {
	t.Helper()
	token := id.New(id.Token) + ".c2VjcmV0LXZhbHVlLW9mLXRoZS10b2tlbg"
	front, _, _, got := harnessWith(t, realishAuth{goodToken: token}, configure,
		func(url string) proxy.Upstreams { return fixedUpstream{addr: url} })
	return front, got, token
}

// answer is what a test reads of the proxy's response.
type answer struct {
	StatusCode int
	Location   string
}

func send(t *testing.T, front *httptest.Server, cookie bool, authorization ...string) answer {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, front.URL+"/api/items", nil)
	require.NoError(t, err)
	if cookie {
		req.AddCookie(&http.Cookie{Name: "pando_session", Value: "ses_alice"})
	}
	for _, v := range authorization {
		req.Header.Add("Authorization", v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return answer{StatusCode: resp.StatusCode, Location: resp.Header.Get("Location")}
}

// TestR173_AnAppsOwnLoginReachesItAndTheCookieDecides asserts R-173's other
// half: what is not Pando's is the app's (issue #93).
//
// An app with its own login sends its own Authorization header from the
// browser. The proxy read every one as a Pando token, failed, and treated the
// visitor as signed out despite a good session cookie — so a private app's
// API calls were answered with Pando's sign-in page.
func TestR173_AnAppsOwnLoginReachesItAndTheCookieDecides(t *testing.T) {
	front, got, _ := credentialHarness(t, func(s *store) { s.owner[appID] = "usr_alice" })

	for name, header := range map[string]string{
		"a bearer JWT": appJWT,
		"Basic":        "Basic YXBwLXVzZXI6aHVudGVyMg==",
	} {
		t.Run(name, func(t *testing.T) {
			resp := send(t, front, true, header)
			require.Equal(t, http.StatusOK, resp.StatusCode, "the visitor is signed in by their cookie")
			require.Equal(t, "usr_alice", got.header.Get(proxy.HeaderUser))
			require.Equal(t, []string{header}, got.header.Values("Authorization"),
				"the app's own credential reaches it untouched")
		})
	}
}

// TestR173_APandoTokenNeverReachesTheApp asserts R-173 for API tokens. An app
// that receives one can replay it against Pando's API as whoever sent it.
func TestR173_APandoTokenNeverReachesTheApp(t *testing.T) {
	front, got, token := credentialHarness(t, func(s *store) { s.owner[appID] = "usr_bob" })

	resp := send(t, front, false, "Bearer "+token)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "usr_bob", got.header.Get(proxy.HeaderUser), "the token authenticated")
	require.Empty(t, got.header.Values("Authorization"), "and was taken off before the app saw the request")

	// Beside the app's own: the Pando token decides and is removed; the app's
	// own credential still arrives.
	resp = send(t, front, false, "Bearer "+token, appJWT)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "usr_bob", got.header.Get(proxy.HeaderUser))
	require.Equal(t, []string{appJWT}, got.header.Values("Authorization"))
}

// A Pando-shaped token that does not authenticate is a failed Pando
// credential: anonymous, with no falling back to the session cookie, and
// still removed before the app sees it — a revoked token is still a secret.
func TestR173_AFailedPandoTokenIsAnonymousAndNeverReachesTheApp(t *testing.T) {
	front, got, token := credentialHarness(t, func(s *store) {
		s.owner[appID] = "usr_alice"
		s.anonymous[appID] = true // public, so the request is served and can be inspected
	})

	revoked := id.New(id.Token) + ".bm90LWEtcmVhbC1zZWNyZXQ"
	for name, header := range map[string]string{
		"unknown":          "Bearer " + revoked,
		"scheme lowercase": "bearer " + token,
	} {
		t.Run(name, func(t *testing.T) {
			resp := send(t, front, true, header)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "anonymous", got.header.Get(proxy.HeaderUser),
				"a failed Pando token does not fall back to the cookie")
			require.Empty(t, got.header.Values("Authorization"))
		})
	}
}

// A private app answered an app's own API call with Pando's sign-in redirect;
// with the cookie deciding, it is answered by the app.
func TestR173_APrivateAppsOwnAPICallIsNotSentToSignIn(t *testing.T) {
	front, got, _ := credentialHarness(t, func(s *store) { s.data[appID] = []string{"usr_alice"} })

	resp := send(t, front, true, appJWT)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, resp.Location)
	require.Equal(t, "/api/items", got.path)
}
