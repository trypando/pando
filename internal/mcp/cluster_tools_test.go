package mcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR414_ClusterAsTools asserts R-414 and R-415 with R-261: an agent lists
// and removes hosts and mints, lists and revokes join tokens through the same
// endpoints as the console.
func TestR414_ClusterAsTools(t *testing.T) {
	for _, tc := range []struct {
		tool, args, method, path string
		body                     map[string]any
	}{
		{"pando_list_hosts", `{}`, "GET", "/cluster/hosts", nil},
		{"pando_remove_host", `{"host_id":"host_1"}`, "DELETE", "/cluster/hosts/host_1", nil},
		{"pando_mark_host_lost", `{"host_id":"host_1"}`, "POST", "/cluster/hosts/host_1/lost", nil},
		{"pando_list_join_tokens", `{}`, "GET", "/cluster/join-tokens", nil},
		{"pando_revoke_join_token", `{"token_id":"jt_1"}`, "DELETE", "/cluster/join-tokens/jt_1", nil},
		{"pando_create_join_token", `{}`, "POST", "/cluster/join-tokens", map[string]any{}},
		{"pando_create_join_token", `{"roles":["control","app"],"max_uses":2,"expires_hours":12,"address":"https://pando.example.com"}`,
			"POST", "/cluster/join-tokens", map[string]any{"roles": []string{"control", "app"}, "max_uses": 2, "expires_hours": 12, "address": "https://pando.example.com"}},
	} {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			srv, s := newSession()
			s.run(t, srv, call(1, tc.tool, tc.args))
			require.Len(t, s.calls, 1)
			require.Equal(t, tc.method, s.calls[0].method)
			require.Equal(t, tc.path, s.calls[0].path)
			if tc.body != nil {
				require.Equal(t, tc.body, s.calls[0].body)
			}
		})
	}
}

func TestClusterToolsRefuseWhatTheyCannotSendWithoutCallingTheAPI(t *testing.T) {
	for _, tc := range []struct{ tool, args, want string }{
		{"pando_remove_host", `{}`, "host_id"},
		{"pando_mark_host_lost", `{}`, "host_id"},
		{"pando_revoke_join_token", `{}`, "token_id"},
		{"pando_create_join_token", `{"max_uses":1.5}`, "max_uses"},
		{"pando_create_join_token", `{"expires_hours":0}`, "expires_hours"},
		{"pando_create_join_token", `{"roles":5}`, "roles"},
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
