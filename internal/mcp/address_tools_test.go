package mcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_AnAgentCanChangeAnAppsAddress asserts R-261 for an app's address:
// PUT /apps/{id}/routing, which the console has, is a tool too, and it can
// name the domain — another of a Cloudflare adapter's zones.
func TestR261_AnAgentCanChangeAnAppsAddress(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_get_address", `{"app_id":"app_1"}`))
	require.Len(t, s.calls, 1)
	require.Equal(t, "GET", s.calls[0].method)
	require.Equal(t, "/apps/app_1/routing", s.calls[0].path)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_set_address",
		`{"app_id":"app_1","hostname":"docs","domain":"trypando.ai","confirm":true}`))
	require.Len(t, s.calls, 1)
	require.Equal(t, "PUT", s.calls[0].method)
	require.Equal(t, "/apps/app_1/routing", s.calls[0].path)
	require.Equal(t, map[string]any{"hostname": "docs", "domain": "trypando.ai", "confirm": true}, s.calls[0].body)

	// Unconfirmed is sent unconfirmed, so the API says what would move.
	srv, s = newSession()
	s.run(t, srv, call(1, "pando_set_address", `{"app_id":"app_1","mode":"path","path_prefix":"/notes"}`))
	require.Len(t, s.calls, 1)
	require.Equal(t, map[string]any{"mode": "path", "path_prefix": "/notes", "confirm": false}, s.calls[0].body)
}

func TestSettingAnAddressNeedsTheApp(t *testing.T) {
	for _, args := range []string{`{}`, `{"app_id":"app_1","hostname":7}`} {
		srv, s := newSession()
		replies := s.run(t, srv, call(1, "pando_set_address", args))
		require.Empty(t, s.calls, "nothing reaches the API")
		require.Equal(t, true, result(t, replies[0])["isError"])
	}
}
