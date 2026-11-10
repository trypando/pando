package cli_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/cli"
)

// visit is the browser following a redirect to the CLI's loopback address,
// without following where that sends it next.
func visit(t *testing.T, address string) visited {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(address) //nolint:noctx // a test's one request.
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return visited{status: resp.StatusCode, location: resp.Header.Get("Location")}
}

// visited is what the CLI's loopback address answered.
type visited struct {
	status   int
	location string
}

// approvalPage parses the address pando login opened.
func approvalPage(t *testing.T, api *fakeAPI, address string) url.Values {
	t.Helper()
	require.True(t, strings.HasPrefix(address, api.url+"/admin/cli?"), address)
	u, err := url.Parse(address)
	require.NoError(t, err)
	return u.Query()
}

func exchanged(t *testing.T, api *fakeAPI) (code, verifier string) {
	t.Helper()
	var body struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
	}
	require.NoError(t, json.Unmarshal([]byte(api.bodyFor("POST /cli-sign-ins/token")), &body))
	return body.Code, body.CodeVerifier
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// TestR406_LoginSignsInThroughTheBrowser asserts R-406 in the CLI: pando login
// opens the console's approval page with a PKCE challenge, takes the code the
// browser brings to its loopback address, and trades it with the verifier for
// a token it stores.
func TestR406_LoginSignsInThroughTheBrowser(t *testing.T) {
	api := newAPI(t).reply("POST /cli-sign-ins/token",
		map[string]any{"secret": "tok_from_browser", "username": "ada"})

	var page url.Values
	defer cli.StandInForBrowser(func(address string) error {
		page = approvalPage(t, api, address)
		require.Equal(t, "S256", page.Get("code_challenge_method"))
		require.Contains(t, page.Get("name"), "CLI")
		require.Empty(t, page.Get("for"), "a sign-in, not a token to print")
		loopback := "http://127.0.0.1:" + page.Get("port") + "/callback"

		// Another page probing the port, or a stale tab, is turned away and
		// the CLI keeps waiting.
		require.Equal(t, http.StatusNotFound,
			visit(t, loopback+"?code=stolen&state=not-this-one").status)
		require.Equal(t, http.StatusNotFound,
			visit(t, loopback+"?state="+url.QueryEscape(page.Get("state"))).status)

		resp := visit(t, loopback+"?code=the-code&state="+url.QueryEscape(page.Get("state")))
		require.Equal(t, http.StatusSeeOther, resp.status)
		require.Equal(t, api.url+"/admin/cli?signed_in=1", resp.location,
			"back to the console, which says what happened")
		return nil
	})()

	got := run(t, api, "", "login")
	require.NoError(t, got.err)
	require.Contains(t, got.errOut, "Opening Pando in your browser")

	code, verifier := exchanged(t, api)
	require.Equal(t, "the-code", code)
	require.Equal(t, page.Get("code_challenge"), s256(verifier), "the verifier behind the challenge")
	require.NotContains(t, page.Encode(), verifier, "the verifier never left the CLI")

	creds, err := cli.LoadCredentials()
	require.NoError(t, err)
	require.Equal(t, "tok_from_browser", creds.Token)
	require.Equal(t, "ada", creds.User)
	require.Contains(t, got.out, "Signed in to "+api.url)
}

func TestR406_LoginPastesTheCodeWithoutABrowser(t *testing.T) {
	api := newAPI(t).reply("POST /cli-sign-ins/token",
		map[string]any{"secret": "tok_pasted", "username": "ada"})
	defer cli.StandInForBrowser(func(string) error {
		t.Fatal("--no-browser opens no browser")
		return nil
	})()

	got := run(t, api, "pasted-code\n", "login", "--no-browser")
	require.NoError(t, got.err)

	start := strings.Index(got.errOut, api.url+"/admin/cli?")
	require.GreaterOrEqual(t, start, 0, got.errOut)
	page := approvalPage(t, api, strings.Fields(got.errOut[start:])[0])
	require.Empty(t, page.Get("port"), "nothing listens, so no port")

	code, verifier := exchanged(t, api)
	require.Equal(t, "pasted-code", code)
	require.Equal(t, page.Get("code_challenge"), s256(verifier))

	creds, err := cli.LoadCredentials()
	require.NoError(t, err)
	require.Equal(t, "tok_pasted", creds.Token)
}

func TestR406_LoginStoresNothingWhenTheBrowserCancels(t *testing.T) {
	api := newAPI(t)
	defer cli.StandInForBrowser(func(address string) error {
		page := approvalPage(t, api, address)
		resp := visit(t, "http://127.0.0.1:"+page.Get("port")+"/callback?error=access_denied&state="+
			url.QueryEscape(page.Get("state")))
		require.Equal(t, api.url+"/admin/cli?canceled=1", resp.location)
		return nil
	})()

	got := run(t, api, "", "login")
	require.ErrorContains(t, got.err, "canceled in the browser")
	require.False(t, api.sawPath("/cli-sign-ins/token"))
	_, err := cli.LoadCredentials()
	require.ErrorIs(t, err, cli.ErrNotLoggedIn)
}

func TestR406_LoginGivesUpWaitingForTheBrowser(t *testing.T) {
	defer cli.WaitForBrowser(20 * time.Millisecond)()
	defer cli.StandInForBrowser(func(string) error { return errors.New("no display") })()

	got := run(t, newAPI(t), "", "login")
	require.ErrorContains(t, got.err, "no approval came back")
	require.ErrorContains(t, got.err, "--no-browser")
	require.Contains(t, got.errOut, "Could not open a browser (no display)")
}

func TestR406_LoginStoresNothingWhenTheCodeIsRefused(t *testing.T) {
	api := newAPI(t).fail("POST /cli-sign-ins/token", http.StatusUnauthorized, map[string]string{
		"code": "AUTH_INVALID", "message": "This sign-in code is not valid.",
	})
	got := run(t, api, "stale\n", "login", "--no-browser")
	require.ErrorContains(t, got.err, "not valid")
	_, err := cli.LoadCredentials()
	require.ErrorIs(t, err, cli.ErrNotLoggedIn)

	got = run(t, newAPI(t), "\n", "login", "--no-browser")
	require.ErrorContains(t, got.err, "no code was pasted")

	got = run(t, newAPI(t), "", "login", "--no-browser", "--username", "ada")
	require.Error(t, got.err, "one way or the other")
}
