package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/mcp"
)

// apiCall is one call the server made on the agent's behalf.
type apiCall struct {
	method, path string
	body         any
}

// session drives a server over a scripted set of requests and returns the
// replies, plus every API call the tools made.
type session struct {
	calls  []apiCall
	result string
	err    error
}

func (s *session) run(t *testing.T, srv *mcp.Server, requests ...string) []map[string]any {
	t.Helper()

	srv.Call = func(_ context.Context, method, path string, body, out any) error {
		s.calls = append(s.calls, apiCall{method: method, path: path, body: body})
		if s.err != nil {
			return s.err
		}
		body2 := s.result
		if body2 == "" {
			body2 = `{"ok":true}`
		}
		return json.Unmarshal([]byte(body2), out)
	}

	var in strings.Builder
	for _, r := range requests {
		in.WriteString(r)
		in.WriteString("\n")
	}
	var out strings.Builder
	require.NoError(t, srv.Serve(context.Background(), strings.NewReader(in.String()), &out))

	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var reply map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &reply), line)
		replies = append(replies, reply)
	}
	return replies
}

func newSession() (*mcp.Server, *session) {
	return &mcp.Server{}, &session{}
}

func rpc(id int, method, params string) string {
	if params == "" {
		return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"` + method + `"}`
	}
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"` + method + `","params":` + params + `}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func call(id int, name, args string) string {
	return rpc(id, "tools/call", `{"name":"`+name+`","arguments":`+args+`}`)
}

func result(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	require.NotContains(t, reply, "error", "%v", reply)
	res, ok := reply["result"].(map[string]any)
	require.True(t, ok, "%v", reply)
	return res
}

// text returns a tool result's single text block.
func text(t *testing.T, reply map[string]any) string {
	t.Helper()
	content, ok := result(t, reply)["content"].([]any)
	require.True(t, ok, "%v", reply)
	require.Len(t, content, 1)
	return content[0].(map[string]any)["text"].(string)
}

func TestInitializeAnnouncesTheProtocolAndTools(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, rpc(1, "initialize", `{}`))

	require.Len(t, replies, 1)
	res := result(t, replies[0])
	require.Equal(t, "2024-11-05", res["protocolVersion"])
	require.Contains(t, res["capabilities"].(map[string]any), "tools")
	require.Equal(t, "pando", res["serverInfo"].(map[string]any)["name"])
}

// A notification has no id and takes no reply. Answering one is a protocol
// violation most clients log loudly.
func TestNotificationsAreNotAnswered(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"initialized"}`)

	require.Empty(t, replies)
}

func TestPing(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, rpc(1, "ping", ""))
	require.Equal(t, map[string]any{}, result(t, replies[0]))
}

func TestAnUnknownMethodIsAProtocolError(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, rpc(1, "resources/list", ""))

	rpcErr := replies[0]["error"].(map[string]any)
	require.EqualValues(t, -32601, rpcErr["code"])
	require.Contains(t, rpcErr["message"], "resources/list")
}

func TestAMalformedLineIsAParseErrorAndTheStreamContinues(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, "{not json", "", rpc(2, "ping", ""))

	require.Len(t, replies, 2, "a blank line is skipped, not answered")
	require.EqualValues(t, -32700, replies[0]["error"].(map[string]any)["code"])
	require.Equal(t, map[string]any{}, result(t, replies[1]))
}

// One tool per endpoint, and the mapping is deliberately boring: a tool that
// composed several calls would be a capability the CLI and console do not have,
// which is what R-261 forbids.
func TestR261_EveryToolIsOneEndpointAndIsDescribed(t *testing.T) {
	srv, s := newSession()
	tools := result(t, s.run(t, srv, rpc(1, "tools/list", ""))[0])["tools"].([]any)

	names := map[string]bool{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		names[name] = true

		require.True(t, strings.HasPrefix(name, "pando_"), name)
		require.NotEmpty(t, tool["description"], name)

		schema := tool["inputSchema"].(map[string]any)
		require.Equal(t, "object", schema["type"], name)
		require.Contains(t, schema, "properties", name)
		require.Contains(t, schema, "required", name)
	}

	for _, want := range []string{
		"pando_list_apps", "pando_get_app", "pando_create_app",
		"pando_get_detection", "pando_answer_detection", "pando_accept_proposal",
		"pando_plan", "pando_deploy", "pando_get_logs", "pando_get_status",
		"pando_stop_app", "pando_start_app", "pando_restart_app",
	} {
		require.True(t, names[want], "missing %s", want)
	}
}

// Exec, secret value reads, grant mutation, policy mutation and user deletion
// are absent — not because this list is the boundary (host policy is, O-12) but
// because offering a tool the policy will refuse wastes the agent's turn.
func TestO12_TheMostDangerousActionsAreNotOfferedAsTools(t *testing.T) {
	srv, s := newSession()
	tools := result(t, s.run(t, srv, rpc(1, "tools/list", ""))[0])["tools"].([]any)

	for _, raw := range tools {
		name := raw.(map[string]any)["name"].(string)
		for _, forbidden := range []string{"exec", "secret", "grant", "policy", "delete_user"} {
			require.NotContains(t, name, forbidden)
		}
	}
}

// A courtesy, not a boundary: it stops the agent wasting a turn on a call it
// cannot make. An agent holding a token could call the REST API directly.
func TestADisabledToolIsNeitherListedNorCallable(t *testing.T) {
	srv, s := newSession()
	srv.ToolsDisabled = map[string]bool{"pando_deploy": true}

	replies := s.run(t, srv, rpc(1, "tools/list", ""), call(2, "pando_deploy", `{"app_id":"app_01HQ8"}`))

	for _, raw := range result(t, replies[0])["tools"].([]any) {
		require.NotEqual(t, "pando_deploy", raw.(map[string]any)["name"])
	}

	require.Contains(t, replies[1]["error"].(map[string]any)["message"], "unknown tool: pando_deploy")
	require.Empty(t, s.calls, "nothing reached the API")
}

func TestAnUnknownToolIsRefusedWithoutCallingTheAPI(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, call(1, "pando_delete_everything", `{}`))

	require.Contains(t, replies[0]["error"].(map[string]any)["message"], "unknown tool")
	require.Empty(t, s.calls)
}

func TestMalformedParamsAreAProtocolError(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, rpc(1, "tools/call", `"not an object"`))

	require.EqualValues(t, -32602, replies[0]["error"].(map[string]any)["code"])
	require.Empty(t, s.calls)
}

func TestEachToolMapsToItsEndpoint(t *testing.T) {
	for _, tc := range []struct {
		tool, args   string
		method, path string
	}{
		{"pando_list_apps", `{}`, "GET", "/apps"},
		{"pando_get_app", `{"app_id":"app_01HQ8"}`, "GET", "/apps/app_01HQ8"},
		{"pando_get_detection", `{"app_id":"app_01HQ8"}`, "GET", "/apps/app_01HQ8/detection"},
		// Issue #80: an agent waits on detection the way every client does,
		// through the API's long poll (R-261).
		{"pando_get_detection", `{"app_id":"app_01HQ8","wait_seconds":30}`, "GET", "/apps/app_01HQ8/detection?wait=30"},
		{"pando_get_detection", `{"app_id":"app_01HQ8","wait_seconds":0}`, "GET", "/apps/app_01HQ8/detection"},
		{"pando_accept_proposal", `{"app_id":"app_01HQ8"}`, "POST", "/apps/app_01HQ8/detection/accept"},
		{"pando_plan", `{"app_id":"app_01HQ8"}`, "POST", "/apps/app_01HQ8/plan"},
		{"pando_deploy", `{"app_id":"app_01HQ8"}`, "POST", "/apps/app_01HQ8/deployments"},

		// R-154: deploy approval, from every surface (R-261).
		{"pando_list_approvals", `{}`, "GET", "/approvals"},
		{"pando_approve_deploy", `{"app_id":"app_01HQ8","deployment_id":"dep_01"}`, "POST", "/apps/app_01HQ8/deployments/dep_01/approve"},
		{"pando_reject_deploy", `{"app_id":"app_01HQ8","deployment_id":"dep_01","comment":"not yet"}`, "POST", "/apps/app_01HQ8/deployments/dep_01/reject"},
		{"pando_get_logs", `{"app_id":"app_01HQ8"}`, "GET", "/apps/app_01HQ8/logs"},
		// R-261: the console and the CLI can read one part of a multi-part
		// app's logs, so an agent can too.
		{"pando_get_logs", `{"app_id":"app_01HQ8","workload":"worker"}`, "GET", "/apps/app_01HQ8/logs?workload=worker"},
		{"pando_get_logs", `{"app_id":"app_01HQ8","workload":"worker","tail":50}`, "GET", "/apps/app_01HQ8/logs?tail=50&workload=worker"},
		{"pando_get_status", `{"app_id":"app_01HQ8"}`, "GET", "/apps/app_01HQ8/status"},

		// R-261: stopping an app without deleting it is a thing the API can
		// do, so it is a thing every surface can do.
		{"pando_stop_app", `{"app_id":"app_01HQ8"}`, "POST", "/apps/app_01HQ8/stop"},
		{"pando_start_app", `{"app_id":"app_01HQ8"}`, "POST", "/apps/app_01HQ8/start"},
		{"pando_restart_app", `{"app_id":"app_01HQ8"}`, "POST", "/apps/app_01HQ8/restart"},

		// R-340: the tile image, settable from every surface (R-261).
		{"pando_set_app_icon", `{"app_id":"app_01HQ8","image_base64":"iVBORw=="}`, "PUT", "/apps/app_01HQ8/icon"},
		{"pando_clear_app_icon", `{"app_id":"app_01HQ8"}`, "DELETE", "/apps/app_01HQ8/icon"},

		// R-259 and R-343 … R-346: the AI functions, from every surface.
		{"pando_list_ai_functions", `{}`, "GET", "/ai/functions"},
		{"pando_assign_ai_function", `{"function":"search_audit","adapter_id":"ai_openai"}`, "PUT", "/ai/functions/search_audit"},
		{"pando_unassign_ai_function", `{"function":"search_audit"}`, "DELETE", "/ai/functions/search_audit"},
		{"pando_ai_draft_access", `{"description":"release managers"}`, "POST", "/ai/access/draft"},
		{"pando_ai_draft_host_rules", `{"description":"no exec"}`, "POST", "/ai/policy/draft"},
		{"pando_ai_search_audit", `{"question":"who deleted what?"}`, "POST", "/ai/audit/search"},
		{"pando_ai_ask_reference", `{"question":"how do I make a group?"}`, "POST", "/ai/reference/answer"},

		// R-341: favorites, from every surface.
		{"pando_favorite_app", `{"app_id":"app_01HQ8"}`, "PUT", "/me/favorites/app_01HQ8"},
		{"pando_unfavorite_app", `{"app_id":"app_01HQ8"}`, "DELETE", "/me/favorites/app_01HQ8"},

		// R-342 and renaming: every surface (R-261).
		{"pando_rename_app", `{"app_id":"app_01HQ8","name":"Notes"}`, "PATCH", "/apps/app_01HQ8"},
		{"pando_list_my_apps", `{}`, "GET", "/me/apps"},
		{"pando_get_config", `{}`, "GET", "/config"},

		// Issue #53: updates and the in-place upgrade's plan and outcome.
		{"pando_get_updates", `{}`, "GET", "/updates"},
		{"pando_plan_upgrade", `{"version":"0.4.0"}`, "GET", "/upgrade?version=0.4.0"},
		{"pando_get_last_upgrade", `{}`, "GET", "/upgrade/last"},
		{"pando_list_audit", `{"principal_id":"usr_1","target_kind":"app","since":"2026-09-21T00:00:00Z"}`, "GET",
			"/audit?principal_id=usr_1&since=2026-09-21T00%3A00%3A00Z&target_kind=app"},
		{"pando_list_audit", `{"principal_kind":"system","target_id":"app_1","until":"2026-09-22T00:00:00Z","before":"41"}`, "GET",
			"/audit?before=41&principal_kind=system&target_id=app_1&until=2026-09-22T00%3A00%3A00Z"},
		{"pando_list_audit", `{"involving":"usr_1"}`, "GET", "/audit?involving=usr_1"},
		{"pando_list_audit_archives", `{}`, "GET", "/audit/archives"},

		// Issue #50: event subscriptions from every surface (R-261).
		{"pando_list_events", `{}`, "GET", "/events"},
		{"pando_list_subscriptions", `{}`, "GET", "/subscriptions"},
		{"pando_list_subscriptions", `{"app_id":"app_01HQ8"}`, "GET", "/subscriptions?app_id=app_01HQ8"},
		{"pando_create_subscription", `{"events":"deploy.*","url":"https://example.com/hook"}`, "POST", "/subscriptions"},
		{"pando_update_subscription", `{"subscription_id":"sub_01","enabled":"false"}`, "PATCH", "/subscriptions/sub_01"},
		{"pando_delete_subscription", `{"subscription_id":"sub_01"}`, "DELETE", "/subscriptions/sub_01"},
		{"pando_test_subscription", `{"subscription_id":"sub_01"}`, "POST", "/subscriptions/sub_01/test"},
		{"pando_list_deliveries", `{"subscription_id":"sub_01"}`, "GET", "/subscriptions/sub_01/deliveries"},
		{"pando_get_delivery", `{"subscription_id":"sub_01","delivery_id":"dlv_01"}`, "GET", "/subscriptions/sub_01/deliveries/dlv_01"},
		{"pando_redeliver", `{"subscription_id":"sub_01","delivery_id":"dlv_01"}`, "POST", "/subscriptions/sub_01/deliveries/dlv_01/redeliver"},
		{"pando_get_notification_preferences", `{}`, "GET", "/notification-preferences"},
		{"pando_list_app_events", `{"app_id":"app_01HQ8"}`, "GET", "/apps/app_01HQ8/events"},
		{"pando_list_notifications", `{"unread":"true"}`, "GET", "/me/notifications?unread=true"},
		{"pando_mark_notifications_read", `{}`, "POST", "/me/notifications/read"},
		{"pando_mark_notifications_read", `{"notification_id":"ntf_01"}`, "POST", "/me/notifications/ntf_01/read"},
		{"pando_set_notification_preference", `{"kind":"app_shared","channel":"ntf_smtp","enabled":"true"}`, "PUT", "/notification-preferences"},
		{"pando_create_section", `{"name":"Work"}`, "POST", "/me/sections"},
		{"pando_list_user_apps", `{"user_id":"usr_01"}`, "GET", "/users/usr_01/apps"},
		{"pando_get_usage", `{"app_id":"app_01HQ8"}`, "GET", "/apps/app_01HQ8/usage"},
		{"pando_rename_section", `{"section_id":"sect_01","name":"Office"}`, "PATCH", "/me/sections/sect_01"},
		{"pando_delete_section", `{"section_id":"sect_01"}`, "DELETE", "/me/sections/sect_01"},
		{"pando_add_app_to_section", `{"section_id":"sect_01","app_id":"app_01HQ8"}`, "PUT", "/me/sections/sect_01/apps/app_01HQ8"},
		{"pando_remove_app_from_section", `{"section_id":"sect_01","app_id":"app_01HQ8"}`, "DELETE", "/me/sections/sect_01/apps/app_01HQ8"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			srv, s := newSession()
			s.run(t, srv, call(1, tc.tool, tc.args))

			require.Len(t, s.calls, 1)
			require.Equal(t, tc.method, s.calls[0].method)
			require.Equal(t, tc.path, s.calls[0].path)
		})
	}
}

func TestCreateAppSendsTheSourceItWasGiven(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_create_app",
		`{"name":"notes","source_url":"https://github.com/ben/notes","ref":"main"}`))

	require.Len(t, s.calls, 1)
	require.Equal(t, "POST", s.calls[0].method)
	require.Equal(t, "/apps", s.calls[0].path)

	body, err := json.Marshal(s.calls[0].body)
	require.NoError(t, err)
	require.JSONEq(t,
		`{"name":"notes","source":{"type":"git","url":"https://github.com/ben/notes","ref":"main"}}`,
		string(body))
}

func TestAnswerDetectionSendsOneAnswer(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_answer_detection",
		`{"app_id":"app_01HQ8","key":"port","answer":"3000"}`))

	body, err := json.Marshal(s.calls[0].body)
	require.NoError(t, err)
	require.JSONEq(t, `{"answers":{"port":"3000"}}`, string(body))
}

// Retrying with the same key must not deploy twice, and a deploy with no key
// must not send an empty one.
func TestDeploySendsAnIdempotencyKeyOnlyWhenGivenOne(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_deploy", `{"app_id":"app_01HQ8","idempotency_key":"agent-run-7"}`))
	body, err := json.Marshal(s.calls[0].body)
	require.NoError(t, err)
	require.JSONEq(t, `{"idempotency_key":"agent-run-7"}`, string(body))

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_deploy", `{"app_id":"app_01HQ8"}`))
	body, err = json.Marshal(s.calls[0].body)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(body))
}

// The ID comes from an agent, which means it comes from a model, which means it
// can be anything at all.
func TestAnAppIDFromAModelIsEscapedIntoThePath(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_get_app", `{"app_id":"../../policy"}`))

	require.Len(t, s.calls, 1)
	require.NotContains(t, s.calls[0].path, "../")
	require.Equal(t, "/apps/..%2F..%2Fpolicy", s.calls[0].path)
}

func TestAMissingOrWrongTypedArgumentIsReportedToTheAgent(t *testing.T) {
	for _, args := range []string{`{}`, `{"app_id":""}`, `{"app_id":null}`, `{"app_id":123}`} {
		srv, s := newSession()
		replies := s.run(t, srv, call(1, "pando_get_app", args))

		require.Contains(t, text(t, replies[0]), "app_id", args)
		require.True(t, result(t, replies[0])["isError"].(bool), args)
		require.Empty(t, s.calls, "a malformed call does not reach the API")
	}
}

func TestCreateAppReportsEachMissingArgumentByName(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, call(1, "pando_create_app", `{"source_url":"https://example/x"}`))
	require.Contains(t, text(t, replies[0]), "name is required")

	srv, s = newSession()
	replies = s.run(t, srv, call(1, "pando_create_app", `{"name":"notes"}`))
	require.Contains(t, text(t, replies[0]), "give exactly one of source_url, image, or upload")
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

// An image app and an upload app can be created from MCP, as from the API
// (R-261, issue #41).
func TestR261_CreateAppTakesAnImageOrAnUpload(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_create_app",
		`{"name":"web","image":"ghcr.io/acme/web:1","registry_credential":{"kind":"basic","username":"ben","password":"tok"}}`))
	require.Len(t, s.calls, 1)
	require.Contains(t, jsonOf(t, s.calls[0].body), `"type":"image"`)
	require.Contains(t, jsonOf(t, s.calls[0].body), `"credential":{`)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_create_app", `{"name":"site","upload":true}`))
	require.Len(t, s.calls, 1)
	require.Contains(t, jsonOf(t, s.calls[0].body), `"type":"upload"`)

	srv, s = newSession()
	replies := s.run(t, srv, call(1, "pando_create_app", `{"name":"both","image":"nginx","source_url":"https://x/y"}`))
	require.True(t, result(t, replies[0])["isError"].(bool))
	require.Empty(t, s.calls)
}

// A wait that is not a whole number of seconds is refused before it reaches
// the API, saying what is valid.
func TestGetDetectionRefusesAnUnusableWait(t *testing.T) {
	for _, args := range []string{
		`{"app_id":"app_01HQ8","wait_seconds":-1}`,
		`{"app_id":"app_01HQ8","wait_seconds":1.5}`,
		`{"app_id":"app_01HQ8","wait_seconds":"30"}`,
	} {
		srv, s := newSession()
		replies := s.run(t, srv, call(1, "pando_get_detection", args))
		require.Contains(t, text(t, replies[0]), "wait_seconds must be a whole number of seconds", args)
		require.Empty(t, s.calls, "a malformed call does not reach the API")
	}
}

// A tail that is not a whole number of lines is refused before it reaches the
// API, saying what is valid.
func TestGetLogsRefusesAnUnusableTail(t *testing.T) {
	for _, args := range []string{
		`{"app_id":"app_01HQ8","tail":0}`,
		`{"app_id":"app_01HQ8","tail":2.5}`,
		`{"app_id":"app_01HQ8","tail":"50"}`,
	} {
		srv, s := newSession()
		replies := s.run(t, srv, call(1, "pando_get_logs", args))
		require.Contains(t, text(t, replies[0]), "tail must be a whole number of lines", args)
		require.Empty(t, s.calls, "a malformed call does not reach the API")
	}
}

func TestAnswerDetectionReportsEachMissingArgumentByName(t *testing.T) {
	for args, want := range map[string]string{
		`{"app_id":"app_01HQ8","answer":"3000"}`: "key is required",
		`{"app_id":"app_01HQ8","key":"port"}`:    "answer is required",
	} {
		srv, s := newSession()
		replies := s.run(t, srv, call(1, "pando_answer_detection", args))
		require.Contains(t, text(t, replies[0]), want)
	}
}

// An agent can read and act on a tool result; a JSON-RPC error is a transport
// failure and most clients surface it as one. "You do not have permission to do
// this" is information the agent should get, not a broken connection.
func TestAnAPIFailureIsAToolResultRatherThanATransportError(t *testing.T) {
	srv, s := newSession()
	s.err = errs.New(errs.PermDenied, "You do not have permission to deploy this app.")

	replies := s.run(t, srv, call(1, "pando_deploy", `{"app_id":"app_01HQ8"}`))

	require.NotContains(t, replies[0], "error", "not a transport failure")
	require.True(t, result(t, replies[0])["isError"].(bool))

	// The API's own message, which is held to the R-105 standard and written to
	// be pasted into an assistant. An agent is exactly that reader, so rewriting
	// it here would discard the thing it was written for.
	require.Contains(t, text(t, replies[0]), "You do not have permission to deploy this app.")
}

// Whatever the caller's error renders is what the agent reads — the remedy
// included. In the binary, Call is the CLI's client, whose APIError prints the
// remedy under the message.
func TestARemedyReachesTheAgentAlongWithTheMessage(t *testing.T) {
	srv, s := newSession()
	s.err = errors.New("You do not have permission to deploy this app.\n" +
		"Ask the app's owner for the deployer role.")

	replies := s.run(t, srv, call(1, "pando_deploy", `{"app_id":"app_01HQ8"}`))
	require.Contains(t, text(t, replies[0]), "Ask the app's owner for the deployer role.")
}

func TestAPlainErrorFromTheAPIStillReachesTheAgent(t *testing.T) {
	srv, s := newSession()
	s.err = errors.New("connection refused")

	replies := s.run(t, srv, call(1, "pando_list_apps", `{}`))
	require.Contains(t, text(t, replies[0]), "connection refused")
}

func TestASuccessfulCallReturnsTheAPIsJSONVerbatim(t *testing.T) {
	srv, s := newSession()
	s.result = `{"apps":[{"id":"app_01HQ8","name":"notes","state":"running"}]}`

	replies := s.run(t, srv, call(1, "pando_list_apps", `{}`))
	require.JSONEq(t, s.result, text(t, replies[0]))
}

// A tools/call result can carry a whole app spec, and the default scanner
// buffer of 64 KiB would truncate one.
func TestALineLargerThanTheDefaultScannerBufferIsRead(t *testing.T) {
	srv, s := newSession()

	padding := strings.Repeat("a", 200*1024)
	replies := s.run(t, srv, call(1, "pando_get_app", `{"app_id":"app_01HQ8","ignored":"`+padding+`"}`))

	require.Len(t, replies, 1)
	require.Len(t, s.calls, 1, "the request was read whole rather than truncated")
}

func TestRequestsAreAnsweredInOrderOverOneStream(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv,
		rpc(1, "initialize", `{}`),
		rpc(2, "tools/list", ""),
		call(3, "pando_list_apps", `{}`),
		rpc(4, "ping", ""))

	require.Len(t, replies, 4)
	for i, reply := range replies {
		require.EqualValues(t, i+1, reply["id"])
		require.Equal(t, "2.0", reply["jsonrpc"])
	}
}

// TestR340_SetAppIconSendsTheDecodedBytes asserts the MCP half of R-340: the
// agent sends base64, and the API receives the file itself — not JSON.
func TestR340_SetAppIconSendsTheDecodedBytes(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_set_app_icon", `{"app_id":"app_01HQ8","image_base64":"iVBORw=="}`))

	require.Len(t, s.calls, 1)
	raw, ok := s.calls[0].body.(mcp.Bytes)
	require.True(t, ok, "the body should be sent as bytes, not encoded as JSON")
	require.Equal(t, []byte{0x89, 'P', 'N', 'G'}, raw.Data)
}

func TestSetAppIconRefusesBadBase64WithoutCallingTheAPI(t *testing.T) {
	srv, s := newSession()
	replies := s.run(t, srv, call(1, "pando_set_app_icon", `{"app_id":"app_01HQ8","image_base64":"not base64!"}`))

	require.Empty(t, s.calls)
	require.Equal(t, true, result(t, replies[0])["isError"])
}
