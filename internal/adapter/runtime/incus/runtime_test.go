package incus

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/incus/incusapi"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

// echoIncus is the fake, whose exec copies stdin to stdout, as socat to a
// socket that echoes would.
type echoIncus struct {
	*fakeIncus
	command []string
}

func (e *echoIncus) Exec(_ context.Context, _ string, command []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	e.command = command
	_, err := io.Copy(stdout, stdin)
	return 0, err
}

// TestR027_ThePodmanInAVMIsReachedThroughIncusOnly asserts the design's
// transport: a connection to the guest's Podman is an exec of socat through
// the Incus API, and closing it ends the exec.
func TestR027_ThePodmanInAVMIsReachedThroughIncusOnly(t *testing.T) {
	e := &echoIncus{fakeIncus: newFakeIncus()}
	conn, err := dialExec(ctx, e, "pando-app-1", []string{"socat", "STDIO", "UNIX-CONNECT:" + podmanSocket})
	require.NoError(t, err)
	_, err = conn.Write([]byte("GET /_ping"))
	require.NoError(t, err)
	buf := make([]byte, 10)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "GET /_ping", string(buf))
	require.NoError(t, conn.Close())
	require.NoError(t, conn.Close(), "closing twice is harmless")
	require.Equal(t, "incus-exec", conn.LocalAddr().String())
	require.Equal(t, "incus-exec", conn.RemoteAddr().Network())
	require.NoError(t, conn.SetDeadline(time.Now()))
	require.NoError(t, conn.SetReadDeadline(time.Now()))
	require.NoError(t, conn.SetWriteDeadline(time.Now()))
	require.Equal(t, []string{"socat", "STDIO", "UNIX-CONNECT:/run/podman/podman.sock"}, e.command)

	cli, err := dockerClient(e, "pando-app-1")
	require.NoError(t, err)
	require.NotNil(t, cli)
}

// TestR116_TheGuestImageIsPreparedOnceAndPublished asserts the guest image
// path: built from Debian's cloud image by cloud-init, cleaned, published
// under its recipe's alias, and the builder removed.
func TestR116_TheGuestImageIsPreparedOnceAndPublished(t *testing.T) {
	f := newFakeIncus()
	f.aliases = map[string]bool{}
	a, _, _ := testAdapter(t, f, nil)
	_, err := a.Apply(ctx, webPlan("app_1"))
	require.NoError(t, err)

	require.True(t, f.aliases[guestAlias()])
	require.Equal(t, guestAlias()+"-build", f.created[0].Name)
	require.Equal(t, "debian/13/cloud", f.created[0].Source.Alias)
	require.Contains(t, f.created[0].Config["cloud-init.user-data"], "podman")
	require.Equal(t, "8GiB", f.created[0].Devices["root"]["size"])
	require.NotContains(t, f.instances, guestAlias()+"-build")
	require.Contains(t, f.execs, []string{"cloud-init", "status", "--wait"})
	require.Contains(t, f.execs, []string{"cloud-init", "clean", "--logs", "--machine-id"})
	require.Equal(t, guestAlias(), f.created[1].Source.Alias)

	// Once prepared, it is not prepared again.
	_, err = a.Apply(ctx, webPlan("app_2"))
	require.NoError(t, err)
	require.Len(t, f.created, 3)

	// An operator's own image is used as given.
	src, err := (&Adapter{config: Config{GuestImage: "0123456789abcdef"}}).guestImage(ctx, "default")
	require.NoError(t, err)
	require.Equal(t, "0123456789abcdef", src.Fingerprint)
	src, err = (&Adapter{config: Config{GuestImage: "my-guest"}}).guestImage(ctx, "default")
	require.NoError(t, err)
	require.Equal(t, "my-guest", src.Alias)
}

// TestR254_AGuestImageThatCannotBeProvisionedSaysWhy asserts R-105 and
// R-254 for the guest image's preparation.
func TestR254_AGuestImageThatCannotBeProvisionedSaysWhy(t *testing.T) {
	f := newFakeIncus()
	f.aliases = map[string]bool{}
	f.execCode = 1
	a, _, _ := testAdapter(t, f, nil)
	_, err := a.Apply(ctx, webPlan("app_1"))
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "prepare the VM image")
	require.Contains(t, errs.As(err).Remedy, "guest_image")
	require.NotContains(t, f.instances, guestAlias()+"-build", "the builder is removed on failure too")
}

// TestR242_AChangedSizeResizesTheVM asserts that a changed plan changes the
// VM, and an unchanged one leaves it alone (idempotent Apply).
func TestR242_AChangedSizeResizesTheVM(t *testing.T) {
	f := newFakeIncus()
	a, inners, _ := testAdapter(t, f, nil)
	p := webPlan("app_1")
	_, err := a.Apply(ctx, p)
	require.NoError(t, err)
	_, err = a.Apply(ctx, p)
	require.NoError(t, err)
	require.Len(t, f.created, 1)

	p.Workloads[0].Resources = api.ResourcePlan{CPUMillis: 3000, MemoryBytes: 2 << 30}
	_, err = a.Apply(ctx, p)
	require.NoError(t, err)
	cfg := f.instances["pando-app-1"].Config
	require.Equal(t, "3", cfg["limits.cpu"])
	require.Equal(t, "2560MiB", cfg["limits.memory"])
	require.Equal(t, "00:16:3e:00:00:01", cfg["volatile.eth0.hwaddr"], "what Incus keeps there is kept")
	require.Len(t, inners["pando-app-1"].applied, 3)
}

// TestR148_ObserveReportsWhatRunsInARunningVM asserts that a running VM's
// workloads are what the Docker adapter in it sees, and that what is not
// running is never started to be looked at.
func TestR148_ObserveReportsWhatRunsInARunningVM(t *testing.T) {
	f := newFakeIncus()
	a, inners, _ := testAdapter(t, f, nil)
	_, err := a.Apply(ctx, webPlan("app_1"))
	require.NoError(t, err)
	inners["pando-app-1"].observed = api.ObservedBundle{Exists: true, Workloads: []api.ObservedWorkload{{Name: "web", Present: true, Running: true}}}
	a.forget("app_1")
	a.vms["app_1"] = &vm{name: "pando-app-1", rt: inners["pando-app-1"]}

	obs, err := a.Observe(ctx, api.BundleRef{BundleID: "app_1"})
	require.NoError(t, err)
	require.True(t, obs.Workloads[0].Running)

	// Logs, exec and usage go to the same place.
	_, err = a.Logs(ctx, api.WorkloadRef{BundleID: "app_1", Workload: "web"}, api.LogOptions{})
	require.NoError(t, err)
	_, err = a.Exec(ctx, api.WorkloadRef{BundleID: "app_1", Workload: "web"}, api.ExecRequest{})
	require.NoError(t, err)
	require.NoError(t, a.Run(ctx, api.WorkloadRef{BundleID: "app_1", Workload: "web"}, api.RunRequest{}))
	u, err := a.Usage(ctx, api.BundleRef{BundleID: "app_1"})
	require.NoError(t, err)
	require.Equal(t, "web", u.Workloads[0].Workload)

	// Stopped: no logs to stream, usage of nothing.
	require.NoError(t, a.Stop(ctx, api.BundleRef{BundleID: "app_1"}))
	_, err = a.Logs(ctx, api.WorkloadRef{BundleID: "app_1", Workload: "web"}, api.LogOptions{})
	require.Contains(t, errs.As(err).Message, "stopped")
	u, err = a.Usage(ctx, api.BundleRef{BundleID: "app_1"})
	require.NoError(t, err)
	require.Equal(t, int64(-1), u.Workloads[0].DiskBytes)
	err = a.Run(ctx, api.WorkloadRef{BundleID: "app_1", Workload: "web"}, api.RunRequest{})
	require.Equal(t, errs.NotFound, errs.CodeOf(err), "a stopped VM runs nothing (R-423)")
	err = a.Run(ctx, api.WorkloadRef{BundleID: "app_none"}, api.RunRequest{})
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	_, err = a.Exec(ctx, api.WorkloadRef{BundleID: "app_none"}, api.ExecRequest{})
	require.Equal(t, errs.NotFound, errs.As(err).Code)
	u, err = a.Usage(ctx, api.BundleRef{BundleID: "app_none"})
	require.NoError(t, err)
	require.Empty(t, u.Workloads)
}

// TestR245_InUseSumsTheRunningVMs asserts R-245: what Pando's VMs use now,
// CPU from two readings a second apart.
func TestR245_InUseSumsTheRunningVMs(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	_, err := a.Apply(ctx, webPlan("app_1"))
	require.NoError(t, err)
	inst := f.instances["pando-app-1"]
	inst.State = &incusapi.InstanceState{Memory: incusapi.InstanceStateMemory{Usage: 300 << 20}}
	now := time.Unix(0, 0)
	a.now = func() time.Time { return now }
	a.sleep = func(time.Duration) <-chan time.Time {
		f.mu.Lock()
		inst.State.CPU.Usage += 500_000_000 // half a core for a second
		f.mu.Unlock()
		now = now.Add(time.Second)
		c := make(chan time.Time, 1)
		c <- now
		return c
	}
	got, err := a.InUse(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(300<<20), got.MemoryBytes)
	require.Equal(t, 500, got.CPUMillis)
}

// TestR254_TheEdgeIsTheDockerRuntimes asserts R-254: the Incus runtime says
// it runs no edge and watches no events, rather than failing quietly.
func TestR254_TheEdgeIsTheDockerRuntimes(t *testing.T) {
	a := New()
	require.Equal(t, Kind, a.Kind())
	require.Equal(t, api.CategoryRuntime, a.Category())
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.As(a.ApplyEdge(ctx, api.EdgePlan{})).Code)
	_, err := a.ObserveEdge(ctx, "x")
	require.Error(t, err)
	require.Error(t, a.RemoveEdge(ctx, "x"))
	edges, err := a.Edges(ctx)
	require.NoError(t, err)
	require.Empty(t, edges)
	vols, err := a.EdgeVolumes(ctx)
	require.NoError(t, err)
	require.Empty(t, vols)
	require.Error(t, a.WatchBundles(ctx, nil))
	require.Error(t, a.HealthCheck(ctx), "not set up")
}

// TestR254_ConfigureOverAUnixSocketContactsNothing asserts that a server
// that is down is HealthCheck's to report, not Configure's.
func TestR254_ConfigureOverAUnixSocketContactsNothing(t *testing.T) {
	authority, err := hostagent.NewAuthority()
	require.NoError(t, err)
	raw, _ := json.Marshal(map[string]any{
		"address": "unix:///nonexistent/unix.socket", "network": "incusbr0", "proxy_sources": "10.1.0.10",
		"agent_image": "trypando/pando:0.4.0", "profiles": "a, b",
		"credentials": map[string]any{"agent_authority": string(authority)},
	})
	a := New()
	require.NoError(t, a.Configure(ctx, raw))
	require.Equal(t, []string{"a", "b"}, a.config.profiles())
	err = a.HealthCheck(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.As(err).Code)
	require.Contains(t, errs.As(err).Message, "not responding")

	require.Error(t, New().Configure(ctx, []byte("{")))
	bad, _ := json.Marshal(map[string]any{
		"network": "incusbr0", "proxy_sources": "10.1.0.10", "agent_image": "x",
		"credentials": map[string]any{"agent_authority": "not an authority"},
	})
	require.Contains(t, errs.As(New().Configure(ctx, bad)).Remedy, "new-authority")
}

func TestPlatformNamesArchitecturesAsRegistriesDo(t *testing.T) {
	require.Equal(t, "amd64", goArch("x86_64"))
	require.Equal(t, "arm64", goArch("aarch64"))
	require.Equal(t, "riscv64", goArch("riscv64"))
	require.True(t, strings.HasPrefix(instanceName("app_01ABC"), "pando-app-01abc"))
	require.Equal(t, int64(1<<20), clamp(1<<20))
	v, err := parseBytes("2GiB")
	require.NoError(t, err)
	require.Equal(t, int64(2<<30), v)
	v, err = parseBytes("1500MB")
	require.NoError(t, err)
	require.Equal(t, int64(1_500_000_000), v)
	_, err = parseBytes("")
	require.Error(t, err)
}
