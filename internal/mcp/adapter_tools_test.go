package mcp_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_AdapterToolsMapToTheirEndpoints asserts R-261 for adapters: an
// agent lists and configures them through the same endpoints as every other
// surface, the install's image registry among them (issue #153), with its
// password in credentials and never in config.
func TestR261_AdapterToolsMapToTheirEndpoints(t *testing.T) {
	for _, tc := range []struct {
		tool, args   string
		method, path string
		body         string
	}{
		{"pando_list_adapters", `{}`, "GET", "/adapters", ""},
		{"pando_list_adapter_kinds", `{}`, "GET", "/adapters/kinds", ""},
		{"pando_configure_adapter",
			`{"id":"reg_main","category":"image_registry","kind":"oci","config":{"url":"https://registry.internal:5000","username":"pando"},"credentials":{"password":"pw"}}`,
			"POST", "/adapters",
			`{"id":"reg_main","category":"image_registry","kind":"oci","config":{"url":"https://registry.internal:5000","username":"pando"},"credentials":{"password":"pw"}}`},
		{"pando_configure_adapter", `{"id":"reg_main","category":"image_registry","kind":"oci","enabled":false}`,
			"POST", "/adapters", `{"id":"reg_main","category":"image_registry","kind":"oci","enabled":false}`},
	} {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			srv, s := newSession()
			s.run(t, srv, call(1, tc.tool, tc.args))
			require.Len(t, s.calls, 1)
			require.Equal(t, tc.method, s.calls[0].method)
			require.Equal(t, tc.path, s.calls[0].path)
			if tc.body != "" {
				body, err := json.Marshal(s.calls[0].body)
				require.NoError(t, err)
				require.JSONEq(t, tc.body, string(body))
			}
		})
	}
}
