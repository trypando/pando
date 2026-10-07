package cli_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_TheImageRegistryIsSetFromTheCLI asserts R-261 for the install
// registry (issue #72): the CLI reads, sets and clears it through the same
// endpoint as the console, sends only the settings given, and reads the
// password from the terminal rather than a flag, never echoing it (R-194).
func TestR261_TheImageRegistryIsSetFromTheCLI(t *testing.T) {
	api := newAPI(t).
		reply("PUT /image-registry", map[string]any{"configured": true, "password_set": true}).
		reply("GET /image-registry", map[string]any{"configured": true, "url": "https://registry.internal:5000"})

	got := run(t, api, "registry-pass\n", "image-registry", "set", "--url", "https://registry.internal:5000", "--password", "--always")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"url":"https://registry.internal:5000","password":"registry-pass","always":true}`,
		api.bodyFor("PUT /image-registry"))
	require.NotContains(t, got.out, "registry-pass")

	got = run(t, api, "", "image-registry", "show")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "registry.internal:5000")

	removing := newAPI(t).reply("PUT /image-registry", map[string]any{"configured": true})
	got = run(t, removing, "", "image-registry", "set", "--remove-password")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"password":""}`, removing.bodyFor("PUT /image-registry"))

	got = run(t, api, "", "image-registry", "clear")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Removed")

	got = run(t, api, "", "image-registry", "set")
	require.Error(t, got.err, "nothing to change")
}
