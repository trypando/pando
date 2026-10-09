package mcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR397_IdleSettingsAsTools asserts R-397 with R-261: an agent reads and
// sets an app's idle settings through the same endpoint as the console, with
// "never" and "default" meaning 0 and the installation's.
func TestR397_IdleSettingsAsTools(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_get_app_idle", `{"app_id":"app_1"}`))
	require.Len(t, s.calls, 1)
	require.Equal(t, "GET", s.calls[0].method)
	require.Equal(t, "/apps/app_1/idle", s.calls[0].path)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_set_app_idle", `{"app_id":"app_1","stop_days":"never","delete_days":"default"}`))
	require.Len(t, s.calls, 1)
	require.Equal(t, "PUT", s.calls[0].method)
	require.Equal(t, map[string]any{"stop_days": 0, "delete_days": nil}, s.calls[0].body)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_set_app_idle", `{"app_id":"app_1","stop_days":30,"delete_days":"90"}`))
	require.Equal(t, map[string]any{"stop_days": 30, "delete_days": 90}, s.calls[0].body)
}

// TestR244_AppLimitsAsTools asserts R-244 with R-261: the limit on an
// account or a group, read and set by an agent, to exactly one of them.
func TestR244_AppLimitsAsTools(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_get_app_limit", `{"user_id":"usr_1"}`))
	require.Equal(t, "/users/usr_1/app-limit", s.calls[0].path)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_set_app_limit", `{"group_id":"grp_1","max_apps":5}`))
	require.Equal(t, "PUT", s.calls[0].method)
	require.Equal(t, "/groups/grp_1/app-limit", s.calls[0].path)
	require.Equal(t, map[string]any{"max_apps": 5}, s.calls[0].body)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_set_app_limit", `{"user_id":"usr_1","max_apps":"clear"}`))
	require.Equal(t, map[string]any{"max_apps": nil}, s.calls[0].body)
}

// TestLimitToolsRefuseWhatTheyCannotSendWithoutCallingTheAPI: a missing or
// malformed argument is refused before the API, saying what was wanted.
func TestLimitToolsRefuseWhatTheyCannotSendWithoutCallingTheAPI(t *testing.T) {
	for _, tc := range []struct{ tool, args, want string }{
		{"pando_get_app_idle", `{}`, "app_id"},
		{"pando_set_app_idle", `{"app_id":"app_1","stop_days":30}`, "delete_days"},
		{"pando_set_app_idle", `{"app_id":"app_1","stop_days":"soon","delete_days":0}`, "\"never\", or \"default\""},
		{"pando_set_app_idle", `{"app_id":"app_1","stop_days":-3,"delete_days":0}`, "stop_days"},
		{"pando_get_app_limit", `{}`, "user_id or group_id"},
		{"pando_set_app_limit", `{"user_id":"usr_1","group_id":"grp_1","max_apps":2}`, "not both"},
		{"pando_set_app_limit", `{"user_id":"usr_1","max_apps":2.5}`, "max_apps"},
		{"pando_set_app_limit", `{"user_id":"usr_1"}`, "max_apps"},
	} {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			srv, s := newSession()
			replies := s.run(t, srv, call(1, tc.tool, tc.args))
			require.Empty(t, s.calls, "nothing reaches the API")
			require.Equal(t, true, result(t, replies[0])["isError"])
			require.Contains(t, text(t, replies[0]), tc.want)
		})
	}
}
