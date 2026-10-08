package mcp_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_TheAutoDeployToolsAreTheAPIsCalls asserts that an agent reads and
// changes auto-deploy with the calls the console and CLI make.
func TestR261_TheAutoDeployToolsAreTheAPIsCalls(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, call(1, "pando_get_auto_deploy", `{"app_id":"app_1"}`))
	require.False(t, result(t, replies[0])["isError"] == true, text(t, replies[0]))
	require.Equal(t, "GET", s.calls[0].method)
	require.Equal(t, "/apps/app_1/auto-deploy", s.calls[0].path)

	srv, s = newSession()
	replies = s.run(t, srv, call(1, "pando_set_auto_deploy",
		`{"app_id":"app_1","enabled":true,"trigger":"release_tagged","tag_pattern":"release-*"}`))
	require.False(t, result(t, replies[0])["isError"] == true, text(t, replies[0]))
	require.Equal(t, "PUT", s.calls[0].method)
	require.Equal(t, "/apps/app_1/auto-deploy", s.calls[0].path)
	sent, err := json.Marshal(s.calls[0].body)
	require.NoError(t, err)
	require.JSONEq(t, `{"enabled":true,"trigger":"release_tagged","tag_pattern":"release-*"}`, string(sent))

	srv, s = newSession()
	replies = s.run(t, srv, call(1, "pando_set_auto_deploy", `{"app_id":"app_1"}`))
	require.True(t, result(t, replies[0])["isError"].(bool))
	require.Contains(t, text(t, replies[0]), "enabled is required")
	require.Empty(t, s.calls)
}
