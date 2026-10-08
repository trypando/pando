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
