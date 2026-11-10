package cli_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/cli"
)

func issued(id, kind, name, secret string) map[string]any {
	return map[string]any{
		"token":  map[string]any{"id": id, "kind": kind, "name": name},
		"secret": secret,
	}
}

// TestR058_TokenCreateMintsADelegatedTokenInTheBrowser asserts R-058 in the
// CLI (issue #396): pando token create has the browser approve a delegated
// token by name, prints its secret alone on standard output, and stores
// nothing — the CLI's own token stays what it was.
func TestR058_TokenCreateMintsADelegatedTokenInTheBrowser(t *testing.T) {
	api := newAPI(t).reply("POST /cli-sign-ins/token", issued("tok_laptop", "delegated", "laptop agent", "s3cret"))

	defer cli.StandInForBrowser(func(address string) error {
		page := approvalPage(t, api, address)
		require.Equal(t, "laptop agent", page.Get("name"))
		require.Empty(t, page.Get("kind"), "delegated is the default")
		resp := visit(t, "http://127.0.0.1:"+page.Get("port")+"/callback?code=c&state="+url.QueryEscape(page.Get("state")))
		require.Equal(t, api.url+"/admin/cli?created=1", resp.location, "the console says a token was made")
		return nil
	})()

	got := run(t, api, "", "token", "create", "laptop agent")
	require.NoError(t, got.err)
	require.Equal(t, "s3cret\n", got.out, "the secret alone, for $(…)")
	require.Contains(t, got.errOut, `Token "laptop agent" created: tok_laptop.`)
	require.NotContains(t, got.errOut, "grant add", "a delegated token is not shared with")

	_, err := cli.LoadCredentials()
	require.Error(t, err, "nothing stored")
}

// TestR060_TokenCreateMintsAServiceToken asserts R-060 in the CLI: --service
// asks the browser to approve a service token, and says how to share an app
// with it.
func TestR060_TokenCreateMintsAServiceToken(t *testing.T) {
	api := newAPI(t).reply("POST /cli-sign-ins/token", issued("tok_agent", "account", "nightly agent", "s3cret"))
	defer cli.StandInForBrowser(func(string) error {
		t.Fatal("--no-browser opens no browser")
		return nil
	})()

	got := run(t, api, "pasted\n", "token", "create", "nightly agent", "--service", "--no-browser")
	require.NoError(t, got.err)

	start := strings.Index(got.errOut, api.url+"/admin/cli?")
	require.GreaterOrEqual(t, start, 0, got.errOut)
	page := approvalPage(t, api, strings.Fields(got.errOut[start:])[0])
	require.Equal(t, "service", page.Get("kind"))
	require.Equal(t, "nightly agent", page.Get("name"))

	code, verifier := exchanged(t, api)
	require.Equal(t, "pasted", code)
	require.Equal(t, page.Get("code_challenge"), s256(verifier))

	require.Equal(t, "s3cret\n", got.out)
	require.Contains(t, got.errOut, `Service token "nightly agent" created: tok_agent.`)
	require.Contains(t, got.errOut, "pando grant add <app> --token tok_agent")
}

func TestR060_TokenCreateWithAPasswordUsesTheSessionOnce(t *testing.T) {
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"ci"}, "/tokens"},
		{[]string{"ci", "--service"}, "/tokens/service"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			api := newAPI(t).
				reply("POST /sessions", map[string]any{"user_id": "usr_ada"}).
				reply("POST "+tc.path, issued("tok_ci", "account", "ci", "s3cret"))

			got := run(t, api, "hunter2\n", "token", append(append([]string{"create"}, tc.args...), "--username", "ada")...)
			require.NoError(t, got.err)
			require.JSONEq(t, `{"name":"ci"}`, api.bodyFor("POST "+tc.path))
			require.Equal(t, "s3cret\n", got.out)

			last := api.calls[len(api.calls)-1]
			require.Equal(t, http.MethodDelete, last.method, "the session ends")
			require.Equal(t, "/api/v1/sessions", last.path)
		})
	}
}

func TestTokenCreateEndsTheSessionWhenMintingIsRefused(t *testing.T) {
	api := newAPI(t).
		reply("POST /sessions", map[string]any{"user_id": "usr_ada"}).
		fail("POST /tokens/service", http.StatusForbidden,
			map[string]any{"code": "PERM_DENIED", "message": "You need install.tokens.manage."})

	got := run(t, api, "hunter2\n", "token", "create", "ci", "--service", "--username", "ada")
	require.ErrorContains(t, got.err, "install.tokens.manage")
	require.Empty(t, got.out)
	require.Equal(t, http.MethodDelete, api.calls[len(api.calls)-1].method)
}

func TestTokenCreateStopsWhenTheBrowserCancels(t *testing.T) {
	api := newAPI(t)
	defer cli.StandInForBrowser(func(address string) error {
		page := approvalPage(t, api, address)
		visit(t, "http://127.0.0.1:"+page.Get("port")+"/callback?error=access_denied&state="+url.QueryEscape(page.Get("state")))
		return nil
	})()

	got := run(t, api, "", "token", "create", "ci")
	require.ErrorContains(t, got.err, "Run pando token create again")
	require.False(t, api.sawPath("/cli-sign-ins/token"))
}

func TestTokenCreateNeedsAName(t *testing.T) {
	got := run(t, newAPI(t), "", "token", "create", "  ")
	require.ErrorContains(t, got.err, "needs a name")
}

func TestR060_TokenListShowsServiceTokens(t *testing.T) {
	api := newAPI(t).reply("GET /tokens/service", map[string]any{"tokens": []map[string]any{
		{"id": "tok_agent", "name": "nightly agent"},
		{"id": "tok_old", "name": "old agent", "revoked_at": "2026-10-01T00:00:00Z"},
	}})
	got := run(t, api, "", "token", "list", "--service")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "nightly agent  active   tok_agent")
	require.Contains(t, got.out, "old agent      revoked  tok_old")
}

// TestR060_GrantAddSharesAnAppWithAServiceToken asserts R-060's "appears in
// ACLs" in the CLI: an app is shared with a service token by its ID.
func TestR060_GrantAddSharesAnAppWithAServiceToken(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "", "grant", "add", "app_1", "--token", "tok_agent")
	require.NoError(t, got.err)

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(api.bodyFor("POST /apps/app_1/grants")), &body))
	require.Equal(t, map[string]any{"plane": "data", "principal_kind": "token", "principal_id": "tok_agent"}, body)

	got = run(t, api, "", "grant", "add", "app_1", "--token", "tok_agent", "--user", "usr_ada")
	require.ErrorContains(t, got.err, "--token=<id>")
}
