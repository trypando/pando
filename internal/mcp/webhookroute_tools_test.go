package mcp_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR432_TheWebhookRouteToolsAreTheAPIsCalls asserts an agent manages routes
// with the calls the console and CLI make.
func TestR432_TheWebhookRouteToolsAreTheAPIsCalls(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, call(1, "pando_list_webhook_routes", `{"app_id":"app_1"}`))
	require.False(t, result(t, replies[0])["isError"] == true, text(t, replies[0]))
	require.Equal(t, "GET", s.calls[0].method)
	require.Equal(t, "/apps/app_1/webhook-routes", s.calls[0].path)

	srv, s = newSession()
	replies = s.run(t, srv, call(1, "pando_add_webhook_route",
		`{"app_id":"app_1","name":"noodle","method":"POST","path":"/hook","kind":"hmac_sha256","header":"X-Sig","prefix":"sha256="}`))
	require.False(t, result(t, replies[0])["isError"] == true, text(t, replies[0]))
	require.Equal(t, "POST", s.calls[0].method)
	require.Equal(t, "/apps/app_1/webhook-routes", s.calls[0].path)
	sent, err := json.Marshal(s.calls[0].body)
	require.NoError(t, err)
	require.JSONEq(t, `{"name":"noodle","method":"POST","path":"/hook","allow_repeats":false,"secret":"",
		"verification":{"kind":"hmac_sha256","header":"X-Sig","prefix":"sha256="}}`, string(sent))

	srv, s = newSession()
	replies = s.run(t, srv, call(1, "pando_add_webhook_route", `{"app_id":"app_1","name":"noodle"}`))
	require.True(t, result(t, replies[0])["isError"].(bool))
	require.Empty(t, s.calls)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_remove_webhook_route", `{"app_id":"app_1","name":"noodle"}`))
	require.Equal(t, "DELETE", s.calls[0].method)
	require.Equal(t, "/apps/app_1/webhook-routes/noodle", s.calls[0].path)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_set_webhook_route_secret", `{"app_id":"app_1","name":"noodle"}`))
	require.Equal(t, "POST", s.calls[0].method)
	require.Equal(t, "/apps/app_1/webhook-routes/noodle/secret", s.calls[0].path)
	require.Nil(t, s.calls[0].body, "no body generates one")

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_set_webhook_route_secret", `{"app_id":"app_1","name":"noodle","secret":"whsec_from_provider_1"}`))
	require.NotNil(t, s.calls[0].body)

	srv, s = newSession()
	replies = s.run(t, srv, call(1, "pando_remove_webhook_route", `{"app_id":"app_1"}`))
	require.True(t, result(t, replies[0])["isError"].(bool))
	require.Empty(t, s.calls)
}
