package cli_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/cli"
)

// TestR424_PandoInstructionsReadsAndSetsWhatAgentsAreTold asserts R-424 from
// the CLI: `show` prints what agents get, `--company` only the company's
// part, and `set` sends stdin as the company's part.
func TestR424_PandoInstructionsReadsAndSetsWhatAgentsAreTold(t *testing.T) {
	doc := map[string]any{"text": "# Building apps on Pando\n\nUse Go.", "builtin": "# Building apps on Pando", "company": "Use Go."}

	api := newAPI(t).reply("GET /instructions", doc)
	r := run(t, api, "", "instructions", "show")
	require.NoError(t, r.err)
	require.Equal(t, "# Building apps on Pando\n\nUse Go.\n", r.out)

	r = run(t, api, "", "instructions", "show", "--company")
	require.NoError(t, r.err)
	require.Equal(t, "Use Go.\n", r.out)

	api = newAPI(t).reply("PUT /instructions", map[string]any{"company": "Use Go."})
	r = run(t, api, "Use Go.\n", "instructions", "set")
	require.NoError(t, r.err)
	require.JSONEq(t, `{"company":"Use Go.\n"}`, api.bodyFor("PUT /instructions"))
	require.Contains(t, r.out, "saved")

	api = newAPI(t).reply("PUT /instructions", map[string]any{"company": ""})
	r = run(t, api, "", "instructions", "set")
	require.NoError(t, r.err)
	require.Contains(t, r.out, "cleared")

	api = newAPI(t).fail("PUT /instructions", http.StatusForbidden,
		map[string]any{"code": "AUTHZ_DENIED", "message": "You need install.policy.manage."})
	r = run(t, api, "x", "instructions", "set")
	require.ErrorContains(t, r.err, "install.policy.manage")
}

// TestR424_PandoMCPSendsTheInstructionsWhenAnAgentConnects asserts R-424 over
// stdio: initialize carries what GET /instructions says, and a server without
// them still answers.
func TestR424_PandoMCPSendsTheInstructionsWhenAnAgentConnects(t *testing.T) {
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	instructionsOf := func(out string) (string, bool) {
		var reply struct {
			Result map[string]any `json:"result"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &reply), out)
		s, ok := reply.Result["instructions"].(string)
		return s, ok
	}

	api := newAPI(t).reply("GET /instructions", map[string]any{"text": "Store data in Postgres."})
	t.Setenv(cli.EnvServer, api.url)
	t.Setenv(cli.EnvToken, "tok_test")
	got := run(t, api, initialize, "mcp")
	require.NoError(t, got.err, got.errOut)
	text, ok := instructionsOf(got.out)
	require.True(t, ok, got.out)
	require.Equal(t, "Store data in Postgres.", text)

	old := newAPI(t).fail("GET /instructions", http.StatusNotFound, map[string]any{"code": "NOT_FOUND", "message": "No such route."})
	t.Setenv(cli.EnvServer, old.url)
	got = run(t, old, initialize, "mcp")
	require.NoError(t, got.err, got.errOut)
	_, ok = instructionsOf(got.out)
	require.False(t, ok, "an older server: no instructions, and the connection still works")
}
