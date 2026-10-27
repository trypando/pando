package cli_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR415_PandoJoinPostsTheTokenToSetup asserts R-415's CLI: `pando join`
// posts the token and name to the new Pando's POST /setup/join, which needs
// no sign-in.
func TestR415_PandoJoinPostsTheTokenToSetup(t *testing.T) {
	api := newAPI(t).reply("POST /setup/join", map[string]any{
		"roles": []string{"app"}, "host_id": "host_01", "name": "edge-2", "status": "joined",
	})
	got := run(t, api, "", "join", "pj1.abc", "--name", "edge-2")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"token":"pj1.abc","name":"edge-2"}`, api.bodyFor("POST /setup/join"))
	require.Contains(t, got.out, "edge-2 (host_01), roles app")
}

func TestPandoJoinSaysWhyTheJoinWasRefused(t *testing.T) {
	api := newAPI(t).fail("POST /setup/join", http.StatusConflict, map[string]any{
		"code": "STATE_INVALID", "message": "This Pando already has accounts or apps, so it cannot join a cluster.",
	})
	got := run(t, api, "", "join", "pj1.abc")
	require.ErrorContains(t, got.err, "already has accounts")
}
