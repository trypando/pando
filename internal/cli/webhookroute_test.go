package cli_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR432_WebhookRoutesAreManagedFromTheCLI asserts a route is added with the
// verification the API takes, and a generated secret is printed.
func TestR432_WebhookRoutesAreManagedFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("POST /apps/app_1/webhook-routes", map[string]any{"revision": 4, "secret": "pdwr_shown_once"})

	got := run(t, api, "", "app", "webhook-route", "add", "app_1", "noodle", "/api/v1/webhook/noodle",
		"--header", "X-Hub-Signature-256", "--prefix", "sha256=").out
	require.Contains(t, got, "pdwr_shown_once")
	require.Contains(t, got, "revision 4")
	require.JSONEq(t, `{"name":"noodle","method":"POST","path":"/api/v1/webhook/noodle","allow_repeats":false,"secret":"",
		"verification":{"kind":"hmac_sha256","header":"X-Hub-Signature-256","prefix":"sha256="}}`,
		api.bodyFor("POST /apps/app_1/webhook-routes"))
}

// TestR432_ASecretIsReplacedFromTheCLI asserts both ways of replacing one.
func TestR432_ASecretIsReplacedFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("POST /apps/app_1/webhook-routes/noodle/secret", map[string]any{"secret": "pdwr_new"})
	require.Contains(t, run(t, api, "", "app", "webhook-route", "secret", "app_1", "noodle").out, "pdwr_new")

	api = newAPI(t).reply("POST /apps/app_1/webhook-routes/noodle/secret", map[string]any{})
	got := run(t, api, "", "app", "webhook-route", "secret", "app_1", "noodle", "--secret", "whsec_from_the_provider")
	require.Contains(t, got.out, "Saved.")
	require.JSONEq(t, `{"secret":"whsec_from_the_provider"}`,
		api.bodyFor("POST /apps/app_1/webhook-routes/noodle/secret"))
}

func TestR432_RoutesAreListedAndRemovedFromTheCLI(t *testing.T) {
	api := newAPI(t).
		reply("GET /apps/app_1/webhook-routes", map[string]any{"routes": []any{map[string]any{"name": "noodle"}}}).
		reply("DELETE /apps/app_1/webhook-routes/noodle", map[string]any{"revision": 5})
	require.Contains(t, run(t, api, "", "app", "webhook-route", "list", "app_1").out, "noodle")
	require.Contains(t, run(t, api, "", "app", "webhook-route", "remove", "app_1", "noodle").out, "Removed.")
}
