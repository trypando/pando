//go:build incus

package incus

// Against a real Incus server (make test-incus, notes-incus-runtime.md). Run
// where Pando's proxy would run — somewhere that can route to the network —
// because the proxy's path through each VM's agent is what is tested.
//
//	PANDO_INCUS_ADDRESS      https://host:8443 (or a unix:// socket)
//	PANDO_INCUS_CERTS        a directory with incus-client.crt, incus-client.key, incus-server.crt
//	PANDO_INCUS_PROJECT      a project the tests may fill and empty, such as pando-test
//	PANDO_INCUS_NETWORK      the network VMs attach to
//	PANDO_INCUS_SOURCES      this machine's address on that network
//	PANDO_INCUS_TARGET       optional: the cluster member to use
//	PANDO_INCUS_POOL         optional: the storage pool
//	PANDO_INCUS_AGENT_IMAGE  Pando's image, such as trypando/pando:0.4.0

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	servicesdocker "github.com/trypando/pando/internal/adapter/services/docker"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/secret"
)

func integration(t *testing.T) *Adapter {
	t.Helper()
	address := os.Getenv("PANDO_INCUS_ADDRESS")
	if address == "" {
		t.Skip("PANDO_INCUS_ADDRESS is not set")
	}
	dir := os.Getenv("PANDO_INCUS_CERTS")
	read := func(name string) string {
		if dir == "" {
			return ""
		}
		b, err := os.ReadFile(dir + "/" + name)
		require.NoError(t, err)
		return string(b)
	}
	authority, err := hostagent.NewAuthority()
	require.NoError(t, err)
	cfg := map[string]any{
		"address":            address,
		"server_certificate": read("incus-server.crt"),
		"project":            os.Getenv("PANDO_INCUS_PROJECT"),
		"network":            os.Getenv("PANDO_INCUS_NETWORK"),
		"proxy_sources":      os.Getenv("PANDO_INCUS_SOURCES"),
		"target":             os.Getenv("PANDO_INCUS_TARGET"),
		"storage_pool":       os.Getenv("PANDO_INCUS_POOL"),
		"agent_image":        os.Getenv("PANDO_INCUS_AGENT_IMAGE"),
		"credentials": map[string]any{
			"tls_client":      read("incus-client.crt") + read("incus-client.key"),
			"agent_authority": string(authority),
		},
	}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	a := New()
	require.NoError(t, a.Configure(context.Background(), raw))
	require.NoError(t, a.HealthCheck(context.Background()))
	return a
}

func ctxFor(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func bundle(t *testing.T, prefix string) string {
	return fmt.Sprintf("app_%s%d", prefix, time.Now().UnixNano()%1_000_000)
}

func cleanup(t *testing.T, a *Adapter, bundleID string) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: bundleID}, api.DestroyOptions{KeepVolumes: false}))
	})
}

func web(name string) api.WorkloadPlan {
	return api.WorkloadPlan{
		Name:      name,
		Image:     "docker.io/library/nginx:1.29-alpine",
		Ports:     []api.PortPlan{{Number: 80, Protocol: "tcp"}},
		Exposed:   true,
		LogBytes:  10 << 20,
		Resources: api.ResourcePlan{CPUMillis: 500, MemoryBytes: 256 << 20},
	}
}

func plan(bundleID string, workloads ...api.WorkloadPlan) api.BundlePlan {
	return api.BundlePlan{BundleID: bundleID, Workloads: workloads, Network: api.NetworkPlan{Private: true}, FirstDeploy: true}
}

// get fetches / from a workload through Pando's path: the agent in its VM.
func get(t *testing.T, a *Adapter, bundleID, workload string, port int) (int, string) {
	t.Helper()
	ctx := ctxFor(t, time.Minute)
	up, err := a.Upstream(ctx, api.WorkloadRef{BundleID: bundleID, Workload: workload}, port)
	require.NoError(t, err)
	require.NotNil(t, up.Dial, "the proxy reaches a VM through its agent")
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return up.Dial(ctx) },
	}, Timeout: 30 * time.Second}
	var lastErr error
	for i := 0; i < 15; i++ {
		resp, err := client.Get(up.URL + "/")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.StatusCode, string(body)
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	require.NoError(t, lastErr)
	return 0, ""
}

// TestR023_AnImageAppRunsInItsOwnVM asserts R-023 and R-115 on a real Incus:
// a deployed app runs in a VM of its own, and Pando reaches it only through
// that VM's agent.
func TestR023_AnImageAppRunsInItsOwnVM(t *testing.T) {
	a := integration(t)
	id := bundle(t, "web")
	cleanup(t, a, id)
	ctx := ctxFor(t, 15*time.Minute)

	_, err := a.Apply(ctx, plan(id, web("web")))
	require.NoError(t, err)

	obs, err := a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	require.True(t, obs.Exists)
	require.Len(t, obs.Workloads, 1)
	require.True(t, obs.Workloads[0].Running)

	inst, _, err := a.api.Instance(ctx, instanceName(id))
	require.NoError(t, err)
	require.Equal(t, "virtual-machine", inst.Type)
	require.Equal(t, id, inst.Config[keyBundle])
	require.Empty(t, inst.Profiles, "no profile's NIC reaches the VM")

	code, body := get(t, a, id, "web", 80)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, "nginx")

	// Applying the same plan again changes nothing (idempotent).
	_, err = a.Apply(ctx, plan(id, web("web")))
	require.NoError(t, err)
	after, _, err := a.api.Instance(ctx, instanceName(id))
	require.NoError(t, err)
	require.Equal(t, inst.Config["volatile.uuid"], after.Config["volatile.uuid"])

	// Stopped, the app's workloads are present and not running, and
	// Observe does not start the VM (R-148).
	require.NoError(t, a.Stop(ctx, api.BundleRef{BundleID: id}))
	obs, err = a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	require.True(t, obs.Exists)
	require.False(t, obs.Workloads[0].Running)
	st, err := a.api.InstanceState(ctx, instanceName(id))
	require.NoError(t, err)
	require.Equal(t, "Stopped", st.Status)
}

// TestR025_OneAppsVMCannotReachAnothers asserts R-025 and R-026: the ACL on
// every VM's NIC admits nothing but Pando, including another app's VM.
func TestR025_OneAppsVMCannotReachAnothers(t *testing.T) {
	a := integration(t)
	one, two := bundle(t, "one"), bundle(t, "two")
	cleanup(t, a, one)
	cleanup(t, a, two)
	ctx := ctxFor(t, 15*time.Minute)

	_, err := a.Apply(ctx, plan(one, web("web")))
	require.NoError(t, err)
	_, err = a.Apply(ctx, plan(two, web("web")))
	require.NoError(t, err)

	addr, ok := a.address(ctx, instanceName(two))
	require.True(t, ok)
	for _, port := range []int{a.config.AgentPort, 80, 22} {
		var out bytes.Buffer
		code, err := a.api.Exec(ctx, instanceName(one), []string{
			"timeout", "5", "bash", "-c", fmt.Sprintf("echo > /dev/tcp/%s/%d", addr, port),
		}, nil, &out, &out)
		require.NoError(t, err)
		require.NotEqual(t, 0, code, "app one's VM reached app two's on port %d: %s", port, out.String())
	}

	// Pando still can.
	code, _ := get(t, a, two, "web", 80)
	require.Equal(t, http.StatusOK, code)
}

// TestR134_APostgresSlotRunsInsideTheAppsVM asserts R-134 and the note's
// services decision: the existing services adapter's workloads run in the
// app's VM, on its private network, with their data on an Incus volume.
func TestR134_APostgresSlotRunsInsideTheAppsVM(t *testing.T) {
	a := integration(t)
	id := bundle(t, "pg")
	cleanup(t, a, id)
	ctx := ctxFor(t, 15*time.Minute)

	svcs := servicesdocker.New()
	require.NoError(t, svcs.Configure(ctx, nil))
	res, err := svcs.Provision(ctx, api.ProvisionRequest{
		AppID: id, BundleID: id, SlotKey: "DATABASE_URL", Type: spec.SlotPostgres, ServiceID: "svc_" + strings.TrimPrefix(id, "app_"),
	})
	require.NoError(t, err)

	app := api.WorkloadPlan{
		Name:      "app",
		Image:     "docker.io/library/postgres:17-alpine",
		Command:   []string{"sh", "-c", "until psql \"$DATABASE_URL\" -c 'select 1'; do sleep 2; done; sleep infinity"},
		Env:       map[string]secret.Value{"DATABASE_URL": res.ConnectionSecret},
		DependsOn: []string{res.Workloads[0].Name},
		LogBytes:  10 << 20,
	}
	p := plan(id, append(res.Workloads, app)...)
	p.Volumes = res.Volumes
	_, err = a.Apply(ctx, p)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		rc, err := a.Logs(ctx, api.WorkloadRef{BundleID: id, Workload: "app"}, api.LogOptions{Tail: 50})
		if err != nil {
			return false
		}
		defer func() { _ = rc.Close() }()
		b, _ := io.ReadAll(rc)
		return strings.Contains(string(b), "(1 row)")
	}, 3*time.Minute, 3*time.Second, "the app never reached its database")

	vol, err := a.api.Volume(ctx, mustPool(t, a), volumeName(res.Volumes[0].VolumeID))
	require.NoError(t, err)
	require.Equal(t, id, vol.Config[keyBundle])
}

func mustPool(t *testing.T, a *Adapter) string {
	p, err := a.storagePool(context.Background())
	require.NoError(t, err)
	return p
}

// TestR212_AVolumeRestoresAfterItsVMIsGone asserts R-204 and R-212: a volume
// outlives its app's VM, backs up while there is no VM, and restores.
func TestR212_AVolumeRestoresAfterItsVMIsGone(t *testing.T) {
	a := integration(t)
	id := bundle(t, "vol")
	cleanup(t, a, id)
	ctx := ctxFor(t, 15*time.Minute)

	volID := "vol_" + strings.TrimPrefix(id, "app_")
	w := api.WorkloadPlan{
		Name:     "writer",
		Image:    "docker.io/library/busybox:1.37",
		Command:  []string{"sh", "-c", "echo kept > /data/note && sleep 3600"},
		Mounts:   []api.MountPlan{{VolumeID: volID, Path: "/data"}},
		LogBytes: 10 << 20,
	}
	p := plan(id, w)
	p.Volumes = []api.VolumePlan{{VolumeID: volID, Name: "data"}}
	_, err := a.Apply(ctx, p)
	require.NoError(t, err)
	time.Sleep(5 * time.Second)

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: id}, api.DestroyOptions{KeepVolumes: true}))
	_, err = a.api.Volume(ctx, mustPool(t, a), volumeName(volID))
	require.NoError(t, err, "the volume outlived the VM (R-204)")

	var backup bytes.Buffer
	h := api.VolumeHandle{VolumeID: volID, Handle: volumeName(volID)}
	require.NoError(t, a.SnapshotVolume(ctx, h, &backup))
	require.Contains(t, backup.String(), "kept")

	other := api.VolumeHandle{VolumeID: volID + "r", Handle: volumeName(volID + "r")}
	t.Cleanup(func() { _ = a.DestroyVolume(context.Background(), other) })
	require.NoError(t, a.RestoreVolume(ctx, other, bytes.NewReader(backup.Bytes())))
	var again bytes.Buffer
	require.NoError(t, a.SnapshotVolume(ctx, other, &again))
	require.Contains(t, again.String(), "kept")
}

// TestR097_ATrialRunsInAThrowawayVM asserts R-097: the trial observes the
// port the app binds, in a VM that is gone afterwards.
func TestR097_ATrialRunsInAThrowawayVM(t *testing.T) {
	a := integration(t)
	ctx := ctxFor(t, 15*time.Minute)
	id := fmt.Sprintf("trial%d", time.Now().UnixNano()%1_000_000)
	res, err := a.Trial(ctx, api.TrialRequest{
		TrialID: id,
		Image:   "docker.io/library/nginx:1.29-alpine",
		Timeout: 20 * time.Second,
	})
	require.NoError(t, err)
	require.True(t, res.Started)
	require.Contains(t, res.ObservedPorts, 80)
	_, _, err = a.api.Instance(ctx, trialName(id))
	require.ErrorIs(t, err, errNotFound)
}

// TestR243_CapacityComesFromIncus asserts R-243 against a real server.
func TestR243_CapacityComesFromIncus(t *testing.T) {
	a := integration(t)
	c, err := a.Capacity(ctxFor(t, time.Minute))
	require.NoError(t, err)
	require.Positive(t, c.TotalCPUMillis)
	require.Positive(t, c.TotalMemoryBytes)
}
