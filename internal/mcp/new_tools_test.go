package mcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// R-154: a comment on an approval or a rejection reaches the API, and none is
// sent when none was given.
func TestR154_ApprovalToolsSendTheComment(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_reject_deploy", `{"app_id":"app_1","deployment_id":"dep_1","comment":"not on a Friday"}`))
	require.Len(t, s.calls, 1)
	require.Equal(t, map[string]any{"comment": "not on a Friday"}, s.calls[0].body)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_approve_deploy", `{"app_id":"app_1","deployment_id":"dep_1"}`))
	require.Len(t, s.calls, 1)
	require.Equal(t, map[string]any{}, s.calls[0].body)
}

// Every tool this branch added refuses a call missing a required argument —
// or with one of the wrong type — before reaching the API, and says which.
func TestNewToolsRefuseMissingArgumentsWithoutCallingTheAPI(t *testing.T) {
	for _, tc := range []struct{ tool, args, want string }{
		{"pando_set_app_icon", `{}`, "app_id"},
		{"pando_set_app_icon", `{"app_id":"app_1"}`, "image_base64"},
		{"pando_clear_app_icon", `{}`, "app_id"},
		{"pando_favorite_app", `{}`, "app_id"},
		{"pando_unfavorite_app", `{}`, "app_id"},
		{"pando_list_notices", `{}`, "app_id"},
		{"pando_ignore_notice", `{}`, "app_id"},
		{"pando_ignore_notice", `{"app_id":"app_1"}`, "key"},
		{"pando_restore_notice", `{"app_id":"app_1"}`, "key"},
		{"pando_rename_app", `{}`, "app_id"},
		{"pando_rename_app", `{"app_id":"app_1"}`, "name"},
		{"pando_create_section", `{}`, "name"},
		{"pando_rename_section", `{}`, "section_id"},
		{"pando_rename_section", `{"section_id":"sect_1"}`, "name"},
		{"pando_delete_section", `{}`, "section_id"},
		{"pando_add_app_to_section", `{}`, "section_id"},
		{"pando_add_app_to_section", `{"section_id":"sect_1"}`, "app_id"},
		{"pando_remove_app_from_section", `{}`, "section_id"},
		{"pando_remove_app_from_section", `{"section_id":"sect_1"}`, "app_id"},
		{"pando_list_audit", `{"since":7}`, "since"},
		{"pando_approve_deploy", `{}`, "app_id"},
		{"pando_approve_deploy", `{"app_id":"app_1"}`, "deployment_id"},
		{"pando_reject_deploy", `{"app_id":"app_1"}`, "deployment_id"},
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
