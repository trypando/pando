package cli_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR261_TheCLICanStopAndStartAnApp asserts R-261.
//
// `POST /apps/{id}/stop` and `/start` existed with nothing calling them, so the
// only way to take an app down from a terminal was to delete it — which also
// asks what to do with its data and cannot be undone. A capability the API has
// is a capability every surface has.
func TestR261_TheCLICanStopAndStartAnApp(t *testing.T) {
	for _, action := range []string{"stop", "start", "restart"} {
		t.Run(action, func(t *testing.T) {
			api := newAPI(t)
			got := run(t, api, "", "app", action, "app_01HQ8")

			require.NoError(t, got.err, got.errOut)
			require.True(t, api.sawPath("/apps/app_01HQ8/"+action),
				"saw %v", api.calls)
		})
	}
}

// Stopping says how to undo it, because that is the difference between this
// and deleting.
func TestStoppingSaysHowToStartItAgain(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "", "app", "stop", "app_01HQ8")

	require.NoError(t, got.err)
	require.Contains(t, got.out, "pando app start app_01HQ8")
}

// TestR261_TheCLIReportsEachPartOfAnApp asserts R-261.
//
// An app can be a web service, a proxy and a database it brought with it, and
// "degraded" is one word for all of them. The console shows the parts; so does
// this, and the names it prints are what `pando logs --workload` takes.
func TestR261_TheCLIReportsEachPartOfAnApp(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/status", map[string]any{
		"state":         "degraded",
		"desired_state": "running",
		"workloads": []map[string]any{
			{
				"name": "app", "primary": false, "present": true, "running": true,
				"restarting": true, "restart_count": 14, "healthy": false, "exit_code": 1,
			},
			{
				"name": "proxy", "primary": true, "present": true, "running": true,
				"restarting": false, "restart_count": 0, "healthy": nil,
			},
		},
	})

	got := run(t, api, "", "app", "status", "app_01HQ8")
	require.NoError(t, got.err, got.errOut)

	require.Contains(t, got.out, "degraded")

	// The crash loop, named and counted. "running" is true of it at almost
	// every instant, which is the reading that makes a broken app look fine.
	line := lineWith(t, got.out, "app")
	require.Contains(t, line, "restarting")
	require.Contains(t, line, "14")
	require.Contains(t, line, "failing")

	// R-221: no health check is not a failing one.
	require.Contains(t, lineWith(t, got.out, "proxy"), "no check")
	require.Contains(t, got.out, "*", "the part the address resolves to is marked")
}

// An app whose runtime cannot be reached reports that, rather than printing a
// table of absences as though they were facts.
func TestStatusSaysWhenTheRuntimeCouldNotBeReached(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/status", map[string]any{
		"state":         "running",
		"desired_state": "running",
		"observability": "unreachable",
	})

	got := run(t, api, "", "app", "status", "app_01HQ8")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "could not be reached")
}

// Logs take a part, for the same reason the console has a picker: an app of
// three containers has three logs, and the one that is failing is not always
// the primary.
func TestLogsCanNameAPart(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/logs", "hello\n")

	got := run(t, api, "", "logs", "app_01HQ8", "--workload", "worker")
	require.NoError(t, got.err, got.errOut)
	require.True(t, api.sawPath("/apps/app_01HQ8/logs?workload=worker"), "saw %v", api.calls)
	require.Equal(t, "hello\n", got.out)
}

// A tail and a follow reach the endpoint as query parameters (O-51): the CLI
// follows the same shared stream the console does.
func TestLogsPassTailAndFollow(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/logs", "hello\n")

	got := run(t, api, "", "logs", "app_01HQ8", "-n", "50", "--follow")
	require.NoError(t, got.err, got.errOut)
	require.True(t, api.sawPath("/apps/app_01HQ8/logs?follow=true&tail=50"), "saw %v", api.calls)
}

// lineWith returns the output line naming a part.
func lineWith(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), name) {
			return line
		}
	}
	t.Fatalf("no line for %q in:\n%s", name, out)
	return ""
}
