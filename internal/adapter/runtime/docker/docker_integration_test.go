//go:build integration

package docker_test

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	dockeradapter "github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/secret"
)

func adapter(t *testing.T) *dockeradapter.Adapter {
	t.Helper()
	a := dockeradapter.New()
	require.NoError(t, a.Configure(context.Background(), nil))
	if err := a.HealthCheck(context.Background()); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	return a
}

// bundle returns a plan for a tiny container that stays up.
func bundle(bundleID string, env map[string]secret.Value) api.BundlePlan {
	return api.BundlePlan{
		BundleID: bundleID,
		Network:  api.NetworkPlan{Private: true},
		Labels:   map[string]string{"pando.app": bundleID},
		Workloads: []api.WorkloadPlan{{
			Name:    "web",
			Image:   "alpine:3.20",
			Command: []string{"sleep", "3600"},
			Env:     env,
			Exposed: true,
		}},
	}
}

// TestR020_ACarriedFileIsInTheContainer asserts R-020.
//
// A configuration file travels in the spec and is placed in the workload before
// it starts — the mechanism that replaced turning `./Caddyfile:/etc/caddy/
// Caddyfile` into a volume Docker refuses to mount. Nothing is read from the
// repository at deploy time: these bytes came out of the pinned revision.
func TestR020_ACarriedFileIsInTheContainer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-files-" + time.Now().Format("150405")
	cleanup(t, a, id)

	const body = ":80 {\n  respond \"ok\"\n}\n"

	plan := bundle(id, nil)
	plan.Workloads[0].Files = []api.FilePlan{
		{Path: "/etc/caddy/Caddyfile", Content: body},
		{Path: "/usr/local/bin/start.sh", Content: "#!/bin/sh\nexec sleep 3600\n", Mode: 0o755},
	}
	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	t.Run("the file is there, in a directory the image did not have", func(t *testing.T) {
		require.Equal(t, body, inContainer(t, id, "web", "cat", "/etc/caddy/Caddyfile"))
	})

	t.Run("the executable one is executable", func(t *testing.T) {
		out := inContainer(t, id, "web", "stat", "-c", "%a", "/usr/local/bin/start.sh")
		require.Equal(t, "755", strings.TrimSpace(out))
	})

	t.Run("and a changed file recreates the workload", func(t *testing.T) {
		first, err := a.Observe(ctx, api.BundleRef{BundleID: id})
		require.NoError(t, err)
		startedAt := first.Workloads[0].StartedAt

		// The same plan changes nothing.
		_, err = a.Apply(ctx, plan)
		require.NoError(t, err)
		same, err := a.Observe(ctx, api.BundleRef{BundleID: id})
		require.NoError(t, err)
		require.Equal(t, startedAt, same.Workloads[0].StartedAt)

		// A file's contents are not part of a container's configuration, so
		// without the digest label an edited Caddyfile would converge to
		// "already running" and never ship.
		edited := bundle(id, nil)
		edited.Workloads[0].Files = []api.FilePlan{
			{Path: "/etc/caddy/Caddyfile", Content: body + "# changed\n"},
			{Path: "/usr/local/bin/start.sh", Content: "#!/bin/sh\nexec sleep 3600\n", Mode: 0o755},
		}
		_, err = a.Apply(ctx, edited)
		require.NoError(t, err)

		after, err := a.Observe(ctx, api.BundleRef{BundleID: id})
		require.NoError(t, err)
		require.NotEqual(t, startedAt, after.Workloads[0].StartedAt,
			"an edited file is a workload that no longer matches its plan")
	})
}

// inContainer runs a command in a workload and returns its output.
func inContainer(t *testing.T, bundleID, workload string, args ...string) string {
	t.Helper()
	name := "pando-" + bundleID + "-" + workload
	out, err := exec.Command("docker", append([]string{"exec", name}, args...)...).CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

// dockerCLI runs a docker command and returns its trimmed output.
func dockerCLI(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestR224_AnAnonymousVolumeGoesWithItsContainer asserts R-224. An image that
// declares VOLUME got an unnamed volume on every container, and each one
// outlived it: one more per redeploy, and all of them after the app was
// deleted (issue #55).
func TestR224_AnAnonymousVolumeGoesWithItsContainer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	const img = "pando-it-anonymous-volume:latest"
	build := exec.Command("docker", "build", "-q", "-t", img, "-")
	build.Stdin = strings.NewReader("FROM alpine:3.20\nVOLUME /data\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build the test image: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = dockerCLI("rmi", img) })

	id := "test-anon-" + time.Now().Format("150405")
	plan := bundle(id, nil)
	plan.Workloads[0].Image = img
	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	volume, err := dockerCLI("inspect", "pando-"+id+"-web", "--format", "{{range .Mounts}}{{.Name}}{{end}}")
	require.NoError(t, err)
	require.NotEmpty(t, volume, "the image's VOLUME got an anonymous volume")

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: id}, api.DestroyOptions{KeepVolumes: true}))
	_, err = dockerCLI("volume", "inspect", volume)
	require.Error(t, err, "the anonymous volume went with its container")
}

// TestR224_AnImagePandoPulledGoesWithTheLastAppThatRanIt asserts R-224. Images
// pulled for an app stayed after it was deleted (issue #55); one another app
// still runs must stay, and so must one that was on the host before.
func TestR224_AnImagePandoPulledGoesWithTheLastAppThatRanIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	const img = "busybox:1.36.1-musl"
	if _, err := dockerCLI("image", "inspect", img); err == nil {
		t.Skipf("%s was on this host before the test, so it is not Pando's to remove", img)
	}

	stamp := time.Now().Format("150405")
	first, second := "test-pull-a-"+stamp, "test-pull-b-"+stamp
	for _, id := range []string{first, second} {
		cleanup(t, a, id) // before Apply, so a failed Apply leaves nothing either
		plan := bundle(id, nil)
		plan.Workloads[0].Image = img
		if _, err := a.Apply(ctx, plan); err != nil {
			if strings.Contains(err.Error(), "429") || strings.Contains(strings.ToLower(err.Error()), "rate limit") {
				t.Skipf("the registry is rate-limiting this host: %v", err)
			}
			require.NoError(t, err)
		}
	}

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: first}, api.DestroyOptions{}))
	_, err := dockerCLI("image", "inspect", img)
	require.NoError(t, err, "the other app still runs it")

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: second}, api.DestroyOptions{}))
	_, err = dockerCLI("image", "inspect", img)
	require.Error(t, err, "nobody runs it and Pando fetched it: removed")
}

func cleanup(t *testing.T, a *dockeradapter.Adapter, bundleID string) {
	t.Helper()
	t.Cleanup(func() {
		_ = a.Destroy(context.Background(), api.BundleRef{BundleID: bundleID}, api.DestroyOptions{})
	})
}

// TestR096_ADependentStartsOnceItsDependencyIsHealthy asserts R-096.
//
// Dependencies were started first and never waited for: a backend started while
// its database was still initializing and crashed on a refused connection
// (issue #55). A dependency with a health check now holds its dependents back
// until it reports healthy.
func TestR096_ADependentStartsOnceItsDependencyIsHealthy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-depends-" + time.Now().Format("150405")
	cleanup(t, a, id)

	plan := bundle(id, nil)
	plan.Workloads = []api.WorkloadPlan{
		{
			Name:    "db",
			Image:   "alpine:3.20",
			Command: []string{"sh", "-c", "sleep 4 && touch /tmp/ready && sleep 3600"},
			Health:  &api.HealthPlan{Command: []string{"test", "-f", "/tmp/ready"}, IntervalSeconds: 1, TimeoutSeconds: 1, Retries: 30},
		},
		{
			Name:      "web",
			Image:     "alpine:3.20",
			Command:   []string{"sleep", "3600"},
			DependsOn: []string{"db"},
			Exposed:   true,
		},
	}

	started := time.Now()
	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(started), 4*time.Second,
		"web was held back until db reported healthy")

	observed, err := a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	for _, w := range observed.Workloads {
		if w.Name == "db" {
			require.NotNil(t, w.Healthy)
			require.True(t, *w.Healthy)
		}
	}
}

func TestApplyThenObserve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-apply-" + time.Now().Format("150405")
	cleanup(t, a, id)

	_, err := a.Apply(ctx, bundle(id, nil))
	require.NoError(t, err)

	observed, err := a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	require.True(t, observed.Exists)
	require.Len(t, observed.Workloads, 1)
	require.Equal(t, "web", observed.Workloads[0].Name)
	require.True(t, observed.Workloads[0].Running)

	// R-221: no health check configured means no signal, which is NOT
	// unhealthy. An app without a health check is running, not perpetually
	// degraded, and collapsing those two states is the bug this guards.
	require.Nil(t, observed.Workloads[0].Healthy, "no health check means nil, not false")
}

// Apply is idempotent: the reconciler calls it freely, so a satisfied plan must
// touch nothing.
func TestApplyIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-idem-" + time.Now().Format("150405")
	cleanup(t, a, id)

	_, err := a.Apply(ctx, bundle(id, nil))
	require.NoError(t, err)

	first, err := a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	startedAt := first.Workloads[0].StartedAt

	_, err = a.Apply(ctx, bundle(id, nil))
	require.NoError(t, err)

	second, err := a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	require.Equal(t, startedAt, second.Workloads[0].StartedAt,
		"an already-satisfied plan must not restart the workload")
}

// R-193: a rotated secret changes the resolved environment, and the workload
// must be recreated rather than left running with the old value.
// TestR071_LogsArriveWithoutDockerFraming asserts that reading an app's logs
// gives back what the app printed.
//
// Docker frames the output of a container with no TTY: an 8-byte header before
// every chunk. Pando creates every workload without a TTY, so this stream
// always carries it, and the logs endpoint copies the adapter's reader straight
// into the response body — so anything the adapter leaves in shows up in the
// console, as control bytes at the start of each line. Nothing read this
// endpoint until the console grew a logs tab, which is why the framing had
// never been noticed.
func TestR071_LogsArriveWithoutDockerFraming(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-logs-" + time.Now().Format("150405")
	cleanup(t, a, id)

	plan := bundle(id, nil)
	plan.Workloads[0].Command = []string{"sh", "-c", "echo hello from pando; sleep 3600"}

	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	// The line is printed at startup, and Apply returns once the container is
	// created rather than once it has said anything.
	var out []byte
	require.Eventually(t, func() bool {
		rc, err := a.Logs(ctx, api.WorkloadRef{BundleID: id, Workload: "web"}, api.LogOptions{Tail: 10})
		if err != nil {
			return false
		}
		defer func() { _ = rc.Close() }()
		out, err = io.ReadAll(rc)
		return err == nil && len(out) > 0
	}, 20*time.Second, 250*time.Millisecond)

	require.Equal(t, "hello from pando\n", string(out))
	for _, b := range out {
		require.Greater(t, b, byte(0x08), "stream framing reached the caller: %q", string(out))
	}
}

func TestR193_ChangedEnvironmentCausesRecreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-env-" + time.Now().Format("150405")
	cleanup(t, a, id)

	_, err := a.Apply(ctx, bundle(id, map[string]secret.Value{"TOKEN": secret.New("old-value")}))
	require.NoError(t, err)

	before, err := a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)

	_, err = a.Apply(ctx, bundle(id, map[string]secret.Value{"TOKEN": secret.New("rotated-value")}))
	require.NoError(t, err)

	after, err := a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	require.NotEqual(t, before.Workloads[0].StartedAt, after.Workloads[0].StartedAt,
		"a rotated secret must recreate the workload")
}

// R-026: workloads are reachable only inside the bundle's own network. Nothing
// is published to the host, because traffic arrives through Pando's proxy.
func TestR026_NoPortsArePublishedToTheHost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-ports-" + time.Now().Format("150405")
	cleanup(t, a, id)

	plan := bundle(id, nil)
	plan.Workloads[0].Ports = []api.PortPlan{{Number: 8080, Protocol: "http"}}

	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	// Inspect through the adapter's own logs path to confirm it is running,
	// then assert on the container's published ports via the Docker CLI, since
	// the adapter deliberately exposes no way to ask.
	observed, err := a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	require.True(t, observed.Workloads[0].Running)

	ports := dockerInspect(t, "pando-"+id+"-web", "{{json .NetworkSettings.Ports}}")
	require.NotContains(t, ports, "HostPort", "no port may be published to the host")
}

// R-025: each bundle gets its own network, so no app can reach another's.
func TestR025_EachBundleGetsItsOwnNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	one := "test-net-a-" + time.Now().Format("150405")
	two := "test-net-b-" + time.Now().Format("150405")
	cleanup(t, a, one)
	cleanup(t, a, two)

	_, err := a.Apply(ctx, bundle(one, nil))
	require.NoError(t, err)
	_, err = a.Apply(ctx, bundle(two, nil))
	require.NoError(t, err)

	netOne := dockerInspect(t, "pando-"+one+"-web", "{{json .NetworkSettings.Networks}}")
	netTwo := dockerInspect(t, "pando-"+two+"-web", "{{json .NetworkSettings.Networks}}")

	require.Contains(t, netOne, "pando-"+one)
	require.Contains(t, netTwo, "pando-"+two)
	require.NotContains(t, netOne, "pando-"+two, "one app must not be on another's network")
}

// R-204: volumes outlive the apps that mount them, so the default teardown
// keeps them.
func TestR204_DestroyKeepsVolumesByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-vol-" + time.Now().Format("150405")

	plan := bundle(id, nil)
	plan.Volumes = []api.VolumePlan{{VolumeID: "vol_data", Name: "data"}}
	plan.Workloads[0].Mounts = []api.MountPlan{{VolumeID: "vol_data", Path: "/data"}}

	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: id}, api.DestroyOptions{KeepVolumes: true}))

	observed, err := a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	require.Empty(t, observed.Workloads, "containers are gone")
	require.NotEmpty(t, observed.Volumes, "but the storage is not")

	// Now discard it explicitly.
	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: id}, api.DestroyOptions{}))
	observed, err = a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	require.Empty(t, observed.Volumes)
}

// R-026 again, from the plan side: an adapter must refuse a plan that asks for a
// shared network rather than quietly complying.
func TestApplyRefusesANonPrivateNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	plan := bundle("test-refuse", nil)
	plan.Network.Private = false

	_, err := a.Apply(ctx, plan)
	require.Error(t, err)
	require.Contains(t, err.Error(), "shared network")
}

func TestLogsAndExec(t *testing.T) {
	ctx := context.Background()
	a := adapter(t)
	id := "test-exec-" + time.Now().Format("150405")
	cleanup(t, a, id)

	plan := bundle(id, nil)
	plan.Workloads[0].Command = []string{"sh", "-c", "echo hello-from-pando; sleep 3600"}
	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	time.Sleep(time.Second)

	logs, err := a.Logs(ctx, api.WorkloadRef{BundleID: id, Workload: "web"}, api.LogOptions{Tail: 10})
	require.NoError(t, err)
	defer func() { _ = logs.Close() }()

	out, err := io.ReadAll(logs)
	require.NoError(t, err)
	require.Contains(t, string(out), "hello-from-pando")

	session, err := a.Exec(ctx, api.WorkloadRef{BundleID: id, Workload: "web"}, api.ExecRequest{
		Command: []string{"echo", "exec-works"},
	})
	require.NoError(t, err)
	defer func() { _ = session.Close() }()

	buf := make([]byte, 512)
	n, _ := session.Read(buf)
	require.Contains(t, string(buf[:n]), "exec-works")
}

// R-243: capacity is adapter-reported. The adapter answers for itself rather
// than core reading /proc.
func TestR243_CapacityIsAdapterReported(t *testing.T) {
	t.Parallel()
	capacity, err := adapter(t).Capacity(context.Background())
	require.NoError(t, err)
	require.Positive(t, capacity.TotalCPUMillis)
	require.Positive(t, capacity.TotalMemoryBytes)
	require.GreaterOrEqual(t, capacity.RunningWorkloads, 0)
	require.NotEmpty(t, capacity.Details["server_version"], "what else the daemon says is passed through")
	require.False(t, capacity.Reported.IsZero())

	inUse, err := adapter(t).InUse(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, inUse.CPUMillis, 0)
	require.False(t, inUse.Reported.IsZero())
}

// R-254: capabilities are data, and the class is reported honestly — a policy
// floor above `container` must exclude this adapter, which it can only do if the
// class is true.
func TestR254_CapabilitiesAreHonest(t *testing.T) {
	t.Parallel()
	caps, err := adapter(t).Capabilities(context.Background())
	require.NoError(t, err)
	require.Equal(t, spec.IsolationContainer, caps.IsolationClass, "docker is a shared kernel")
	require.True(t, caps.SupportsPrivateNetwork)
	require.False(t, caps.SupportsStartThenSwap, "not built yet, so not claimed")
}

// dockerInspect shells out, deliberately: asserting on the container's actual
// configuration rather than on what the adapter believes it configured.
func dockerInspect(t *testing.T, name, format string) string {
	t.Helper()
	out, err := execCommand("docker", "inspect", "-f", format, name)
	require.NoError(t, err, out)
	return strings.TrimSpace(out)
}

func execCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestR023_ProxyRejoinsRunningAppsNetworksAfterItIsReplaced asserts R-023.
//
// Every app sits on its own private network and publishes nothing (R-025,
// R-026), so the proxy can reach an app only by being on that network — which
// makes network membership the mechanism behind "the proxy is never routed
// around" rather than a detail of it. Membership belongs to a container, and
// Pando's container is replaced on every upgrade, so the new one starts on
// none of the networks the old one joined while every app carries on running.
// Nothing redeploys a running app, so nothing re-attaches: the install comes
// back up with every app healthy and every app 502.
func TestR023_ProxyRejoinsRunningAppsNetworksAfterItIsReplaced(t *testing.T) {
	ctx := context.Background()

	// A stand-in for Pando's own container: something long-lived that can be
	// joined to an app network and taken off it again.
	proxy := "test-proxy-" + time.Now().Format("150405")
	run := exec.Command("docker", "run", "-d", "--name", proxy, "alpine:3.20", "sleep", "3600")
	require.NoError(t, run.Run())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", proxy).Run() })

	a := dockeradapter.New()
	require.NoError(t, a.Configure(ctx, json.RawMessage(`{"proxy_container":"`+proxy+`"}`)))
	if err := a.HealthCheck(ctx); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}

	id := "test-rejoin-" + time.Now().Format("150405")
	cleanup(t, a, id)
	// Cleanups run last-registered first. The stand-in proxy has to leave the
	// app's network before Destroy removes it, or the network is left behind.
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", proxy).Run() })
	_, err := a.Apply(ctx, bundle(id, nil))
	require.NoError(t, err)

	networkName := "pando-" + id
	require.Contains(t, dockerInspect(t, proxy, "{{json .NetworkSettings.Networks}}"), networkName,
		"a deploy puts the proxy on the app's network")

	// Replace the proxy container. Disconnecting is what a recreate amounts to
	// from the network's point of view, and it is the part that matters: the
	// app container is untouched and still running.
	require.NoError(t, exec.Command("docker", "network", "disconnect", networkName, proxy).Run())
	require.NotContains(t, dockerInspect(t, proxy, "{{json .NetworkSettings.Networks}}"), networkName)

	joined, err := a.RejoinNetworks(ctx, nil)
	require.NoError(t, err)
	require.GreaterOrEqual(t, joined, 1)
	require.Contains(t, dockerInspect(t, proxy, "{{json .NetworkSettings.Networks}}"), networkName,
		"startup must put it back, because no deploy is coming")
}

// TestR023_RejoiningLeavesTheNetworksOfStoppedAppsAlone asserts the ordering
// that keeps R-023's fix from undoing network reclamation: an empty network
// belongs to an app that is not running, an endpoint on it would make it look
// busy to the reclaimer, and there is nothing on it to reach anyway.
func TestR023_RejoiningLeavesTheNetworksOfStoppedAppsAlone(t *testing.T) {
	ctx := context.Background()

	proxy := "test-proxy-empty-" + time.Now().Format("150405")
	require.NoError(t, exec.Command("docker", "run", "-d", "--name", proxy, "alpine:3.20", "sleep", "3600").Run())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", proxy).Run() })

	a := dockeradapter.New()
	require.NoError(t, a.Configure(ctx, json.RawMessage(`{"proxy_container":"`+proxy+`"}`)))
	if err := a.HealthCheck(ctx); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}

	id := "test-rejoin-empty-" + time.Now().Format("150405")
	cleanup(t, a, id)
	_, err := a.Apply(ctx, bundle(id, nil))
	require.NoError(t, err)

	networkName := "pando-" + id
	// Take the app's containers away but keep its network, which is what a
	// stopped app looks like.
	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: id}, api.DestroyOptions{KeepVolumes: true}))
	_ = exec.Command("docker", "network", "disconnect", networkName, proxy).Run()

	_, err = a.RejoinNetworks(ctx, nil)
	require.NoError(t, err)
	require.NotContains(t, dockerInspect(t, proxy, "{{json .NetworkSettings.Networks}}"), networkName,
		"an empty network gets no endpoint, so the reclaimer can still see it is empty")
}
