package mcp_test

import (
	"testing"
)

// TestR261_AdapterToolsMapToTheirEndpoints asserts R-261 for adapters: an
// agent lists and configures them through the same endpoints as every other
// surface, the install's image registry among them (issue #153), with its
// password in credentials and never in config.
func TestR261_AdapterToolsMapToTheirEndpoints(t *testing.T) {
	requireToolCalls(t, []toolCase{
		{"pando_list_adapters", `{}`, "GET", "/adapters", ""},
		{"pando_list_adapter_kinds", `{}`, "GET", "/adapters/kinds", ""},
		{"pando_configure_adapter",
			`{"id":"reg_main","category":"image_registry","kind":"oci","config":{"url":"https://registry.internal:5000","username":"pando"},"credentials":{"password":"pw"}}`,
			"POST", "/adapters",
			`{"id":"reg_main","category":"image_registry","kind":"oci","config":{"url":"https://registry.internal:5000","username":"pando"},"credentials":{"password":"pw"}}`},
		{"pando_configure_adapter", `{"id":"reg_main","category":"image_registry","kind":"oci","enabled":false}`,
			"POST", "/adapters", `{"id":"reg_main","category":"image_registry","kind":"oci","enabled":false}`},
	})
}
