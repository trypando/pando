package cli_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// R-261, R-367: subscriptions are made and inspected from a terminal as from
// the console and an agent, and a webhook's key is printed the one time it is
// given (R-371).
func TestR367_SubscriptionsCreateSendsTheFilterAndPrintsTheKeyOnce(t *testing.T) {
	api := newAPI(t).reply("POST /subscriptions", map[string]any{
		"id": "sub_01", "app_id": "app_01HQ8", "app_name": "notes", "events": []string{"deploy.*"},
		"destination": "webhook", "url": "https://example.com/hook", "enabled": true,
		"signing_key": "whsec_abc",
	})
	got := run(t, api, "", "subscriptions", "create", "--app", "app_01HQ8",
		"--events", "deploy.*,app.state_changed", "--url", "https://example.com/hook")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"app_id":"app_01HQ8","events":["deploy.*","app.state_changed"],"destination":"webhook",
		"url":"https://example.com/hook","description":""}`, api.bodyFor("POST /subscriptions"))
	require.Contains(t, got.out, "sub_01")
	require.Contains(t, got.out, "whsec_abc")
	require.Contains(t, got.out, "shown once")

	got = run(t, newAPI(t), "", "subscriptions", "create", "--events", "*")
	require.ErrorContains(t, got.err, "exactly one of --url")
}

func TestR369_DeliveriesAndRedeliverCallTheirEndpoints(t *testing.T) {
	api := newAPI(t).reply("GET /subscriptions/sub_01/deliveries", map[string]any{
		"deliveries": []map[string]any{{
			"id": "dlv_01", "event": "deploy.failed", "status": "pending", "attempts": 2,
			"last_status_code": 502, "last_error": "The endpoint answered 502 Bad Gateway.",
		}},
	})
	got := run(t, api, "", "subscriptions", "deliveries", "sub_01")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "dlv_01")
	require.Contains(t, got.out, "502")

	api = newAPI(t).reply("POST /subscriptions/sub_01/deliveries/dlv_01/redeliver", map[string]any{"id": "dlv_01"})
	got = run(t, api, "", "subscriptions", "redeliver", "sub_01", "dlv_01")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "Queued dlv_01 again")
}

func TestR373_NotificationPreferencesSetOneChoice(t *testing.T) {
	view := map[string]any{
		"kinds":    []map[string]any{{"kind": "app_shared", "label": "An app was shared with you"}},
		"channels": []map[string]any{{"id": "ntf_smtp", "kind": "smtp"}},
		"choices":  []map[string]any{{"kind": "app_shared", "channel": "ntf_smtp", "enabled": true}},
	}
	api := newAPI(t).reply("PUT /notification-preferences", view)
	got := run(t, api, "", "notifications", "preferences", "--set", "app_shared:ntf_smtp=on")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"choices":[{"kind":"app_shared","channel":"ntf_smtp","enabled":true}]}`,
		api.bodyFor("PUT /notification-preferences"))
	require.Contains(t, got.out, "NTF_SMTP")
	require.Contains(t, got.out, "on")

	got = run(t, newAPI(t), "", "notifications", "preferences", "--set", "app_shared=on")
	require.ErrorContains(t, got.err, "kind:channel=on")
}
