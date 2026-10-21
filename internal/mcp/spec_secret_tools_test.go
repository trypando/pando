package mcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_SecretsAndSpecRevisionsHaveTools asserts R-261: setting a secret
// and writing, pinning and comparing spec revisions are endpoints the CLI and
// console reach, so MCP reaches them too. Without them an agent could not
// store a secret, change a configured app's variables, or narrow its egress.
func TestR261_SecretsAndSpecRevisionsHaveTools(t *testing.T) {
	requireToolCalls(t, []toolCase{
		{tool: "pando_list_secrets", args: `{"app_id":"app_1"}`, method: "GET", path: "/apps/app_1/secrets"},
		{tool: "pando_set_secret", args: `{"app_id":"app_1","key":"NTFY_TOPIC","value":"alerts"}`,
			method: "PUT", path: "/apps/app_1/secrets/NTFY_TOPIC", body: `{"value":"alerts"}`},
		{tool: "pando_delete_secret", args: `{"app_id":"app_1","key":"NTFY_TOPIC"}`,
			method: "DELETE", path: "/apps/app_1/secrets/NTFY_TOPIC"},
		{tool: "pando_list_specs", args: `{"app_id":"app_1"}`, method: "GET", path: "/apps/app_1/specs"},
		{tool: "pando_get_spec", args: `{"app_id":"app_1","revision":"3"}`, method: "GET", path: "/apps/app_1/specs/3"},
		{tool: "pando_create_spec", args: `{"app_id":"app_1","spec":{"egress":{"allow":["ntfy.sh"]}}}`,
			method: "POST", path: "/apps/app_1/specs", body: `{"egress":{"allow":["ntfy.sh"]}}`},
		{tool: "pando_create_spec", args: `{"app_id":"app_1","spec":{},"dry_run":true}`,
			method: "POST", path: "/apps/app_1/specs?dry_run=true", body: `{}`},
		{tool: "pando_pin_spec", args: `{"app_id":"app_1","revision":"4"}`, method: "POST", path: "/apps/app_1/specs/4/pin"},
		{tool: "pando_diff_specs", args: `{"app_id":"app_1","from":"3","to":"4"}`, method: "GET", path: "/apps/app_1/specs/3/diff/4"},
	})
}

// A key or revision from an agent is escaped into the path, not trusted.
func TestSecretAndSpecToolsEscapeThePath(t *testing.T) {
	requireToolCalls(t, []toolCase{
		{tool: "pando_set_secret", args: `{"app_id":"app_1","key":"../value","value":"x"}`,
			method: "PUT", path: "/apps/app_1/secrets/..%2Fvalue"},
		{tool: "pando_pin_spec", args: `{"app_id":"app_1","revision":"../../x"}`,
			method: "POST", path: "/apps/app_1/specs/..%2F..%2Fx/pin"},
	})
}

// TestR261_AcceptProposalTakesConfirmSecretsAndChoices asserts R-261: the
// accept tool sends everything the endpoint and `pando app detection accept`
// take — confirm for an app already configured, secrets, optional
// dependencies and the hostname — and leaves out what was not given.
func TestR261_AcceptProposalTakesConfirmSecretsAndChoices(t *testing.T) {
	requireToolCalls(t, []toolCase{
		{tool: "pando_accept_proposal", args: `{"app_id":"app_1"}`,
			method: "POST", path: "/apps/app_1/detection/accept", body: `{"values":[]}`},
		{tool: "pando_accept_proposal",
			args: `{"app_id":"app_1","values":{"B":"2","A":"1"},"secrets":{"NTFY_TOPIC":"t"},` +
				`"optional":["redis"],"hostname":"hn.example.com","confirm":true}`,
			method: "POST", path: "/apps/app_1/detection/accept",
			body: `{"values":[{"key":"A","value":"1"},{"key":"B","value":"2"},{"key":"NTFY_TOPIC","value":"t","secret":true}],` +
				`"optional":["redis"],"hostname":"hn.example.com","confirm":true}`},
	})
}

func TestSecretAndSpecToolsRefuseBadArgumentsWithoutCallingTheAPI(t *testing.T) {
	for _, tc := range []struct{ tool, args, want string }{
		{"pando_set_secret", `{"app_id":"app_1"}`, "key"},
		{"pando_set_secret", `{"app_id":"app_1","key":"K"}`, "value"},
		{"pando_delete_secret", `{"app_id":"app_1"}`, "key"},
		{"pando_list_secrets", `{}`, "app_id"},
		{"pando_list_specs", `{}`, "app_id"},
		{"pando_get_spec", `{"app_id":"app_1"}`, "revision"},
		{"pando_create_spec", `{}`, "app_id"},
		{"pando_create_spec", `{"app_id":"app_1"}`, "spec"},
		{"pando_create_spec", `{"app_id":"app_1","spec":"{}"}`, "spec"},
		{"pando_pin_spec", `{"app_id":"app_1"}`, "revision"},
		{"pando_diff_specs", `{"app_id":"app_1"}`, "from"},
		{"pando_diff_specs", `{"app_id":"app_1","from":"1"}`, "to"},
		{"pando_accept_proposal", `{}`, "app_id"},
		{"pando_accept_proposal", `{"app_id":"app_1","values":["A=1"]}`, "values"},
		{"pando_accept_proposal", `{"app_id":"app_1","secrets":{"K":1}}`, "secrets.K"},
		{"pando_accept_proposal", `{"app_id":"app_1","values":{"K":1}}`, "values.K"},
		{"pando_accept_proposal", `{"app_id":"app_1","optional":"redis"}`, "optional"},
		{"pando_accept_proposal", `{"app_id":"app_1","hostname":7}`, "hostname"},
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

// TestR261_DeployCanShipANewerRevision asserts R-261: the console's Deploy
// ships an edit by naming its revision, so the deploy tool can too, and a
// deploy that names none sends no revision.
func TestR261_DeployCanShipANewerRevision(t *testing.T) {
	requireToolCalls(t, []toolCase{
		{tool: "pando_deploy", args: `{"app_id":"app_1","spec_revision":5}`,
			method: "POST", path: "/apps/app_1/deployments", body: `{"spec_revision":5}`},
		{tool: "pando_deploy", args: `{"app_id":"app_1"}`,
			method: "POST", path: "/apps/app_1/deployments", body: `{}`},
	})
	for _, bad := range []string{`"5"`, `0`, `2.5`} {
		srv, s := newSession()
		replies := s.run(t, srv, call(1, "pando_deploy", `{"app_id":"app_1","spec_revision":`+bad+`}`))
		require.Empty(t, s.calls, bad)
		require.Contains(t, text(t, replies[0]), "spec_revision must be a revision number")
	}
}
