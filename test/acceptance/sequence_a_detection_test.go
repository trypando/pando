//go:build integration

package acceptance_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Sequence A, against real public repositories, over HTTP, against the shipped
// Compose stack. This is phase 6's "done when".
//
// The repositories are the shapes the phase names: a Dockerfile app, a compose
// stack, a static site, a Node app with no deployment artifacts, and a
// monorepo. They are the same ones the detection corpus uses, so a divergence
// between what the auction does in-process and what the running system does
// shows up as a failure here rather than as a surprise later.

func (c *client) createAppFromSource(t *testing.T, name, url, ref, subdir string) string {
	t.Helper()
	body, status := c.do(t, http.MethodPost, "/apps", fmt.Sprintf(
		`{"name":%q,"source":{"type":"git","url":%q,"ref":%q,"subdir":%q}}`,
		name, url, ref, subdir))
	require.Equal(t, http.StatusAccepted, status, body)

	var app map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &app))

	appID := app["id"].(string)
	cleanupBundle(t, appID)
	return appID
}

// awaitDetection polls until detection reaches a terminal status.
//
// Detection is queued at app creation and runs in the background, because a
// clone plus a trial run takes longer than any reasonable HTTP timeout. So the
// client polls, exactly as the console does.
func (c *client) awaitDetection(t *testing.T, appID string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		body, status := c.do(t, http.MethodGet, "/apps/"+appID+"/detection", "")
		if status == http.StatusOK {
			var out map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &out))
			if s, _ := out["status"].(string); s != "" && s != "running" {
				return out
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("detection did not finish for %s", appID)
	return nil
}

func detectionBody(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(response["detection"])
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func questionKeys(t *testing.T, detection map[string]any) []string {
	t.Helper()
	raw, _ := detection["questions"].([]any)

	var keys []string
	for _, q := range raw {
		if m, ok := q.(map[string]any); ok {
			if deferred, _ := m["deferred"].(bool); deferred {
				continue
			}
			keys = append(keys, m["key"].(string))
		}
	}
	return keys
}

func TestSequenceA_ADockerfileApp(t *testing.T) {
	c := login(t)
	appID := c.createAppFromSource(t, "seq-a-dockerfile-"+stamp(),
		"https://github.com/docker/welcome-to-docker", "main", "")

	response := c.awaitDetection(t, appID)
	require.Equal(t, "ready", response["status"], "a Dockerfile at the root leaves nothing to ask")

	detection := detectionBody(t, response)
	winner := detection["winning_bid"].(map[string]any)
	require.Equal(t, "dockerfile", winner["strategy"])
	require.GreaterOrEqual(t, winner["confidence"].(float64), 0.8)

	// R-093: the user sees the auction, not a verdict.
	require.Contains(t, detection, "runners_up")

	// R-120: Ref is what was asked for, Commit is what was read.
	require.Len(t, response["commit"].(string), 40)

	// Accepting pins revision 1 and does not deploy.
	c.acceptDetection(t, appID)
	app := c.get(t, "/apps/"+appID)
	require.Equal(t, "proposed", app["state"], "accepting a proposal is not deploying it")

	deployments := c.get(t, "/apps/"+appID+"/deployments")
	require.Empty(t, deployments["deployments"], "nothing was deployed")

	// Sequence A: exactly two grant rows, one per plane (R-073).
	grants := c.get(t, "/apps/"+appID+"/grants")
	require.Len(t, grants["grants"], 2)
}

func TestSequenceA_AComposeStack(t *testing.T) {
	c := login(t)
	appID := c.createAppFromSource(t, "seq-a-compose-"+stamp(),
		"https://github.com/docker/awesome-compose", "master", "react-express-mysql")

	response := c.awaitDetection(t, appID)
	detection := detectionBody(t, response)

	winner := detection["winning_bid"].(map[string]any)
	require.Equal(t, "compose", winner["strategy"],
		"a compose file is a complete answer, not a hint (R-096)")

	// Which service is the app's one canonical endpoint is not something the
	// file says, so it is asked rather than picked (R-026, R-102).
	require.Equal(t, []string{"primary_service"}, questionKeys(t, detection))
	require.Equal(t, "needs_answers", response["status"])

	draft := detection["draft_spec"].(map[string]any)
	require.NotEmpty(t, draft["volumes"], "a declared volume is imported and honored (R-200)")

	// An answer to a question nobody asked is a typo, and reported as one.
	body, status := c.postRaw(t, "/apps/"+appID+"/detection/answers",
		`{"answers":{"primray_service":"web"}}`)
	require.Equal(t, http.StatusBadRequest, status, body)

	body, status = c.postRaw(t, "/apps/"+appID+"/detection/answers",
		`{"answers":{"primary_service":"backend"}}`)
	require.Equal(t, http.StatusOK, status, body)

	c.acceptDetection(t, appID)

	pinned := c.get(t, "/apps/"+appID+"/specs/1")
	spec := pinned["body"].(map[string]any)
	var primary map[string]any
	for _, w := range spec["workloads"].([]any) {
		if m := w.(map[string]any); m["primary"] == true {
			primary = m
		}
	}
	require.NotNil(t, primary)
	require.Equal(t, "backend", primary["name"], "the answer decided which one, not Pando")
}

func TestSequenceA_AStaticSite(t *testing.T) {
	c := login(t)
	appID := c.createAppFromSource(t, "seq-a-static-"+stamp(),
		"https://github.com/h5bp/html5-boilerplate", "main", "")

	response := c.awaitDetection(t, appID)
	detection := detectionBody(t, response)

	winner := detection["winning_bid"].(map[string]any)
	require.Equal(t, "static", winner["strategy"],
		"a committed dist/ is build output that is already there, not a reason to run a buildpack")
}

func TestSequenceA_ANodeAppWithNoDeploymentArtifacts(t *testing.T) {
	c := login(t)
	appID := c.createAppFromSource(t, "seq-a-node-"+stamp(),
		"https://github.com/expressjs/express", "master", "")

	// Ready, not needs_answers. This used to ask which port the app serves
	// on; since issue #55 (bc0dd0d) a buildpack app's port is the language's
	// usual one, marked as assumed, and the deploy watches the built image
	// start and records the port it really binds — R-097: "port discovery
	// happens by observing what the process binds, not by asking". Nothing
	// in express since this test was written touches what detection reads.
	response := c.awaitDetection(t, appID)
	require.Equal(t, "ready", response["status"])

	detection := detectionBody(t, response)
	winner := detection["winning_bid"].(map[string]any)
	require.Equal(t, "buildpack", winner["strategy"],
		"this is the case R-103 is really about: written by someone who never thought about deployment")

	draft := detection["draft_spec"].(map[string]any)
	workloads := draft["workloads"].([]any)
	require.NotEmpty(t, workloads)
	port := workloads[0].(map[string]any)["ports"].([]any)[0].(map[string]any)
	require.Equal(t, "framework", port["source"],
		"the port is marked as assumed, which is what makes the deploy check it against the built image (R-097)")
	require.Contains(t, fmt.Sprint(winner["evidence"]), "assumed to serve HTTP",
		"and the person reading the proposal is told it is an assumption")

	// R-105 is a content requirement. Every question must be answerable by
	// something that cannot see the repository, because the workflow is pasting
	// it into the assistant that wrote the app.
	for _, raw := range detection["questions"].([]any) {
		q := raw.(map[string]any)
		prompt := q["prompt"].(string)
		require.Greater(t, len(prompt), 60, "a fragment is not a self-contained question")
		require.NotEmpty(t, q["why"], "the console shows this beside the question")
		require.True(t,
			strings.Contains(prompt, "Valid answer") || strings.Contains(prompt, "for example"),
			"a question that never says what a valid answer looks like makes an assistant guess the format: %q", prompt)
	}
}

func TestSequenceA_AMonorepo(t *testing.T) {
	c := login(t)
	appID := c.createAppFromSource(t, "seq-a-monorepo-"+stamp(),
		"https://github.com/vercel/turbo", "main", "")

	response := c.awaitDetection(t, appID)
	require.Equal(t, "needs_answers", response["status"])

	detection := detectionBody(t, response)
	require.Equal(t, "unknown", detection["winning_bid"].(map[string]any)["strategy"],
		"several deployable things in one repository: Pando asks which, rather than picking")

	// R-105 again, and the part that matters here: the question names the
	// candidates it found, because the reader cannot see the repository.
	questions := detection["questions"].([]any)
	require.Len(t, questions, 1)
	q := questions[0].(map[string]any)
	require.NotEmpty(t, q["options"], "a choice with nothing to choose from is not a question")

	// Accepting is refused while a question a person must answer is open.
	body, status := c.postRaw(t, "/apps/"+appID+"/detection/accept", "")
	require.Equal(t, http.StatusConflict, status, body)
	require.Contains(t, body, "unanswered")
}

// R-092: a blocked source produces zero disk writes. git clone is never
// invoked, which is why the check runs before the fetch rather than inside it.
//
// Proving the absence of a clone from outside the process is the hard part —
// checkouts go to a temp directory and are removed afterwards, so counting them
// later cannot tell a refused fetch from a finished one. What can be observed
// is ordering: detection marks itself `running` the moment it begins, and it
// does that after the allowlist check. So a refused re-detection leaves the
// previous result untouched, and a check that had been inside the fetch would
// have reset it before failing.
func TestSequenceA_ABlockedSourceClonesNothing(t *testing.T) {
	c := login(t)

	// An app that detected successfully while the source was still allowed.
	appID := c.createAppFromSource(t, "seq-a-blocked-"+stamp(),
		"https://github.com/docker/welcome-to-docker", "main", "")
	before := c.awaitDetection(t, appID)
	require.Equal(t, "ready", before["status"])

	setHostPolicy(t, `{"allow_anonymous_grants": true, "min_build_isolation": 10, `+
		`"min_runtime_isolation": 10, "source_allowlist": ["github.corp.example"]}`)
	t.Cleanup(func() {
		setHostPolicy(t, `{"allow_anonymous_grants": true, "min_build_isolation": 10, `+
			`"min_runtime_isolation": 10}`)
	})

	// Creating a new app from a now-blocked source is refused outright.
	body, status := c.do(t, http.MethodPost, "/apps", fmt.Sprintf(
		`{"name":%q,"source":{"type":"git","url":"https://github.com/docker/welcome-to-docker","ref":"main"}}`,
		"seq-a-blocked-2-"+stamp()))
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "POLICY_SOURCE_NOT_ALLOWED")

	// So is re-detecting the existing one (R-022 makes this explicit, and the
	// allowlist can have changed since the app was created).
	body, status = c.postRaw(t, "/apps/"+appID+"/detection/rerun", "")
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "POLICY_SOURCE_NOT_ALLOWED")

	after := c.get(t, "/apps/"+appID+"/detection")
	require.Equal(t, before["status"], after["status"],
		"a refused re-detection never started, so it cannot have cloned anything")
	require.Equal(t, before["commit"], after["commit"])
	require.Equal(t, before["updated_at"], after["updated_at"],
		"nothing was written at all — not even the row saying detection had begun")
}

// setHostPolicy writes the host policy document directly.
func setHostPolicy(t *testing.T, body string) {
	t.Helper()
	out, err := execCompose("exec", "-T", "postgres", "psql", "-U", "pando", "-d", "pando", "-c",
		fmt.Sprintf("UPDATE host_policy SET body = %s::jsonb, updated_at = now() WHERE id = 1",
			quoteSQL(body)))
	require.NoError(t, err, out)

	// The policy evaluator caches; give it a moment to pick the change up.
	time.Sleep(2 * time.Second)
}

func quoteSQL(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func execCompose(args ...string) (string, error) {
	out, err := exec.Command("docker", append([]string{"compose"}, args...)...).CombinedOutput()
	return string(out), err
}

func (c *client) acceptDetection(t *testing.T, appID string) {
	t.Helper()
	body, status := c.postRaw(t, "/apps/"+appID+"/detection/accept", "")
	require.Equal(t, http.StatusOK, status, body)

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Equal(t, float64(1), out["revision"], "a detected proposal pins as revision 1")
	require.Equal(t, false, out["deployed"])
}

// The seam between Sequence A and Sequence B: a repository, detected, accepted,
// and actually deployed.
//
// This test exists because its absence hid a real gap. Sequence A ends at
// pinning revision 1 and asserts that accepting does not deploy; Sequence B
// deploys a hand-written spec. Both passed while the spec that detection
// produced could not be planned at all — it described the app and said nothing
// about which runtime, which routing mode, or what limits, so the very first
// plan-time check refused it. "Accepting does not deploy" and "what was pinned
// can be deployed" are different claims, and only the first was being tested.
//
// R-104 is the rule the fix implements: questions are blockers, everything else
// is configuration, and configuration gets a default.
func TestSequenceAtoB_ADetectedRepositoryDeploysAndServes(t *testing.T) {
	c := login(t)

	appID := c.createAppFromSource(t, "seq-ab-"+stamp(),
		"https://github.com/docker/welcome-to-docker", "main", "")

	response := c.awaitDetection(t, appID)
	require.Equal(t, "ready", response["status"])
	c.acceptDetection(t, appID)

	// The dry run is the check that was failing. It is side-effect-free, so the
	// console calls it on every spec edit — and a detected spec has to pass it
	// without anyone editing anything.
	body, status := c.postRaw(t, "/apps/"+appID+"/plan", "")
	require.Equal(t, http.StatusOK, status, body)

	var plan map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	checks := plan["checks"].(map[string]any)
	for _, name := range []string{"spec_valid", "policy", "adapters", "capabilities", "isolation", "slots", "capacity"} {
		require.Equal(t, "ok", checks[name], "plan check %q", name)
	}

	// Every field detection could not learn from the repository got a default
	// rather than a question (R-104).
	pinned := c.get(t, "/apps/"+appID+"/specs/1")
	s := pinned["body"].(map[string]any)

	routing := s["routing"].(map[string]any)
	require.NotEmpty(t, routing["adapter_ref"])
	require.NotEmpty(t, routing["mode"])
	require.Equal(t, "adapter_default", routing["mode_source"],
		"R-163: the console shows whether a mode was inherited or chosen")

	require.NotEmpty(t, s["runtime"].(map[string]any)["adapter_ref"])
	require.NotEmpty(t, s["build"].(map[string]any)["adapter_ref"])
	require.Equal(t, "recreate", s["deploy"].(map[string]any)["strategy"], "R-144")
	require.Greater(t, s["resources"].(map[string]any)["memory_bytes"].(float64), float64(0), "R-240")
	require.Greater(t, s["retention"].(map[string]any)["spec_revisions"].(float64), float64(0), "R-152")

	// And what it did learn is still there, unchanged by any of it.
	build := s["build"].(map[string]any)
	require.Equal(t, "dockerfile", build["strategy"])
	require.Equal(t, "Dockerfile", build["dockerfile"])

	// Now deploy the thing detection proposed.
	dep := c.deploy(t, appID, 1)
	final := c.awaitDeployment(t, appID, dep["id"].(string), 10*time.Minute)
	require.Equal(t, "succeeded", final["status"],
		"the build and deploy of a spec nobody hand-wrote\n%s",
		c.deploymentLogs(t, appID, dep["id"].(string)))

	// R-023: it is reachable through Pando's proxy, which is the only way in.
	// Addressed by slug rather than by the spec's routing fields — the proxy is
	// the fallback route and resolves the app itself, so there is no separate
	// proxy port whatever mode the app is in.
	app := c.get(t, "/apps/"+appID)
	slug := app["slug"].(string)

	require.Eventually(t, func() bool {
		return c.appResponds(t, slug)
	}, 120*time.Second, 3*time.Second,
		"the deployed app never answered through the proxy")
}

// appResponds asks the app for its front page, through the proxy.
func (c *client) appResponds(t *testing.T, slug string) bool {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet,
		strings.TrimSuffix(baseURL(), "/api/v1")+"/"+slug+"/", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})

	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_ = resp.Body
	return resp.StatusCode == http.StatusOK
}
