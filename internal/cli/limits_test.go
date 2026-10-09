package cli_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR244_AppLimitsFromTheCLI asserts R-244 with R-261: the limit on an
// account or a group is read, set and cleared from the CLI as from the API.
func TestR244_AppLimitsFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("GET /users/usr_01/app-limit", map[string]any{
		"limit": 5, "source": "group", "group_name": "Finance", "owned": 3, "max_apps": nil,
	})

	got := run(t, api, "", "user", "app-limit", "usr_01")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Finance")

	for _, tc := range []struct {
		args []string
		call string
		body string
	}{
		{[]string{"user", "app-limit", "usr_01", "2"}, "PUT /users/usr_01/app-limit", `{"max_apps":2}`},
		{[]string{"user", "app-limit", "usr_01", "0"}, "PUT /users/usr_01/app-limit", `{"max_apps":0}`},
		{[]string{"user", "app-limit", "usr_01", "--clear"}, "PUT /users/usr_01/app-limit", `{"max_apps":null}`},
		{[]string{"group", "app-limit", "grp_01", "10"}, "PUT /groups/grp_01/app-limit", `{"max_apps":10}`},
		{[]string{"group", "app-limit", "grp_01", "--clear"}, "PUT /groups/grp_01/app-limit", `{"max_apps":null}`},
	} {
		api := newAPI(t)
		got := run(t, api, "", tc.args[0], tc.args[1:]...)
		require.NoError(t, got.err, got.errOut)
		require.JSONEq(t, tc.body, api.bodyFor(tc.call), tc.args)
	}

	got = run(t, api, "", "group", "app-limit", "grp_01", "lots")
	require.ErrorContains(t, got.err, "is not a number of apps")
	got = run(t, api, "", "group", "app-limit", "grp_01", "3", "--clear")
	require.Error(t, got.err)
}

// TestR397_IdleSettingsFromTheCLI asserts R-397 with R-261: a flag left out
// keeps what the app has, never and default say what they mean, and nothing
// is sent for a value that is not one.
func TestR397_IdleSettingsFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("GET /apps/notes/idle", map[string]any{
		"stop_days": 14, "delete_days": nil, "effective_stop_days": 14, "effective_delete_days": 90,
	})

	got := run(t, api, "", "app", "idle", "notes")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "effective_delete_days")

	got = run(t, api, "", "app", "idle", "notes", "--delete-days", "120")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"stop_days":14,"delete_days":120}`, api.bodyFor("PUT /apps/notes/idle"))

	api = newAPI(t).reply("GET /apps/notes/idle", map[string]any{"stop_days": 14, "delete_days": nil})
	got = run(t, api, "", "app", "idle", "notes", "--stop-days", "never", "--delete-days", "default")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"stop_days":0,"delete_days":null}`, api.bodyFor("PUT /apps/notes/idle"))

	got = run(t, api, "", "app", "idle", "notes", "--stop-days", "soon")
	require.Error(t, got.err)
	require.Contains(t, got.err.Error(), "never, or default")
}
