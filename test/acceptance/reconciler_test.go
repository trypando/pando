//go:build integration

package acceptance_test

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Phase 7's "done when": killing a container by hand restores it, and killing
// it repeatedly reaches failed and stays there.
//
// Against the shipped stack with its real reconciler loop running, because the
// interesting part is not the algorithm — that has unit coverage — but whether
// the loop is actually running, actually observing, and actually converging on
// a machine where someone can reach in and break things.

// containersFor returns the running container IDs for an app's bundle.
func containersFor(t *testing.T, appID string) []string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-q",
		"--filter", "label=io.pando.app="+appID).Output()
	require.NoError(t, err)
	return strings.Fields(string(out))
}

// deployedApp brings up a real app and returns its ID.
func deployedApp(t *testing.T, c *client, name string) string {
	t.Helper()

	app := c.createApp(t, name)
	c.putSpec(t, app, fmt.Sprintf(`{
		"schema_version": 1,
		"source": {"type": "image", "image": "nginx:1.27-alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"image": "nginx:1.27-alpine", "ports": [{"number": 80, "protocol": "http", "source": "user"}]}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": %d},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`, 9300+time.Now().Second()%50))
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 0)
	final := c.awaitDeployment(t, app, dep["id"].(string), 10*time.Minute)
	require.Equal(t, "succeeded", final["status"], c.deploymentLogs(t, app, dep["id"].(string)))

	require.Len(t, containersFor(t, app), 1)
	return app
}

// awaitState polls until the app reaches one of the given states.
func awaitState(t *testing.T, c *client, appID string, within time.Duration, states ...string) string {
	t.Helper()
	deadline := time.Now().Add(within)

	var last string
	for time.Now().Before(deadline) {
		last = c.get(t, "/apps/"+appID)["state"].(string)
		for _, s := range states {
			if last == s {
				return last
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("app %s stayed in %q, never reached %v", appID, last, states)
	return ""
}

// R-148: drift the reconciler can correct, it corrects.
func TestR148_AKilledContainerIsRestored(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "rec-restore-"+stamp())

	before := containersFor(t, app)
	require.Len(t, before, 1)

	// Reach in and break it, exactly as a person would.
	require.NoError(t, exec.Command("docker", "rm", "-f", before[0]).Run())
	require.Empty(t, containersFor(t, app))

	// Wait for the container, not for the state. The app's state is still
	// `running` the instant after the container is killed — Pando has not
	// looked yet — so waiting on state would pass without the reconciler
	// having done anything at all.
	var after []string
	require.Eventually(t, func() bool {
		after = containersFor(t, app)
		return len(after) == 1
	}, 3*time.Minute, 2*time.Second, "the reconciler never brought the container back")

	require.NotEqual(t, before[0], after[0], "it is a new container, not the old one resurrected")
	require.Equal(t, "running", awaitState(t, c, app, 2*time.Minute, "running"),
		"and it settles back to running once the replacement is observed healthy")
}

// R-150 and R-151: an app that keeps dying reaches failed, and stays there.
//
// A crash-looping container is the ordinary shape of "killing it repeatedly":
// the workload exists, it has exited, Pando recreates it, it exits again. Apply
// succeeds every time — the container really is created — which is exactly the
// case that showed the failure counter was measuring the wrong thing.
//
// The second half is the one that matters. `failed` is terminal because there
// is no code path out of it, and the only way to show that is to wait past
// every backoff interval and find nothing has happened.
func TestR151_ACrashLoopingAppReachesFailedAndStaysThere(t *testing.T) {
	c := login(t)

	app := c.createApp(t, "rec-failed-"+stamp())
	c.putSpec(t, app, `{
		"schema_version": 1,
		"source": {"type": "image", "image": "alpine:3.20"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"image": "alpine:3.20", "command": ["sh", "-c", "exit 1"],
			"ports": [{"number": 80, "protocol": "http", "source": "user"}]}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9399},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 0)
	c.awaitDeployment(t, app, dep["id"].(string), 10*time.Minute)

	// Sized from the schedule the stack is actually running, rather than from
	// the production numbers. At those, this one test is forty minutes of a
	// forty-three minute suite — and it is asserting the state machine, not the
	// durations. See PANDO_RECONCILER_BACKOFF in the README.
	reachFailed, stayFailed := crashLoopDeadlines()

	state := awaitState(t, c, app, reachFailed, "failed")
	require.Equal(t, "failed", state)

	// And stays. The window is past every retry interval the loop has, so if
	// anything were still trying, it would have tried by now.
	deadline := time.Now().Add(stayFailed)
	for time.Now().Before(deadline) {
		require.Equal(t, "failed", c.get(t, "/apps/"+app)["state"],
			"a failed app stays failed until a person intervenes (R-151)")
		time.Sleep(stayFailed / 6)
	}
}

// crashLoopDeadlines works out how long to wait from the schedule the server is
// running, read from the same environment variables that configured it.
//
// Derived rather than hardcoded so the test cannot quietly pass for the wrong
// reason: with a compressed schedule a fixed 40-minute deadline would still
// pass, but it would also pass if the give-up rule had stopped working and the
// app reached failed by some other route.
func crashLoopDeadlines() (reachFailed, stayFailed time.Duration) {
	schedule := []time.Duration{0, 5 * time.Second, 15 * time.Second, 60 * time.Second, 5 * time.Minute}
	if raw := os.Getenv("PANDO_RECONCILER_BACKOFF"); raw != "" {
		var parsed []time.Duration
		ok := true
		for _, part := range strings.Split(raw, ",") {
			d, err := time.ParseDuration(strings.TrimSpace(part))
			if err != nil {
				ok = false
				break
			}
			parsed = append(parsed, d)
		}
		if ok && len(parsed) > 0 {
			schedule = parsed
		}
	}

	threshold := 10
	if raw := os.Getenv("PANDO_RECONCILER_FAILURE_THRESHOLD"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			threshold = n
		}
	}

	// The sum of the first `threshold` steps, extending the last one.
	var total time.Duration
	for i := 0; i < threshold; i++ {
		step := schedule[len(schedule)-1]
		if i < len(schedule) {
			step = schedule[i]
		}
		total += step
	}

	// Double it, plus the reconcile tick and the time each attempt takes to
	// actually fail. Generous, because a flaky deadline in this test is worse
	// than a slow one.
	reachFailed = 2*total + 2*time.Minute
	stayFailed = schedule[len(schedule)-1]*2 + 30*time.Second
	return reachFailed, stayFailed
}

// Design 05 §2.1.1: Pando stopping does not stop apps, and coming back does not
// restart what never stopped.
func TestPandoRestartingLeavesARunningAppAlone(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "rec-survive-"+stamp())

	before := containersFor(t, app)
	require.Len(t, before, 1)

	out, err := execCompose("restart", "pando")
	require.NoError(t, err, out)

	// Back when it answers HTTP, not when its container accepts an exec: the
	// container runs well before Pando has migrated and is listening, and the
	// next request then found nothing there — which the suite rightly reads as
	// the server gone, failing every test after this one.
	require.Eventually(t, func() bool {
		resp, err := http.Get(strings.TrimSuffix(baseURL(), "/api/v1") + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Minute, time.Second, "Pando never answered again after restarting")

	// Same container, still running: it kept serving while Pando was away, and
	// Pando converged to it rather than restarting it for tidiness.
	require.Equal(t, before, containersFor(t, app))

	c2 := login(t)
	require.Equal(t, "running", awaitState(t, c2, app, 2*time.Minute, "running"))
	require.Equal(t, before, containersFor(t, app))
}

// TestR204_DeletingAnAppTearsDownItsBundleButKeepsVolumes asserts the leak that
// went unnoticed through ten phases.
//
// Nothing ever called RuntimeAdapter.Destroy. Deleting an app archived the row
// and left its containers running on a private network nobody reclaimed — and
// Pando takes one network per app against a Docker pool that holds about
// thirty, so an install that adds and removes apps eventually cannot start one.
// The failure arrives as a message about subnets, on an unrelated deploy, long
// after the deletion that caused it.
//
// The other half is R-204: volumes outlive the app. This is the one place a bug
// silently destroys data somebody explicitly chose to keep, so the test asserts
// both directions.
func TestR204_DeletingAnAppTearsDownItsBundleButKeepsVolumes(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "teardown-"+stamp())

	require.Len(t, containersFor(t, app), 1, "the app is running before it is deleted")
	networks := networksFor(t, app)
	require.NotEmpty(t, networks, "and has a private network of its own (R-025)")

	_, status := c.do(t, "DELETE", "/apps/"+app+"?force=true", "")
	require.Equal(t, 204, status)

	// The GC is nudged rather than waited for, and the nudge has to come first.
	//
	// It runs hourly by default, so a three-minute wait for it to come round on
	// its own can only pass on an install that configured a short interval —
	// which is a test that passes on the runner's environment rather than on
	// the code. Recreating the server runs a collection pass at startup, which
	// is what makes this assert "teardown happens at all" — the thing that
	// never used to — instead of asserting a clock.
	//
	// The recreate is needed for the network half regardless. The network is
	// reclaimed when Pando's container is next *recreated* — not when it
	// restarts, and not while it serves.
	//
	// Pando is joined to every bundle network because that is how the proxy
	// reaches an app (R-023), so removing one means disconnecting the running
	// container — and on Docker Desktop that drops its published ports.
	// Measured: healthz 200, disconnect, 000, and still 000 until the container
	// was recreated, all while Pando served happily inside it. A janitor that
	// can take the server off the network is worse than the leak it reclaims.
	//
	// A restart is not enough because a restarted container keeps its network
	// memberships. A recreated one does not exist yet, so the network it held
	// has no endpoints and Docker removes it with nothing to disconnect.
	//
	// The honest cost, stated in the phase file too: networks belonging to apps
	// deleted since the last recreate are held until the next one. The
	// containers — which hold the memory and CPU — are gone on the next
	// collection, which is the half that matters while the install is running.
	recreatePando(t)

	require.Eventually(t, func() bool {
		return len(containersFor(t, app)) == 0
	}, 3*time.Minute, 5*time.Second, "a deleted app's containers must not keep running")

	require.Eventually(t, func() bool {
		return len(networksFor(t, app)) == 0
	}, 2*time.Minute, 5*time.Second,
		"a deleted app's network must be reclaimed when Pando is recreated")

	// And the server is still reachable afterwards, which is the property the
	// whole design above exists to preserve.
	resp, err := http.Get(strings.TrimSuffix(baseURL(), "/api/v1") + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// recreatePando replaces the server's container and waits for it to answer.
func recreatePando(t *testing.T) {
	t.Helper()

	// The port the stack was brought up on, carried through explicitly.
	//
	// `docker compose up` reads PANDO_PORT from the environment and falls back
	// to 8080, so a recreate from a shell that does not have it moves the
	// server to a port the rest of this suite is not talking to — and if
	// anything else on the machine holds 8080, the container never starts at
	// all. That failure looks like Pando crashing on recreate, which is a long
	// way from where it is.
	cmd := exec.Command("docker", "compose", "up", "-d", "--force-recreate", "pando")
	cmd.Env = append(os.Environ(), "PANDO_PORT="+portOf(t, baseURL()))

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	require.Eventually(t, func() bool {
		resp, err := http.Get(strings.TrimSuffix(baseURL(), "/api/v1") + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Minute, 2*time.Second, "Pando did not come back after being recreated")
}

// portOf is the host port the suite is talking to.
func portOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	if port := u.Port(); port != "" {
		return port
	}
	return "8080"
}

// networksFor returns the bundle networks Docker still holds for an app.
func networksFor(t *testing.T, appID string) []string {
	t.Helper()
	out, err := exec.Command("docker", "network", "ls", "-q",
		"--filter", "label=io.pando.bundle="+appID).Output()
	require.NoError(t, err)
	return strings.Fields(string(out))
}

// TestR222_ADeployedWorkloadHasItsLogsCapped asserts O-16's resolution reaches
// a real container.
//
// R-222 says retention is bounded by size so a chatty app cannot fill a disk
// shared with twenty others. Until now nothing carried the cap: the spec had
// retention.log_bytes, the planner ignored it, and the container was created
// with the daemon's default, which is unbounded.
//
// Asserted against Docker's own view of the container rather than against the
// plan, because the plan saying "100 MB" and the daemon doing nothing about it
// is exactly the failure this closes.
func TestR222_ADeployedWorkloadHasItsLogsCapped(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "logcap-"+stamp())

	containers := containersFor(t, app)
	require.Len(t, containers, 1)

	out, err := exec.Command("docker", "inspect", containers[0],
		"--format", "{{.HostConfig.LogConfig.Type}} {{index .HostConfig.LogConfig.Config \"max-size\"}} {{index .HostConfig.LogConfig.Config \"max-file\"}}").Output()
	require.NoError(t, err)

	fields := strings.Fields(string(out))
	require.Len(t, fields, 3, "the container must have a log configuration: %q", string(out))
	require.Equal(t, "json-file", fields[0])
	require.NotEmpty(t, fields[1], "a size cap, or a chatty app fills the host")

	// Two files, not one. Docker rotates before deleting, so a cap of N with a
	// single file keeps somewhere between 0 and N bytes — and 0 is what you get
	// exactly when you most want to read why something failed.
	require.Equal(t, "2", fields[2])
}
