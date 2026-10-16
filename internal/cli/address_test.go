package cli_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_AnAppsAddressFromTheCLI asserts R-261 for an app's address: the
// console's Change address is a command, a flag left out is left out of the
// request, a domain can be named, and the API's confirm is the CLI's
// --confirm.
func TestR261_AnAppsAddressFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("GET /apps/notes/routing", map[string]any{
		"address": "https://notes.bemeek.io",
		"options": []map[string]any{{"adapter_ref": "rte_cf", "domains": []string{"bemeek.io", "trypando.ai"}}},
	})
	got := run(t, api, "", "app", "address", "notes")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "trypando.ai")

	api = newAPI(t).reply("PUT /apps/notes/routing", map[string]any{
		"changed": true, "address": "https://docs.trypando.ai", "revision": 4,
	})
	got = run(t, api, "", "app", "address", "notes", "--hostname", "docs", "--domain", "trypando.ai", "--confirm")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"hostname":"docs","domain":"trypando.ai","confirm":true}`, api.bodyFor("PUT /apps/notes/routing"))
	require.Contains(t, got.out, "revision 4")
	require.Contains(t, got.out, "docs.trypando.ai")

	api = newAPI(t).reply("PUT /apps/notes/routing", map[string]any{"changed": false, "address": "https://notes.bemeek.io"})
	got = run(t, api, "", "app", "address", "notes", "--domain", "bemeek.io")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Nothing changed")

	api = newAPI(t).fail("PUT /apps/notes/routing", http.StatusBadRequest, map[string]any{
		"code":    "VALID_INVALID",
		"message": "Changing this app's address moves it from https://notes.bemeek.io to https://notes.trypando.ai.",
		"remedy":  "Send confirm: true to go ahead.",
		"details": map[string]any{"from": "https://notes.bemeek.io", "to": "https://notes.trypando.ai"},
	})
	got = run(t, api, "", "app", "address", "notes", "--mode", "subdomain", "--domain", "trypando.ai")
	require.ErrorContains(t, got.err, "Add --confirm to go ahead")
	require.NotContains(t, got.err.Error(), "confirm: true")
	require.JSONEq(t, `{"mode":"subdomain","domain":"trypando.ai","confirm":false}`, api.bodyFor("PUT /apps/notes/routing"))

	// Any other refusal is passed on as it is.
	api = newAPI(t).fail("PUT /apps/notes/routing", http.StatusBadRequest, map[string]any{
		"code": "VALID_INVALID", "message": "The rte_cf routing adapter doesn't serve the domain example.org.",
	})
	got = run(t, api, "", "app", "address", "notes", "--domain", "example.org", "--path", "/x", "--adapter", "rte_cf")
	require.ErrorContains(t, got.err, "doesn't serve the domain example.org")
}
