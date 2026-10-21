package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_SpecRevisionsHaveCommands asserts R-261: listing, reading, writing,
// pinning and comparing spec revisions are endpoints, so the CLI reaches each.
func TestR261_SpecRevisionsHaveCommands(t *testing.T) {
	api := newAPI(t).
		reply("GET /apps/app_1/specs", map[string]any{"revisions": []any{}, "pinned_spec_id": "spec_1"}).
		reply("GET /apps/app_1/specs/3", map[string]any{"revision": 3}).
		reply("GET /apps/app_1/specs/3/diff/4", map[string]any{"class": "restart"})

	got := run(t, api, "", "app", "spec", "list", "app_1")
	require.NoError(t, got.err)
	require.Contains(t, got.out, `"pinned_spec_id": "spec_1"`)

	got = run(t, api, "", "app", "spec", "show", "app_1", "3")
	require.NoError(t, got.err)
	require.Contains(t, got.out, `"revision": 3`)

	got = run(t, api, "", "app", "spec", "diff", "app_1", "3", "4")
	require.NoError(t, got.err)
	require.Contains(t, got.out, `"class": "restart"`)

	got = run(t, api, "", "app", "spec", "pin", "app_1", "4")
	require.NoError(t, got.err)
	require.JSONEq(t, `{}`, api.bodyFor("POST /apps/app_1/specs/4/pin"))
	require.Contains(t, got.out, "Pinned revision 4. Nothing is deployed yet.")
	require.Contains(t, got.out, "`pando deploy app_1`")
}

// A spec is written from stdin or a file, and --dry-run asks the server to
// check it without writing it.
func TestR261_SpecCreateWritesARevision(t *testing.T) {
	api := newAPI(t).reply("POST /apps/app_1/specs", map[string]any{"revision": 5})

	got := run(t, api, `{"egress":{"allow":["ntfy.sh"]}}`, "app", "spec", "create", "app_1")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"egress":{"allow":["ntfy.sh"]}}`, api.bodyFor("POST /apps/app_1/specs"))
	require.Contains(t, got.out, "Wrote revision 5. It is not pinned yet.")
	require.Contains(t, got.out, "`pando app spec pin app_1 5`")

	file := filepath.Join(t.TempDir(), "spec.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"name":"hn"}`), 0o600))
	api = newAPI(t).reply("POST /apps/app_1/specs", map[string]any{"would_need_approval": false})
	got = run(t, api, "", "app", "spec", "create", "app_1", "-f", file, "--dry-run")
	require.NoError(t, got.err)
	require.True(t, api.sawPath("/apps/app_1/specs?dry_run=true"))
	require.JSONEq(t, `{"name":"hn"}`, api.bodyFor("POST /apps/app_1/specs"))
	require.Contains(t, got.out, `"would_need_approval": false`)
}

func TestSpecCreateRefusesWhatIsNotJSON(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "not json", "app", "spec", "create", "app_1")
	require.ErrorContains(t, got.err, "that is not valid JSON")
	require.Empty(t, api.calls, "nothing reaches the API")

	got = run(t, api, "", "app", "spec", "create", "app_1", "-f", filepath.Join(t.TempDir(), "missing.json"))
	require.Error(t, got.err)
	require.Empty(t, api.calls)
}

// The server's refusal of a spec is shown as it wrote it.
func TestSpecCommandsReportARefusal(t *testing.T) {
	refusal := map[string]string{"code": "NOT_FOUND", "message": "There is no such revision of this app's spec."}
	api := newAPI(t).
		fail("POST /apps/app_1/specs", 400, map[string]string{"code": "VALID_INVALID", "message": "The spec could not be read."}).
		fail("POST /apps/app_1/specs/9/pin", 404, refusal).
		fail("GET /apps/app_1/specs/9", 404, refusal).
		fail("GET /apps/app_1/specs", 404, refusal).
		fail("GET /apps/app_1/specs/1/diff/9", 404, refusal)

	require.ErrorContains(t, run(t, api, `{}`, "app", "spec", "create", "app_1").err, "The spec could not be read.")
	require.ErrorContains(t, run(t, api, `{}`, "app", "spec", "create", "app_1", "--dry-run").err, "The spec could not be read.")
	require.ErrorContains(t, run(t, api, "", "app", "spec", "pin", "app_1", "9").err, "no such revision")
	require.ErrorContains(t, run(t, api, "", "app", "spec", "show", "app_1", "9").err, "no such revision")
	require.ErrorContains(t, run(t, api, "", "app", "spec", "list", "app_1").err, "no such revision")
	require.ErrorContains(t, run(t, api, "", "app", "spec", "diff", "app_1", "1", "9").err, "no such revision")
}

// TestR261_SecretDeleteHasACommand asserts R-261: removing a secret is an
// endpoint, so the CLI reaches it.
func TestR261_SecretDeleteHasACommand(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "", "secret", "delete", "app_1", "NTFY_TOPIC")
	require.NoError(t, got.err)
	require.True(t, api.sawPath("/apps/app_1/secrets/NTFY_TOPIC"))
	require.Equal(t, "DELETE", api.calls[0].method)
	require.Contains(t, got.out, "Removed NTFY_TOPIC.")

	api = newAPI(t).fail("DELETE /apps/app_1/secrets/NOPE", 404, map[string]string{"code": "NOT_FOUND", "message": "This app has no secret NOPE."})
	require.ErrorContains(t, run(t, api, "", "secret", "delete", "app_1", "NOPE").err, "no secret NOPE")
}
