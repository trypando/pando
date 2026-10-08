package cli_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_AutoDeployIsSetFromTheCLI asserts that turning auto-deploy on and
// choosing its trigger from the CLI is the same call the console makes.
func TestR261_AutoDeployIsSetFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("PUT /apps/app_1/auto-deploy", map[string]any{"changed": true, "revision": 4})

	got := run(t, api, "", "app", "auto-deploy", "set", "app_1", "--trigger", "release", "--tag-pattern", "release-*")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"enabled":true,"trigger":"release_tagged","branch":"","tag_pattern":"release-*"}`,
		api.bodyFor("PUT /apps/app_1/auto-deploy"))
	require.Contains(t, got.out, "revision 4")

	got = run(t, api, "", "app", "auto-deploy", "set", "app_1", "--trigger", "nightly")
	require.Error(t, got.err)

}

// Turning it off keeps what it followed, so turning it on again needs no
// remembering.
func TestAutoDeployIsTurnedOffFromTheCLIKeepingItsTrigger(t *testing.T) {
	api := newAPI(t).
		reply("GET /apps/app_1/auto-deploy", map[string]any{"settings": map[string]any{
			"enabled": true, "trigger": "branch_updated", "branch": "staging",
		}}).
		reply("PUT /apps/app_1/auto-deploy", map[string]any{"changed": true, "revision": 5})

	got := run(t, api, "", "app", "auto-deploy", "off", "app_1")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"enabled":false,"trigger":"branch_updated","branch":"staging"}`,
		api.bodyFor("PUT /apps/app_1/auto-deploy"))
}

func TestAutoDeployIsShownFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_1/auto-deploy", map[string]any{
		"settings": map[string]any{"enabled": true, "trigger": "branch_updated"}, "webhook_secret_set": false,
	})
	got := run(t, api, "", "app", "auto-deploy", "show", "app_1")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, `"trigger": "branch_updated"`)
}

// A save that changes nothing says so, rather than naming a revision that
// was not written.
func TestAnAutoDeploySaveThatChangesNothingSaysSo(t *testing.T) {
	api := newAPI(t).reply("PUT /apps/app_1/auto-deploy", map[string]any{"changed": false, "revision": 3})
	got := run(t, api, "", "app", "auto-deploy", "set", "app_1")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Nothing to change")
}

// The server's refusal reaches the person, whichever subcommand asked.
func TestAutoDeployCommandsReportTheServersRefusal(t *testing.T) {
	refusal := map[string]any{"code": "VALID_INVALID", "message": "This app isn't configured yet, so it has no deploy settings to change."}
	api := newAPI(t).
		fail("GET /apps/app_1/auto-deploy", 404, map[string]any{"code": "NOT_FOUND", "message": "There is no app with that ID."}).
		fail("PUT /apps/app_1/auto-deploy", 400, refusal).
		fail("POST /apps/app_1/auto-deploy/webhook-secret", 403, map[string]any{"code": "PERM_DENIED", "message": "You may not."}).
		fail("DELETE /apps/app_1/auto-deploy/webhook-secret", 403, map[string]any{"code": "PERM_DENIED", "message": "You may not."})

	for _, args := range [][]string{
		{"show", "app_1"},
		{"set", "app_1"},
		{"off", "app_1"},
		{"webhook-secret", "app_1"},
		{"webhook-secret", "app_1", "--remove"},
	} {
		got := run(t, api, "", "app", append([]string{"auto-deploy"}, args...)...)
		require.Error(t, got.err, "%v", args)
	}
}

func TestTheAutoDeployWebhookSecretIsPrintedOnce(t *testing.T) {
	api := newAPI(t).reply("POST /apps/app_1/auto-deploy/webhook-secret", map[string]any{
		"webhook_url": "https://pando.example/api/v1/apps/app_1/auto-deploy/webhook", "webhook_secret": "pdwh_abc",
	})

	got := run(t, api, "", "app", "auto-deploy", "webhook-secret", "app_1")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "pdwh_abc")
	require.Contains(t, got.out, "https://pando.example/api/v1/apps/app_1/auto-deploy/webhook")

	got = run(t, api, "", "app", "auto-deploy", "webhook-secret", "app_1", "--remove")
	require.NoError(t, got.err, got.errOut)
	require.True(t, api.sawPath("/apps/app_1/auto-deploy/webhook-secret"))
	require.Contains(t, got.out, "Removed")
}
