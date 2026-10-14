package cli_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// R-046: an account created or reset from the CLI gets the server's generated
// password, printed once, and must change it at first sign-in unless told not
// to.
func TestR046_UserCreateAndResetUseAGeneratedPassword(t *testing.T) {
	api := newAPI(t).reply("POST /passwords/generate", map[string]string{"password": "Zq7!generated-pass"})

	got := run(t, api, "", "user", "create", "dana", "--name", "Dana")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Zq7!generated-pass")
	require.JSONEq(t,
		`{"username":"dana","display_name":"Dana","email":"","password":"Zq7!generated-pass","must_change_password":true}`,
		api.bodyFor("POST /users"))

	got = run(t, api, "", "user", "reset-password", "usr_01", "--no-change-required")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Zq7!generated-pass")
	require.JSONEq(t, `{"password":"Zq7!generated-pass","must_change_password":false}`,
		api.bodyFor("POST /users/usr_01/password"))
}

// R-078: groups and what they hold, from the CLI as from the API (R-261).
func TestGroupAndProfileCommandsCallTheAPI(t *testing.T) {
	api := newAPI(t)

	for _, tc := range []struct {
		args []string
		call string
		body string
	}{
		{[]string{"user", "update", "usr_01", "--name", "Dana K"}, "PATCH /users/usr_01", `{"display_name":"Dana K"}`},
		{[]string{"group", "add-member", "grp_01", "usr_01"}, "PUT /groups/grp_01/members/usr_01", ""},
		{[]string{"group", "remove-member", "grp_01", "usr_01"}, "DELETE /groups/grp_01/members/usr_01", ""},
		{[]string{"group", "role", "grp_01", "role_administrator"}, "PUT /groups/grp_01/role", `{"role_id":"role_administrator"}`},
		{[]string{"group", "role", "grp_01", "--clear"}, "DELETE /groups/grp_01/role", ""},
	} {
		got := run(t, api, "", tc.args[0], tc.args[1:]...)
		require.NoError(t, got.err, got.errOut)
		if tc.body != "" {
			require.JSONEq(t, tc.body, api.bodyFor(tc.call), tc.call)
		}
		_, path, _ := strings.Cut(tc.call, " ")
		require.True(t, api.sawPath(path), tc.call)
	}

	got := run(t, api, "", "user", "update", "usr_01")
	require.Error(t, got.err, "nothing to change")
}

// TestR078_UserListShowsRolesHeldThroughGroups asserts the CLI prints the
// installation roles an account holds through a group as the API gives them
// (issue #221), rather than only the one granted to the account.
func TestR078_UserListShowsRolesHeldThroughGroups(t *testing.T) {
	held := []map[string]any{{"role_id": "role_administrator",
		"via": map[string]any{"kind": "group", "group_id": "grp_01", "name": "pando-administrator", "source_name": "Okta"}}}
	api := newAPI(t).
		reply("GET /users", map[string]any{"users": []map[string]any{
			{"id": "usr_01", "install_role_id": "", "install_roles": held}}}).
		reply("GET /users/usr_01", map[string]any{"id": "usr_01", "install_role_id": "", "install_roles": held})

	got := run(t, api, "", "user", "list")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, `"pando-administrator"`)
	require.Contains(t, got.out, `"Okta"`)

	got = run(t, api, "", "user", "get", "usr_01")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, `"pando-administrator"`)
}

// R-245: a part's use beside its limits, and a part with no limit said so.
func TestR245_AppUsageShowsEachPartBesideItsLimits(t *testing.T) {
	api := newAPI(t).reply("GET /apps/notes/usage", map[string]any{
		"supported": true,
		"workloads": []map[string]any{
			{"name": "web", "primary": true, "running": true, "cpu_millis": 150, "cpu_limit_millis": 500,
				"memory_bytes": 64 << 20, "memory_limit_bytes": 256 << 20, "disk_bytes": 2 << 20,
				"volumes": []map[string]any{{"name": "data", "bytes": 3 << 20}}},
			{"name": "worker", "running": false, "disk_bytes": -1, "volumes": []any{}},
		},
	})
	got := run(t, api, "", "app", "usage", "notes")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "0.15 of 0.50")
	require.Contains(t, got.out, "64.0 MB of 256.0 MB")
	require.Contains(t, got.out, "data 3.0 MB")
	require.Contains(t, got.out, "stopped")
	require.Contains(t, got.out, "unknown")
}

// R-075a: sharing with a group, with everyone, and with everyone behind a
// passcode, from the CLI as from the console (R-261).
func TestR075a_GrantAddSharesWithAGroupOrEveryone(t *testing.T) {
	api := newAPI(t)

	got := run(t, api, "", "grant", "add", "notes", "--group", "grp_01")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"plane":"data","principal_kind":"group","principal_id":"grp_01"}`, api.bodyFor("POST /apps/notes/grants"))

	api = newAPI(t)
	got = run(t, api, "", "grant", "add", "notes", "--anyone", "--passcode", "open-sesame")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"plane":"data","principal_kind":"anonymous","passcode":"open-sesame"}`, api.bodyFor("POST /apps/notes/grants"))

	got = run(t, api, "", "grant", "passcode", "notes", "gr_01")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"passcode":""}`, api.bodyFor("PATCH /apps/notes/grants/gr_01"))
	require.Contains(t, got.out, "without signing in")

	require.Error(t, run(t, api, "", "grant", "add", "notes", "--user", "usr_01", "--group", "grp_01").err)
	require.Error(t, run(t, api, "", "grant", "add", "notes", "--user", "usr_01", "--passcode", "x").err)
}
