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

// TestR105_TheImageRegistryCommandsPassOnWhatTheServerRefused asserts that a
// refusal from the server reaches the person running the command, and that the
// password flags refuse what cannot be meant: both at once, and an empty
// password, which is what --remove-password is for.
func TestR105_TheImageRegistryCommandsPassOnWhatTheServerRefused(t *testing.T) {
	refusal := map[string]string{"code": "PERM_DENIED",
		"message": "Changing the install registry needs install.adapters.manage."}
	api := newAPI(t).
		fail("GET /image-registry", 403, refusal).
		fail("PUT /image-registry", 403, refusal).
		fail("DELETE /image-registry", 403, refusal)

	for _, args := range [][]string{{"show"}, {"set", "--url", "https://registry.internal"}, {"clear"}} {
		got := run(t, api, "", "image-registry", args...)
		require.Error(t, got.err, args)
		require.Contains(t, got.err.Error(), "install.adapters.manage", args)
	}

	quiet := newAPI(t)
	got := run(t, quiet, "pw\n", "image-registry", "set", "--password", "--remove-password")
	require.ErrorContains(t, got.err, "not both")

	got = run(t, quiet, "\n", "image-registry", "set", "--password")
	require.ErrorContains(t, got.err, "--remove-password")

	got = run(t, quiet, "", "image-registry", "set", "--password")
	require.Error(t, got.err, "nothing to read a password from")
	require.False(t, quiet.sawPath("/image-registry"), "nothing was sent for any of them")
}
