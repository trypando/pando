package mcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR143a_TheImageUpdateToolsMapToTheirEndpoints asserts R-143a and R-261 for
// MCP: each tool is the call the CLI and the console make.
func TestR143a_TheImageUpdateToolsMapToTheirEndpoints(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, call(1, "pando_check_app_image", `{"app_id":"app_1"}`))
	require.False(t, result(t, replies[0])["isError"] == true, text(t, replies[0]))
	require.Equal(t, "GET", s.calls[0].method)
	require.Equal(t, "/apps/app_1/image-update", s.calls[0].path)

	srv, s = newSession()
	replies = s.run(t, srv, call(1, "pando_deploy_newer_image", `{"app_id":"app_1","digest":"sha256:b2"}`))
	require.False(t, result(t, replies[0])["isError"] == true, text(t, replies[0]))
	require.Equal(t, "POST", s.calls[0].method)
	require.Equal(t, "/apps/app_1/image-update", s.calls[0].path)
	require.Equal(t, map[string]any{"digest": "sha256:b2"}, s.calls[0].body)

	srv, s = newSession()
	replies = s.run(t, srv, call(1, "pando_deploy_newer_image", `{"app_id":"app_1"}`))
	require.True(t, result(t, replies[0])["isError"].(bool))
	require.Contains(t, text(t, replies[0]), "digest is required")
	require.Empty(t, s.calls)
}
