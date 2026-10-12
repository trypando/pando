package mcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_DetectionReviseAndDiffHaveTools asserts R-261 for issue #211:
// revising the plan and previewing what accepting changes are endpoints the
// CLI and the console reach, so MCP reaches them too.
func TestR261_DetectionReviseAndDiffHaveTools(t *testing.T) {
	for _, tc := range []struct {
		tool, args   string
		method, path string
	}{
		{"pando_revise_detection", `{"app_id":"app_1","message":"It serves on 8080."}`, "POST", "/apps/app_1/detection/revise"},
		{"pando_get_detection_diff", `{"app_id":"app_1"}`, "GET", "/apps/app_1/detection/diff"},
	} {
		srv, s := newSession()
		replies := s.run(t, srv, call(1, tc.tool, tc.args))
		require.False(t, result(t, replies[0])["isError"] == true, "%s: %s", tc.tool, text(t, replies[0]))
		require.Len(t, s.calls, 1, tc.tool)
		require.Equal(t, tc.method, s.calls[0].method, tc.tool)
		require.Equal(t, tc.path, s.calls[0].path, tc.tool)
	}

	srv, s := newSession()
	replies := s.run(t, srv, call(1, "pando_revise_detection", `{"app_id":"app_1"}`))
	require.True(t, result(t, replies[0])["isError"].(bool))
	require.Contains(t, text(t, replies[0]), "message is required")
	require.Empty(t, s.calls)
}
