package cli_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/cli"
)

// TestR415_PandoJoinPostsTheTokenToSetup asserts R-415's CLI: `pando join`
// posts the token, name and setup token to the new Pando's POST /setup/join,
// which needs no sign-in.
func TestR415_PandoJoinPostsTheTokenToSetup(t *testing.T) {
	api := newAPI(t).reply("POST /setup/join", map[string]any{
		"roles": []string{"app"}, "host_id": "host_01", "name": "edge-2", "status": "joined",
	})
	got := run(t, api, "", "join", "pj1.abc", "--name", "edge-2", "--setup-token", "s3tup")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"setup_token":"s3tup","token":"pj1.abc","name":"edge-2"}`, api.bodyFor("POST /setup/join"))
	require.Contains(t, got.out, "edge-2 (host_01), roles app")
	require.Contains(t, got.errOut, "can be read by other users", "secrets as arguments are warned about")
}

// TestR415_PandoJoinNeedsTheSetupToken asserts R-046 on `pando join`: the
// setup token is asked for before anything is sent.
func TestR415_PandoJoinNeedsTheSetupToken(t *testing.T) {
	t.Setenv(cli.EnvSetupToken, "")
	api := newAPI(t)
	got := run(t, api, "", "join", "pj1.abc")
	require.ErrorContains(t, got.err, "no setup token was given")
	require.Empty(t, api.calls)
}

// TestR194_TheJoinCommandNeedsNoSecretOnTheCommandLine asserts R-194 on
// `pando join`: both tokens are read from standard input or from the
// environment, so neither has to be an argument other users can read, and
// neither is warned about when it is not one.
func TestR194_TheJoinCommandNeedsNoSecretOnTheCommandLine(t *testing.T) {
	t.Setenv(cli.EnvJoinToken, "")
	t.Setenv(cli.EnvSetupToken, "")
	reply := map[string]any{"roles": []string{"app"}, "host_id": "host_01", "name": "edge-2", "status": "joined"}

	api := newAPI(t).reply("POST /setup/join", reply)
	got := run(t, api, "pj1.piped\ns3tup-piped\n", "join", "--name", "edge-2")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"setup_token":"s3tup-piped","token":"pj1.piped","name":"edge-2"}`, api.bodyFor("POST /setup/join"))
	require.NotContains(t, got.errOut, "can be read by other users")

	t.Setenv(cli.EnvJoinToken, "pj1.env")
	t.Setenv(cli.EnvSetupToken, "s3tup-env")
	api = newAPI(t).reply("POST /setup/join", reply)
	got = run(t, api, "", "join")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"setup_token":"s3tup-env","token":"pj1.env","name":""}`, api.bodyFor("POST /setup/join"))
	require.NotContains(t, got.errOut, "can be read by other users")

	t.Setenv(cli.EnvJoinToken, "")
	api = newAPI(t)
	got = run(t, api, "", "join")
	require.ErrorContains(t, got.err, "no join token was given")
	require.Empty(t, api.calls)
}

func TestPandoJoinSaysWhyTheJoinWasRefused(t *testing.T) {
	api := newAPI(t).fail("POST /setup/join", http.StatusConflict, map[string]any{
		"code": "STATE_INVALID", "message": "This Pando already has accounts or apps, so it cannot join a cluster.",
	})
	got := run(t, api, "", "join", "pj1.abc", "--setup-token", "s3tup")
	require.ErrorContains(t, got.err, "already has accounts")
}
