package cli_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/cli"
)

func readyDetection() map[string]any {
	return map[string]any{
		"status":    "ready",
		"detection": map[string]any{"winning_bid": map[string]string{"detector": "Node.js", "strategy": "buildpack"}},
	}
}

// TestR261_DetectionAnswerWaitsForTheRebuild asserts R-261 for issue #211:
// the CLI answers detection's questions, as the console and MCP do. An answer
// that changes what is built has Pando build and scan again, and the command
// waits for that as `--wait` does, then exits by the outcome.
func TestR261_DetectionAnswerWaitsForTheRebuild(t *testing.T) {
	api := newAPI(t).
		reply("POST /apps/app_01HQ8/detection/answers", map[string]any{"status": "running", "stage": "building"}).
		handle("GET /apps/app_01HQ8/detection", sequence(
			map[string]any{"status": "running", "stage": "building", "running": []string{"building", "scanning"}},
			readyDetection(),
		))

	got := run(t, api, "", "app", "detection", "answer", "app_01HQ8", "build_arg_NODE_ENV", "production")

	require.NoError(t, got.err, "ready to accept")
	require.JSONEq(t, `{"answers":{"build_arg_NODE_ENV":"production"}}`,
		api.bodyFor("POST /apps/app_01HQ8/detection/answers"))
	require.Contains(t, got.errOut, "Building the image...")
	require.Contains(t, got.out, "Ready: recognized it as Node.js, built with buildpack.")
	require.Contains(t, got.out, "`pando app detection accept app_01HQ8`")
}

// A question left open after an answer exits 2, and each open question is
// printed verbatim (R-105) with the command that answers it.
func TestR261_DetectionAnswerSaysWhatIsLeft(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/detection", map[string]any{
		"status": "needs_answers",
		"detection": map[string]any{"questions": []map[string]any{
			{"key": "name", "prompt": "Answered."},
			{"key": "port", "prompt": portQuestion},
		}},
		"unanswered": []string{"port"},
	})

	got := run(t, api, "", "app", "detection", "answer", "app_01HQ8", "name", "notes")

	require.Equal(t, 2, cli.ExitCodeOf(got.err))
	require.Contains(t, got.out, portQuestion)
	require.Contains(t, got.out, "Answer: pando app detection answer app_01HQ8 port <answer>")
	require.NotContains(t, got.out, "Answered.")
}

// The server's refusal of an answer is shown as it wrote it.
func TestDetectionAnswerReportsARefusal(t *testing.T) {
	api := newAPI(t).fail("POST /apps/app_01HQ8/detection/answers", 400, map[string]string{
		"code": "VALID_INVALID", "message": "There are no outstanding questions with these keys: [prot].",
	})
	got := run(t, api, "", "app", "detection", "answer", "app_01HQ8", "prot", "3000")
	require.ErrorContains(t, got.err, "There are no outstanding questions with these keys: [prot].")
	require.False(t, api.sawPath("/apps/app_01HQ8/detection?wait=30"), "nothing to wait for")
}

// TestR261_DetectionReviseSendsTheMessageAndPrintsTheReply asserts R-261 for
// issue #211: the CLI can tell the AI adapter what is wrong with the plan, and
// shows what it replied, changed and was refused (R-334).
func TestR261_DetectionReviseSendsTheMessageAndPrintsTheReply(t *testing.T) {
	ready := readyDetection()
	ready["detection"].(map[string]any)["conversation"] = []map[string]any{
		{"from": "person", "text": "It serves on 8080."},
		{"from": "ai", "text": "The server listens on PORT, defaulting to 8080.",
			"changes": []string{"Set the port to 8080."},
			"refused": []string{"Mounting the Docker socket: Pando never gives a build the runtime socket."}},
	}
	api := newAPI(t).reply("POST /apps/app_01HQ8/detection/revise", ready)

	got := run(t, api, "", "app", "detection", "revise", "app_01HQ8", "It", "serves", "on", "8080.")

	require.NoError(t, got.err)
	require.JSONEq(t, `{"message":"It serves on 8080."}`, api.bodyFor("POST /apps/app_01HQ8/detection/revise"))
	require.Contains(t, got.out, "The server listens on PORT, defaulting to 8080.")
	require.Contains(t, got.out, "Changed: Set the port to 8080.")
	require.Contains(t, got.out, "Refused: Mounting the Docker socket")
	require.Contains(t, got.out, "Ready: recognized it as Node.js")
}

// TestR261_DetectionDiffListsEachChangeByClass asserts R-261 for issue #211:
// the CLI shows what accepting would change, each change classed as the API
// classes it, and says when there is nothing pinned to compare with.
func TestR261_DetectionDiffListsEachChangeByClass(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/detection/diff", map[string]any{
		"pinned": 3, "is_first": false,
		"changes": map[string]any{"changes": []map[string]any{
			{"class": "restart", "path": "workloads.web.port", "summary": "The web workload's port changes from 3000 to 8080."},
			{"class": "destructive", "path": "volumes.data", "summary": "The data volume is removed, and what it holds is lost."},
		}},
	})
	got := run(t, api, "", "app", "detection", "diff", "app_01HQ8")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "Accepting would change revision 3:")
	require.Contains(t, got.out, "restart      The web workload's port changes from 3000 to 8080.")
	require.Contains(t, got.out, "destructive  The data volume is removed")
	require.Contains(t, got.out, "`pando app detection accept app_01HQ8 --confirm`")

	api = newAPI(t).reply("GET /apps/app_01HQ8/detection/diff", map[string]any{
		"pinned": 3, "is_first": false, "changes": map[string]any{"changes": nil},
	})
	got = run(t, api, "", "app", "detection", "diff", "app_01HQ8")
	require.Contains(t, got.out, "Accepting changes nothing: the proposal matches revision 3.")

	api = newAPI(t).reply("GET /apps/app_01HQ8/detection/diff", map[string]any{
		"pinned": nil, "changes": nil, "is_first": true,
	})
	got = run(t, api, "", "app", "detection", "diff", "app_01HQ8")
	require.Contains(t, got.out, "This app is not configured yet")
}

// TestR261_DetectionRerunQueuesAndCanWait asserts R-261 for issue #211:
// re-running detection is a command of its own, explicit as R-022 requires.
func TestR261_DetectionRerunQueuesAndCanWait(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "", "app", "detection", "rerun", "app_01HQ8")
	require.NoError(t, got.err)
	require.True(t, api.sawPath("/apps/app_01HQ8/detection/rerun"))
	require.Contains(t, got.out, "`pando app detection app_01HQ8 --wait`")
	require.False(t, api.sawPath("/apps/app_01HQ8/detection?wait=30"), "no wait unless asked")

	api = newAPI(t).reply("GET /apps/app_01HQ8/detection", map[string]any{
		"status": "failed",
		"detection": map[string]any{"error": map[string]string{
			"code": "STATE_INVALID", "message": "The repository could not be cloned.",
		}},
	})
	got = run(t, api, "", "app", "detection", "rerun", "app_01HQ8", "--wait")
	require.Equal(t, 1, cli.ExitCodeOf(got.err))
	require.Contains(t, got.out, "The repository could not be cloned.")
	require.Contains(t, got.out, "`pando app detection rerun app_01HQ8 --wait`")
}

// TestR261_DetectionAcceptSendsWhatTheAPITakes asserts R-261 for issue #211:
// accepting from the CLI sets what the console's review sets — variables,
// secrets, optional dependencies and the hostname — in the one call.
func TestR261_DetectionAcceptSendsWhatTheAPITakes(t *testing.T) {
	api := newAPI(t).reply("POST /apps/app_01HQ8/detection/accept", map[string]any{
		"app_id": "app_01HQ8", "revision": 1, "state": "proposed", "deployed": false,
	})

	got := run(t, api, "", "app", "detection", "accept", "app_01HQ8",
		"--env", "API_URL=https://api", "--secret", "TOKEN=s3cret",
		"--optional", "redis", "--hostname", "notes.example.com")

	require.NoError(t, got.err)
	var sent map[string]any
	require.NoError(t, json.Unmarshal([]byte(api.bodyFor("POST /apps/app_01HQ8/detection/accept")), &sent))
	require.Equal(t, []any{
		map[string]any{"key": "API_URL", "value": "https://api"},
		map[string]any{"key": "TOKEN", "value": "s3cret", "secret": true},
	}, sent["values"])
	require.Equal(t, []any{"redis"}, sent["optional"])
	require.Equal(t, "notes.example.com", sent["hostname"])
	require.Equal(t, false, sent["confirm"])
	require.Contains(t, got.out, "Accepted as revision 1. Nothing is deployed yet.")
	require.Contains(t, got.out, "`pando deploy app_01HQ8`")

	got = run(t, api, "", "app", "detection", "accept", "app_01HQ8", "--env", "NOEQUALS")
	require.ErrorContains(t, got.err, `--env takes KEY=VALUE, and "NOEQUALS" has no =`)
}

// Accepting over a configured app is refused by the server until confirmed,
// and the CLI names the flag that confirms it rather than the API field.
func TestDetectionAcceptOverAConfiguredAppNeedsConfirm(t *testing.T) {
	api := newAPI(t).fail("POST /apps/app_01HQ8/detection/accept", 400, map[string]any{
		"code":    "VALID_INVALID",
		"message": "This app is already configured, and accepting again replaces how it is built and what it runs with what detection found.",
		"remedy":  "Send confirm: true to go ahead.",
		"details": map[string]any{"pinned_spec_id": "spec_01HQ8"},
	})
	got := run(t, api, "", "app", "detection", "accept", "app_01HQ8")
	require.ErrorContains(t, got.err, "This app is already configured")
	require.ErrorContains(t, got.err, "add --confirm to go ahead")
	require.NotContains(t, got.err.Error(), "confirm: true")

	api = newAPI(t).reply("POST /apps/app_01HQ8/detection/accept", map[string]any{"revision": 4})
	got = run(t, api, "", "app", "detection", "accept", "app_01HQ8", "--confirm")
	require.NoError(t, got.err)
	require.Contains(t, api.bodyFor("POST /apps/app_01HQ8/detection/accept"), `"confirm":true`)
}

// The detection command still reads an app's detection: the subcommands
// do not swallow an app ID.
func TestAppDetectionStillTakesAnApp(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_01HQ8/detection", readyDetection())
	got := run(t, api, "", "app", "detection", "app_01HQ8")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "Ready: recognized it as Node.js")
}
