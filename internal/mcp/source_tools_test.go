package mcp_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR091_SourceConnectionToolsMapToTheirEndpoints asserts that an agent
// reaches source connections through the same API as every other surface
// (R-261): listing them, picking a repository and a branch through one, and
// authorizing one with a device code.
func TestR091_SourceConnectionToolsMapToTheirEndpoints(t *testing.T) {
	requireToolCalls(t, []toolCase{
		{"pando_list_sources", `{}`, "GET", "/sources", ""},
		{"pando_list_source_repositories", `{"source_id":"src_github"}`, "GET", "/sources/src_github/repositories", ""},
		{"pando_list_source_repositories", `{"source_id":"src_github","query":"api x"}`, "GET",
			"/sources/src_github/repositories?q=api+x", ""},
		{"pando_list_source_branches", `{"source_id":"src_github","url":"https://github.com/acme/api.git"}`, "GET",
			"/sources/src_github/branches?url=https%3A%2F%2Fgithub.com%2Facme%2Fapi.git", ""},
		{"pando_authorize_source", `{"source_id":"src_github"}`, "POST", "/sources/src_github/authorize", `{"mode":"device"}`},
		{"pando_poll_source_authorization", `{"source_id":"src_github"}`, "POST", "/sources/src_github/authorize/poll", ""},
	})
}

// The source connection tools refuse a call missing what they need before
// reaching the API, and say which argument.
func TestSourceConnectionToolsRefuseMissingArguments(t *testing.T) {
	for _, tc := range []struct{ tool, args, want string }{
		{"pando_list_source_repositories", `{}`, "source_id"},
		{"pando_list_source_branches", `{}`, "source_id"},
		{"pando_list_source_branches", `{"source_id":"src_github"}`, "url"},
		{"pando_authorize_source", `{}`, "source_id"},
		{"pando_poll_source_authorization", `{}`, "source_id"},
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

// TestR091_AnAppIsCreatedThroughANamedConnection asserts that
// pando_create_app passes the connection a private repository is read with.
func TestR091_AnAppIsCreatedThroughANamedConnection(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_create_app",
		`{"name":"api","source_url":"https://github.com/acme/api.git","connection":"src_github"}`))
	require.Len(t, s.calls, 1)
	body, err := json.Marshal(s.calls[0].body)
	require.NoError(t, err)
	require.JSONEq(t,
		`{"name":"api","source":{"type":"git","url":"https://github.com/acme/api.git","ref":"","connection":"src_github"}}`,
		string(body))
}
