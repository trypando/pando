//go:build integration

// Sequence B — Deploy (docs/design/07-sequences.md).
//
// Runs against the real Compose topology rather than against doubles, because
// the assertions that matter here are about the topology: whether the build
// container can reach a runtime socket, and whether a failed build leaves the
// running app alone.
package acceptance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestR112_BuildContainerHasNoRuntimeSocket asserts R-112.
//
// Mounting a container runtime socket into a build is a host compromise and is
// categorically forbidden. This is the non-negotiable assertion in the risk
// register: it reads the build container's ACTUAL mount list rather than
// trusting the Compose file, because intent is what drifts.
func TestR112_BuildContainerHasNoRuntimeSocket(t *testing.T) {
	requireStack(t)

	mounts := inspect(t, buildkitContainer(t), `{{json .Mounts}}`)

	var parsed []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Type        string `json:"Type"`
	}
	require.NoError(t, json.Unmarshal([]byte(mounts), &parsed))

	for _, m := range parsed {
		for _, forbidden := range []string{"docker.sock", "containerd.sock", "crio.sock", "podman.sock"} {
			require.NotContains(t, m.Source, forbidden,
				"the build container must not have a runtime socket mounted (R-112): %s -> %s",
				m.Source, m.Destination)
			require.NotContains(t, m.Destination, forbidden,
				"the build container must not have a runtime socket mounted (R-112): %s -> %s",
				m.Source, m.Destination)
		}
	}

	// And it must not be able to reach one by any other route: no privileged
	// mode, no host network, no host PID namespace.
	require.Equal(t, "false", inspect(t, buildkitContainer(t), `{{.HostConfig.Privileged}}`),
		"the build container must not be privileged")
	require.NotEqual(t, "host", inspect(t, buildkitContainer(t), `{{.HostConfig.NetworkMode}}`),
		"the build container must not share the host network")
	require.NotEqual(t, "host", inspect(t, buildkitContainer(t), `{{.HostConfig.PidMode}}`),
		"the build container must not share the host PID namespace")
}

// TestR112_BuildContainerCannotReachDocker asserts the same property
// behaviorally: not just that no socket is mounted, but that nothing inside
// the build container can talk to a runtime.
func TestR112_BuildContainerCannotReachDocker(t *testing.T) {
	requireStack(t)

	out, _ := exec.Command("docker", "exec", buildkitContainer(t),
		"sh", "-c", "ls /var/run/docker.sock 2>&1 || echo ABSENT").CombinedOutput()
	require.Contains(t, string(out), "ABSENT",
		"no runtime socket may be reachable from inside the build container")
}

// TestSequenceB_DeployAPrebuiltImage walks the pipeline end to end.
//
// Uses a prebuilt image so the assertions are about the pipeline — route,
// apply, health, commit — rather than about a build. The build path is covered
// by the two tests above and by TestR146 below.
func TestSequenceB_DeployAPrebuiltImage(t *testing.T) {
	c := login(t)

	app := c.createApp(t, "seq-b-"+stamp())
	c.putSpec(t, app, prebuiltSpec(9101))
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 0)
	require.Equal(t, "pending", dep["status"])

	final := c.awaitDeployment(t, app, dep["id"].(string), 3*time.Minute)
	require.Equal(t, "succeeded", final["status"],
		"deploy failed: %v", final["error_detail"])

	status := c.get(t, fmt.Sprintf("/apps/%s/status", app))
	require.Equal(t, "running", status["state"])

	// R-026: the workload runs on the app's own private network, and nothing
	// is published to the host.
	container := fmt.Sprintf("pando-%s-web", app)
	ports := inspect(t, container, `{{json .NetworkSettings.Ports}}`)
	require.NotContains(t, ports, "HostPort", "no port may be published to the host")

	networks := inspect(t, container, `{{json .NetworkSettings.Networks}}`)
	require.Contains(t, networks, "pando-"+app, "the workload is on the app's own network")

	// The audit log records the deploy.
	require.Contains(t, auditActions(t), "app.deploy")
}

// TestR146_AFailedBuildLeavesTheRunningAppAlone asserts R-146.
//
// The deployment is failed; the app is not. This is why deployments are their
// own table — collapsing them would make the distinction inexpressible.
func TestR146_AFailedBuildLeavesTheRunningAppAlone(t *testing.T) {
	c := login(t)

	app := c.createApp(t, "seq-b-fail-"+stamp())
	c.putSpec(t, app, prebuiltSpec(9102))
	c.pinSpec(t, app, 1)

	first := c.deploy(t, app, 0)
	final := c.awaitDeployment(t, app, first["id"].(string), 3*time.Minute)
	require.Equal(t, "succeeded", final["status"])

	before := c.get(t, fmt.Sprintf("/apps/%s/status", app))
	require.Equal(t, "running", before["state"])

	// A second revision that cannot possibly build: a repository that does not
	// exist.
	c.putSpec(t, app, `{
		"schema_version": 1,
		"source": {"type": "git", "url": "https://github.com/pando-test/does-not-exist-`+stamp()+`", "ref": "main"},
		"build": {"strategy": "dockerfile", "adapter_ref": "bld_buildkit"},
		"workloads": [{"name": "web", "primary": true, "exposed": true}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9102},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)

	body, status := c.postRaw(t, fmt.Sprintf("/apps/%s/deployments", app), `{"spec_revision": 2}`)

	// The failure may be refused at the request (fetching the source to pin a
	// commit fails) or recorded as a failed deployment. Either is correct; what
	// must hold is that the previously running app is untouched.
	if status == 202 {
		var dep map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &dep))
		final := c.awaitDeployment(t, app, dep["id"].(string), 2*time.Minute)
		require.Equal(t, "failed", final["status"])
	} else {
		require.GreaterOrEqual(t, status, 400)
	}

	after := c.get(t, fmt.Sprintf("/apps/%s/status", app))
	require.Equal(t, "running", after["state"],
		"a failed build must leave the running app's state unchanged (R-146)")
	require.Equal(t, before["revision"], after["revision"],
		"and must not move the pinned revision")

	// The container is still the one that was there before.
	require.Equal(t, "true",
		inspect(t, fmt.Sprintf("pando-%s-web", app), `{{.State.Running}}`),
		"the previously deployed workload is still running")
}

// TestR023_RoutingPointsAtPandoNotTheWorkload asserts that a route sends traffic
// to Pando's proxy, never to the workload.
//
// The loopback adapter records the upstream it was given, and what matters is
// that the value is Pando's own address — an adapter author's instinct is to
// point at the container, and that instinct is the bug.
func TestR023_RoutingPointsAtPandoNotTheWorkload(t *testing.T) {
	c := login(t)

	app := c.createApp(t, "seq-b-route-"+stamp())
	c.putSpec(t, app, prebuiltSpec(9103))
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 0)
	final := c.awaitDeployment(t, app, dep["id"].(string), 3*time.Minute)
	require.Equal(t, "succeeded", final["status"])

	logs := c.deploymentLogs(t, app, dep["id"].(string))
	require.Contains(t, logs, "Routing traffic")

	// The workload's own address never appears as a routing destination.
	containerIP := inspect(t, fmt.Sprintf("pando-%s-web", app),
		`{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}`)
	if containerIP != "" {
		require.NotContains(t, logs, containerIP,
			"a route must never point at the workload (R-023)")
	}
}

// TestSecretsNeverAppearInBuildOrDeployOutput asserts R-194 along the deploy
// path, which is where a secret would be most likely to escape.
func TestSecretsNeverAppearInBuildOrDeployOutput(t *testing.T) {
	c := login(t)

	const canary = "hunter2-THE-ACTUAL-SECRET"
	app := c.createApp(t, "seq-b-secret-"+stamp())

	c.put(t, fmt.Sprintf("/apps/%s/secrets/API_TOKEN", app),
		fmt.Sprintf(`{"value": %q}`, canary))

	c.putSpec(t, app, `{
		"schema_version": 1,
		"source": {"type": "image", "image": "nginx:alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"env": [{"key": "API_TOKEN", "secret_ref": "API_TOKEN"}]}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9104},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 0)
	final := c.awaitDeployment(t, app, dep["id"].(string), 3*time.Minute)
	require.Equal(t, "succeeded", final["status"])

	// Not in the deploy logs.
	require.NotContains(t, c.deploymentLogs(t, app, dep["id"].(string)), canary)

	// Not in the server's own log output.
	serverLogs, _ := exec.Command("docker", "compose", "logs", "pando").CombinedOutput()
	require.NotContains(t, string(serverLogs), canary, "a secret reached the server log")

	// Not in the audit log.
	require.NotContains(t, auditDetail(t), canary, "a secret reached the audit log")

	// Not returned by the secrets listing, which is keys and metadata only.
	listing := c.get(t, fmt.Sprintf("/apps/%s/secrets", app))
	encoded, err := json.Marshal(listing)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), canary)
	require.Contains(t, string(encoded), "API_TOKEN", "the key is listed")

	// But it did reach the workload, which is the point of a secret.
	env := inspect(t, fmt.Sprintf("pando-%s-web", app), `{{json .Config.Env}}`)
	require.Contains(t, env, canary, "the secret must actually reach the app")
}

// TestR120_DeployPinsACommit asserts R-120 and design 01 §2.1: a deploy never
// resolves a ref implicitly, so pinning produces a visible revision.
func TestR120_DeployPinsACommit(t *testing.T) {
	c := login(t)

	app := c.createApp(t, "seq-b-pin-"+stamp())
	c.putSpec(t, app, `{
		"schema_version": 1,
		"source": {"type": "git", "url": "https://github.com/octocat/Hello-World", "ref": "master"},
		"build": {"strategy": "dockerfile", "adapter_ref": "bld_buildkit"},
		"workloads": [{"name": "web", "primary": true, "exposed": true}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9105},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)
	c.pinSpec(t, app, 1)

	// The deploy will fail — that repository has no Dockerfile — but pinning
	// happens first, and that is what this asserts.
	_, _ = c.postRaw(t, fmt.Sprintf("/apps/%s/deployments", app), `{}`)

	specs := c.get(t, fmt.Sprintf("/apps/%s/specs", app))
	revisions := specs["revisions"].([]any)
	require.GreaterOrEqual(t, len(revisions), 2,
		"resolving a ref must produce a new revision rather than editing one")

	newest := revisions[0].(map[string]any)
	full := c.get(t, fmt.Sprintf("/apps/%s/specs/%v", app, newest["revision"]))
	body := full["body"].(map[string]any)
	source := body["source"].(map[string]any)

	require.NotEmpty(t, source["commit"], "the new revision names the commit that will be built")
	require.Len(t, source["commit"], 40, "a full SHA")
	require.Equal(t, "master", source["ref"], "and still records what was asked for")
}

// --- helpers ---------------------------------------------------------------

func stamp() string { return time.Now().Format("150405.000") }

func buildkitContainer(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "compose", "ps", "-q", "buildkit").Output()
	require.NoError(t, err)
	id := strings.TrimSpace(string(out))
	require.NotEmpty(t, id, "the buildkit service is not running")
	return id
}

func inspect(t *testing.T, container, format string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", format, container).CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func auditActions(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "compose", "exec", "-T", "postgres",
		"psql", "-U", "pando", "-d", "pando", "-tAc", "SELECT action FROM audit_events").CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func auditDetail(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "compose", "exec", "-T", "postgres",
		"psql", "-U", "pando", "-d", "pando", "-tAc", "SELECT detail::text FROM audit_events").CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

// TestSequenceB_BuildFromSource is the phase's headline path: source in, image
// out, app running.
//
// Kept separate from the prebuilt tests and skippable, because it clones a real
// repository and runs a real build — minutes rather than seconds, and dependent
// on the network. Set PANDO_TEST_BUILD_REPO to build something else.
func TestSequenceB_BuildFromSource(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a real repository; skipped in short mode")
	}
	c := login(t)

	repo := os.Getenv("PANDO_TEST_BUILD_REPO")
	if repo == "" {
		repo = "https://github.com/docker/welcome-to-docker"
	}

	app := c.createApp(t, "seq-b-build-"+stamp())
	c.putSpec(t, app, fmt.Sprintf(`{
		"schema_version": 1,
		"source": {"type": "git", "url": %q, "ref": "main"},
		"build": {"strategy": "dockerfile", "adapter_ref": "bld_buildkit"},
		"workloads": [{"name": "web", "primary": true, "exposed": true}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9200},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`, repo))
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 0)
	final := c.awaitDeployment(t, app, dep["id"].(string), 15*time.Minute)
	require.Equal(t, "succeeded", final["status"],
		"build failed: %v\n%s", final["error_detail"], c.deploymentLogs(t, app, dep["id"].(string)))

	logs := c.deploymentLogs(t, app, dep["id"].(string))
	require.Contains(t, logs, "Building")
	require.Contains(t, logs, "exporting to docker image format",
		"the image comes back to Pando rather than going to a registry (R-111)")

	status := c.get(t, fmt.Sprintf("/apps/%s/status", app))
	require.Equal(t, "running", status["state"])

	// R-120: the ref was resolved into a new revision carrying the commit, and
	// the deploy built that. A deploy never resolves a ref implicitly.
	require.Equal(t, float64(2), status["revision"],
		"pinning the commit produced a revision")

	specs := c.get(t, fmt.Sprintf("/apps/%s/specs/2", app))
	source := specs["body"].(map[string]any)["source"].(map[string]any)
	require.Len(t, source["commit"], 40)
	require.Equal(t, "main", source["ref"])

	// The built image is named for the app it belongs to, which is also the
	// per-app build cache namespace (R-117). Image references cannot carry an
	// underscore, so the app ID's is replaced.
	//
	// The container runs the image by its ID — a deployment records exactly
	// the image it ran, so a refused build can never be restored in its place
	// (R-146) — and the name is on the image's tags.
	image := inspect(t, fmt.Sprintf("pando-%s-web", app), `{{.Config.Image}}`)
	tags := inspect(t, image, `{{json .RepoTags}}`)
	expected := strings.ReplaceAll(strings.ToLower(app), "_", "-")
	require.Contains(t, strings.ToLower(tags), expected,
		"the built image is named for the app it belongs to")
}
