package mcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_TheImageRegistryToolsMapToTheirEndpoints asserts R-261 for the
// install registry (issue #72): each tool is one call to the endpoint the
// console and the CLI use, and setting it sends only the fields given.
func TestR261_TheImageRegistryToolsMapToTheirEndpoints(t *testing.T) {
	for _, tc := range []struct {
		tool, args   string
		method, path string
	}{
		{"pando_get_image_registry", `{}`, "GET", "/image-registry"},
		{"pando_set_image_registry", `{"url":"https://registry.internal:5000","password":"p","insecure":false}`, "PUT", "/image-registry"},
		{"pando_clear_image_registry", `{}`, "DELETE", "/image-registry"},
	} {
		srv, s := newSession()
		replies := s.run(t, srv, call(1, tc.tool, tc.args))
		require.False(t, result(t, replies[0])["isError"] == true, "%s: %s", tc.tool, text(t, replies[0]))
		require.Len(t, s.calls, 1, tc.tool)
		require.Equal(t, tc.method, s.calls[0].method, tc.tool)
		require.Equal(t, tc.path, s.calls[0].path, tc.tool)
	}

	srv, s := newSession()
	s.run(t, srv, call(1, "pando_set_image_registry", `{"username":"pando","always":true}`))
	require.Equal(t, map[string]any{"username": "pando", "always": true}, s.calls[0].body)

	srv, s = newSession()
	replies := s.run(t, srv, call(1, "pando_set_image_registry", `{}`))
	require.True(t, result(t, replies[0])["isError"].(bool))
	require.Empty(t, s.calls, "nothing to change, nothing sent")
}
