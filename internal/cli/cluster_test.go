package cli_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR414_ClusterCommandsCallTheirEndpoints asserts R-414 and R-415 with
// R-261: the cluster's hosts and join tokens are reachable from the CLI, each
// subcommand the one API call it names.
func TestR414_ClusterCommandsCallTheirEndpoints(t *testing.T) {
	api := newAPI(t).
		reply("GET /cluster/hosts", map[string]any{"hosts": []map[string]any{
			{"id": "host_1", "name": "app-1", "roles": []string{"app"}, "address": "10.0.0.2:7443", "version": "0.16.0", "last_seen_at": "2026-10-26T12:00:00Z"},
			{"id": "host_2", "name": "app-2", "roles": []string{"control", "app"}, "address": "10.0.0.3:7443"},
		}}).
		reply("GET /cluster/join-tokens", map[string]any{"tokens": []map[string]any{
			{"id": "jt_1", "roles": []string{"app"}, "max_uses": 2, "uses": 1, "expires_at": "2026-10-27T12:00:00Z", "revoked_at": "2026-10-26T13:00:00Z"},
		}}).
		reply("POST /cluster/join-tokens", map[string]any{
			"token": map[string]any{"id": "jt_2", "roles": []string{"app"}, "max_uses": 3, "expires_at": "2026-10-27T12:00:00Z"},
			"join":  "pj1.aHR0cHM6Ly9wYW5kbw.hash.jt_2.secret",
		})

	got := run(t, api, "", "cluster", "hosts", "list")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "app-1")
	require.Contains(t, got.out, "2026-10-26T12:00:00Z")
	require.Contains(t, got.out, "never", "a host not yet heard from")
	require.Contains(t, got.out, "control,app")

	got = run(t, api, "", "cluster", "tokens", "list")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "1/2")
	require.Contains(t, got.out, "2026-10-26T13:00:00Z")

	got = run(t, api, "", "cluster", "tokens", "create", "--role", "app", "--uses", "3", "--expires-hours", "48", "--address", "https://pando.example.com")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "pj1.aHR0cHM6Ly9wYW5kbw.hash.jt_2.secret")
	require.Contains(t, got.out, "Shown once.")
	require.JSONEq(t, `{"roles":["app"],"max_uses":3,"expires_hours":48,"address":"https://pando.example.com"}`, api.bodyFor("POST /cluster/join-tokens"))

	got = run(t, api, "", "cluster", "hosts", "remove", "host_1")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Removed the host.")
	api.bodyFor("DELETE /cluster/hosts/host_1")

	got = run(t, api, "", "cluster", "tokens", "revoke", "jt_1")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "Revoked the join token.")
	api.bodyFor("DELETE /cluster/join-tokens/jt_1")
}

func TestClusterCommandsReportTheServersRefusal(t *testing.T) {
	api := newAPI(t)
	for _, key := range []string{
		"GET /cluster/hosts", "DELETE /cluster/hosts/host_1", "GET /cluster/join-tokens",
		"POST /cluster/join-tokens", "DELETE /cluster/join-tokens/jt_1",
	} {
		api.fail(key, http.StatusForbidden, map[string]string{"code": "PERM_VERB_REQUIRED", "message": "You need install.cluster.manage."})
	}
	for _, args := range [][]string{
		{"hosts", "list"}, {"hosts", "remove", "host_1"}, {"tokens", "list"}, {"tokens", "create"}, {"tokens", "revoke", "jt_1"},
	} {
		got := run(t, api, "", "cluster", args...)
		require.Error(t, got.err, args)
	}
}
