package mcp_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR381_TheAuditStreamToolReadsOnePageFromACursor asserts R-381 and R-261
// for agents: pando_read_audit_stream is GET /audit/stream, one page, with the
// cursor, filters and format passed through and no long poll.
func TestR381_TheAuditStreamToolReadsOnePageFromACursor(t *testing.T) {
	for _, tc := range []struct {
		args  string
		query url.Values
	}{
		{`{}`, url.Values{}},
		{`{"after":"now"}`, url.Values{"after": {"now"}}},
		{`{"after":"c1.abc","limit":50,"action":["grant.","app."],"exclude":["grant.view"],"format":"ocsf"}`,
			url.Values{"after": {"c1.abc"}, "limit": {"50"}, "action": {"grant.", "app."}, "exclude": {"grant.view"}, "format": {"ocsf"}}},
	} {
		t.Run(tc.args, func(t *testing.T) {
			srv, s := newSession()
			s.run(t, srv, call(1, "pando_read_audit_stream", tc.args))
			require.Len(t, s.calls, 1)
			require.Equal(t, "GET", s.calls[0].method)
			path, raw, _ := strings.Cut(s.calls[0].path, "?")
			require.Equal(t, "/audit/stream", path)
			got, err := url.ParseQuery(raw)
			require.NoError(t, err)
			require.Equal(t, tc.query, got)
			require.Empty(t, got.Get("wait"), "an agent's call returns at once")
		})
	}
}

// TestR381_TheAuditStreamToolSaysHowToContinue asserts the description tells
// an agent to pass the cursor back and that delivery is at least once.
func TestR381_TheAuditStreamToolSaysHowToContinue(t *testing.T) {
	srv, s := newSession()
	tools := result(t, s.run(t, srv, rpc(1, "tools/list", ""))[0])["tools"].([]any)
	descriptions := map[string]string{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		descriptions[tool["name"].(string)] = tool["description"].(string)
	}
	stream := descriptions["pando_read_audit_stream"]
	require.Contains(t, stream, "Pass the cursor back")
	require.Contains(t, stream, "at least once")
	require.Contains(t, stream, "install.audit.read")
	require.Contains(t, descriptions["pando_list_audit_sinks"], "install.audit.read")

	// design 12 §7: no export tool, for the reason there is no archive download.
	for name := range descriptions {
		require.NotContains(t, name, "export_audit")
	}
}

// TestR383_TheAuditSinksToolListsTheSinks asserts R-383 and R-261 for agents.
func TestR383_TheAuditSinksToolListsTheSinks(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_list_audit_sinks", `{}`))
	require.Len(t, s.calls, 1)
	require.Equal(t, "GET", s.calls[0].method)
	require.Equal(t, "/audit/sinks", s.calls[0].path)
}

func TestTheAuditStreamToolRefusesMalformedArguments(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{`{"after":7}`, "after"},
		{`{"limit":0}`, "limit"},
		{`{"limit":1001}`, "limit"},
		{`{"limit":2.5}`, "limit"},
		{`{"action":"grant."}`, "action"},
		{`{"exclude":[1]}`, "exclude"},
	} {
		t.Run(tc.args, func(t *testing.T) {
			srv, s := newSession()
			replies := s.run(t, srv, call(1, "pando_read_audit_stream", tc.args))
			require.Empty(t, s.calls, "nothing reaches the API")
			require.Equal(t, true, result(t, replies[0])["isError"])
			require.Contains(t, text(t, replies[0]), tc.want)
		})
	}
}
