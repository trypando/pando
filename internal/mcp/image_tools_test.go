package mcp_test

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/mcp"
)

// Each new tool is one endpoint (R-261, issue #41): the call it makes is the
// call the CLI and the console make.
func TestR261_TheUploadAndRegistryCredentialToolsMapToTheirEndpoints(t *testing.T) {
	archive := base64.StdEncoding.EncodeToString([]byte("not really a gzip, but bytes"))
	for _, tc := range []struct {
		tool, args   string
		method, path string
	}{
		{"pando_upload_source", `{"app_id":"app_1","archive_base64":"` + archive + `"}`, "POST", "/apps/app_1/source"},
		{"pando_rerun_detection", `{"app_id":"app_1"}`, "POST", "/apps/app_1/detection/rerun"},
		{"pando_set_registry_credential", `{"app_id":"app_1","registry_credential":{"kind":"ecr","access_key_id":"AKIA","secret_access_key":"s"}}`, "PUT", "/apps/app_1/registry-credential"},
		{"pando_get_registry_credential", `{"app_id":"app_1"}`, "GET", "/apps/app_1/registry-credential"},
		{"pando_remove_registry_credential", `{"app_id":"app_1"}`, "DELETE", "/apps/app_1/registry-credential"},
		{"pando_create_app", `{"name":"notes","source_url":"https://github.com/acme/notes","ref":"main"}`, "POST", "/apps"},
	} {
		srv, s := newSession()
		replies := s.run(t, srv, call(1, tc.tool, tc.args))
		require.False(t, result(t, replies[0])["isError"] == true, "%s: %s", tc.tool, text(t, replies[0]))
		require.Len(t, s.calls, 1, tc.tool)
		require.Equal(t, tc.method, s.calls[0].method, tc.tool)
		require.Equal(t, tc.path, s.calls[0].path, tc.tool)
	}

	// The archive is sent as the file it is, not as JSON.
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_upload_source", `{"app_id":"app_1","archive_base64":"`+archive+`"}`))
	sent, ok := s.calls[0].body.(mcp.Bytes)
	require.True(t, ok)
	require.Equal(t, "application/gzip", sent.ContentType)
	require.Equal(t, "not really a gzip, but bytes", string(sent.Data))
}

// A malformed call is refused before it reaches the API, saying what was wrong.
func TestTheNewToolsRefuseMalformedCalls(t *testing.T) {
	for _, tc := range []struct{ tool, args, says string }{
		{"pando_upload_source", `{"app_id":"app_1","archive_base64":"%%%"}`, "not valid base64"},
		{"pando_upload_source", `{"app_id":"app_1"}`, "archive_base64 is required"},
		{"pando_upload_source", `{"archive_base64":"eA=="}`, "app_id is required"},
		{"pando_rerun_detection", `{}`, "app_id is required"},
		{"pando_set_registry_credential", `{"app_id":"app_1"}`, "registry_credential is required"},
		{"pando_set_registry_credential", `{"registry_credential":{"kind":"basic"}}`, "app_id is required"},
		{"pando_get_registry_credential", `{}`, "app_id is required"},
		{"pando_remove_registry_credential", `{}`, "app_id is required"},
	} {
		srv, s := newSession()
		replies := s.run(t, srv, call(1, tc.tool, tc.args))
		require.True(t, result(t, replies[0])["isError"].(bool), tc.tool)
		require.Contains(t, text(t, replies[0]), tc.says, tc.tool)
		require.Empty(t, s.calls, tc.tool)
	}
}
