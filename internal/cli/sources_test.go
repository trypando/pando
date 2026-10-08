package cli_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR091_SourceConnectionsAreListedWithWhatEachNeeds asserts `pando source
// list`: a ready connection, one waiting to be authorized with the command
// that does it, and one that cannot be built with why.
func TestR091_SourceConnectionsAreListedWithWhatEachNeeds(t *testing.T) {
	api := newAPI(t).reply("GET /sources", map[string]any{"sources": []map[string]any{
		{"id": "src_github", "kind": "github", "capabilities": map[string]any{
			"method": "oauth", "host": "github.com", "scope": "acme", "authorized": true}},
		{"id": "src_gitlab", "kind": "gitlab", "capabilities": map[string]any{
			"method": "oauth", "host": "gitlab.com"}},
		{"id": "src_broken", "kind": "git", "problem": "A git host token connection needs a token.",
			"capabilities": map[string]any{}},
	}})

	got := run(t, api, "", "source", "list")
	require.NoError(t, got.err, got.errOut)
	require.Regexp(t, `src_github\s+github\s+github.com/acme\s+oauth\s+ready`, got.out)
	require.Regexp(t, `src_gitlab\s+gitlab\s+gitlab.com\s+oauth\s+not authorized: pando source authorize src_gitlab`, got.out)
	require.Regexp(t, `src_broken\s+git\s+-\s+-\s+A git host token connection needs a token.`, got.out)

	empty := newAPI(t).reply("GET /sources", map[string]any{"sources": []any{}})
	got = run(t, empty, "", "source", "list")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "No source connections")
	require.Contains(t, got.out, "pando adapter add source/<kind>")
}

// TestR091_ARepositoryAndItsBranchesArePickedThroughAConnection asserts
// `pando source repos` and `pando source branches`.
func TestR091_ARepositoryAndItsBranchesArePickedThroughAConnection(t *testing.T) {
	api := newAPI(t).
		reply("GET /sources/src_github/repositories", map[string]any{"repositories": []map[string]any{
			{"url": "https://github.com/acme/api.git", "full_name": "acme/api", "default_branch": "main", "private": true},
			{"url": "https://github.com/acme/site.git", "full_name": "acme/site"},
		}}).
		reply("GET /sources/src_github/branches", map[string]any{"branches": []string{"main", "release"}})

	got := run(t, api, "", "source", "repos", "src_github", "--query", "api", "--limit", "5")
	require.NoError(t, got.err, got.errOut)
	require.True(t, api.sawPath("/sources/src_github/repositories?limit=5&q=api"))
	require.Regexp(t, `acme/api\s+main\s+private\s+https://github.com/acme/api.git`, got.out)
	require.Regexp(t, `acme/site\s+-\s+public`, got.out)

	got = run(t, api, "", "source", "repos", "src_github")
	require.NoError(t, got.err, got.errOut)
	require.True(t, api.sawPath("/sources/src_github/repositories"))

	got = run(t, api, "", "source", "branches", "src_github", "https://github.com/acme/api.git")
	require.NoError(t, got.err, got.errOut)
	require.Equal(t, "main\nrelease\n", got.out)
	require.True(t, api.sawPath("/sources/src_github/branches?url=https%3A%2F%2Fgithub.com%2Facme%2Fapi.git"))

	// The server's refusal is what the person sees.
	refused := newAPI(t).fail("GET /sources/src_git/repositories", http.StatusBadRequest, map[string]any{
		"code": "VALID_INVALID", "message": "The source connection \"src_git\" cannot list repositories."})
	require.ErrorContains(t, run(t, refused, "", "source", "repos", "src_git").err, "cannot list repositories")
	refused.fail("GET /sources/src_git/branches", http.StatusBadRequest, map[string]any{
		"code": "VALID_INVALID", "message": "The source connection \"src_git\" cannot list branches."})
	require.ErrorContains(t, run(t, refused, "", "source", "branches", "src_git", "x").err, "cannot list branches")
}

// TestR091_AConnectionIsAuthorizedFromTheCLI asserts `pando source authorize`:
// a device code shown and waited on until approved, an address printed with
// --web, and a code not approved in time said so.
func TestR091_AConnectionIsAuthorizedFromTheCLI(t *testing.T) {
	polls := 0
	api := newAPI(t).
		reply("POST /sources/src_github/authorize", map[string]any{
			"mode": "device", "user_code": "ABCD-1234", "verification_url": "https://github.com/login/device",
			"interval_seconds": 1}).
		handle("POST /sources/src_github/authorize/poll", func() any {
			polls++
			return map[string]any{"status": "authorized"}
		})

	got := run(t, api, "", "source", "authorize", "src_github")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Go to https://github.com/login/device and enter the code ABCD-1234")
	require.Contains(t, got.out, "Authorized. src_github is ready")
	require.JSONEq(t, `{"mode":"device"}`, api.bodyFor("POST /sources/src_github/authorize"))
	require.Equal(t, 1, polls)

	web := newAPI(t).reply("POST /sources/src_github/authorize", map[string]any{
		"mode": "web", "authorize_url": "https://github.com/login/oauth/authorize?state=x"})
	got = run(t, web, "", "source", "authorize", "src_github", "--web")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "https://github.com/login/oauth/authorize?state=x")
	require.JSONEq(t, `{"mode":"web"}`, web.bodyFor("POST /sources/src_github/authorize"))

	// No time to wait: the code is shown and the command says to run it again.
	got = run(t, api, "", "source", "authorize", "src_github", "--timeout", "0s")
	require.ErrorContains(t, got.err, "was not approved within 0s")
	require.ErrorContains(t, got.err, "pando source authorize src_github")

	refused := newAPI(t).fail("POST /sources/src_git/authorize", http.StatusBadRequest, map[string]any{
		"code": "VALID_INVALID", "message": "The source connection \"src_git\" cannot be authorized with a device code."})
	require.ErrorContains(t, run(t, refused, "", "source", "authorize", "src_git").err, "cannot be authorized")
}

// TestR091_ASlowDownIsHonoredAndAPollErrorEndsTheWait asserts the device
// wait's two other answers: the provider asking for slower polling, and the
// server refusing the poll.
func TestR091_ASlowDownIsHonoredAndAPollErrorEndsTheWait(t *testing.T) {
	polls := 0
	api := newAPI(t).
		reply("POST /sources/src_github/authorize", map[string]any{"user_code": "C", "verification_url": "https://v", "interval_seconds": 1}).
		handle("POST /sources/src_github/authorize/poll", func() any {
			polls++
			return map[string]any{"status": "pending", "slow_down": true}
		})
	got := run(t, api, "", "source", "authorize", "src_github", "--timeout", "500ms")
	require.ErrorContains(t, got.err, "was not approved within 500ms")
	require.Equal(t, 1, polls)

	failing := newAPI(t).
		reply("POST /sources/src_github/authorize", map[string]any{"user_code": "C", "verification_url": "https://v", "interval_seconds": 1}).
		fail("POST /sources/src_github/authorize/poll", http.StatusConflict, map[string]any{
			"code": "STATE_INVALID", "message": "The authorization of \"src_github\" expired before it was finished."})
	require.ErrorContains(t, run(t, failing, "", "source", "authorize", "src_github").err, "expired")
}

// TestR091_AConnectionIsRemovedFromTheCLI asserts `pando source remove`.
func TestR091_AConnectionIsRemovedFromTheCLI(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "", "source", "remove", "src_github")
	require.NoError(t, got.err, got.errOut)
	require.Equal(t, "Removed src_github.\n", got.out)
	require.True(t, api.sawPath("/sources/src_github"))
}

var gitSourceKind = map[string]any{"kinds": []map[string]any{{
	"category": "source", "kind": "git", "name": "Git host", "id_prefix": "src_",
	"fields": []map[string]any{
		{"key": "method", "label": "How Pando signs in", "type": "string"},
		{"key": "host", "label": "Host", "type": "string"},
		{"key": "token", "label": "Token", "type": "string", "credential": true},
	},
}}}

// TestR091_ASourceConnectionIsAddedAsNoOnesDefault asserts that adding a
// source connection never marks it the default — the one covering a
// repository is chosen for it — and that it says it is used at once, with no
// restart.
func TestR091_ASourceConnectionIsAddedAsNoOnesDefault(t *testing.T) {
	api := newAPI(t).reply("GET /adapters/kinds", gitSourceKind)
	got := run(t, api, "ghp_secret\n", "adapter", "add", "source/git",
		"--set", "method=token", "--set", "host=git.example.com")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{
		"id": "src_git", "category": "source", "kind": "git", "name": "Git host",
		"config": {"method": "token", "host": "git.example.com"},
		"credentials": {"token": "ghp_secret"},
		"is_default": false
	}`, api.bodyFor("POST /adapters"))
	require.Contains(t, got.out, "It is used from now on")
	require.NotContains(t, got.out, "Restart")
}

// TestR091_AnAppIsAddedThroughANamedConnection asserts `pando app add
// --connection`, which names the connection a private repository is read
// with.
func TestR091_AnAppIsAddedThroughANamedConnection(t *testing.T) {
	api := newAPI(t).reply("POST /apps", map[string]any{"id": "app_01"})
	got := run(t, api, "", "app", "add", "https://github.com/acme/api.git", "--connection", "src_github")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"name":"api","source":{"type":"git","url":"https://github.com/acme/api.git","connection":"src_github"}}`,
		api.bodyFor("POST /apps"))
}
