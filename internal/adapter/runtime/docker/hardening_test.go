package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// createdHostConfig is what the fake daemon was asked to create.
type createdHostConfig struct {
	CapDrop     []string
	CapAdd      []string
	SecurityOpt []string
	PidsLimit   *int64
	StorageOpt  map[string]string
}

type createdContainer struct {
	Labels     map[string]string
	HostConfig createdHostConfig
}

// recordCreates answers a workload's or trial's create and the calls around
// it, and keeps every create's body. refuseSize makes the daemon refuse a
// layer size the way Docker does off XFS with pquota.
func recordCreates(f *fakeDaemon, refuseSize bool) func() []createdContainer {
	var mu sync.Mutex
	var got []createdContainer
	f.on("GET /containers/json", respond(http.StatusOK, []any{}))
	f.on("GET /images/*", respond(http.StatusOK, map[string]any{"Id": "sha256:abc", "Config": map[string]any{}}))
	// A driver that sizes layers, unless a test says otherwise.
	f.on("GET /version", respond(http.StatusOK, map[string]any{"Version": "27.3.1"}))
	f.on("GET /info", respond(http.StatusOK, map[string]any{
		"Driver": "overlay2", "DriverStatus": [][2]string{{"Backing Filesystem", "xfs"}},
	}))
	f.on("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Labels     map[string]string
			HostConfig createdHostConfig
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, createdContainer(body))
		mu.Unlock()
		if refuseSize && len(body.HostConfig.StorageOpt) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"message": "--storage-opt is supported only for overlay over xfs with 'pquota' mount option",
			})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"Id": "c1", "Warnings": []string{}})
	})
	f.on("GET /containers/c1/json", respond(http.StatusOK, map[string]any{"Id": "c1", "HostConfig": map[string]any{}}))
	f.on("POST /containers/c1/start", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return func() []createdContainer {
		mu.Lock()
		defer mu.Unlock()
		return append([]createdContainer(nil), got...)
	}
}

func requireHardened(t *testing.T, h createdHostConfig, pids int64) {
	t.Helper()
	// The client writes capabilities with their CAP_ prefix.
	added := make([]string, 0, len(h.CapAdd))
	for _, c := range h.CapAdd {
		added = append(added, strings.TrimPrefix(c, "CAP_"))
	}
	require.Len(t, h.CapDrop, 1)
	require.Equal(t, "ALL", strings.TrimPrefix(h.CapDrop[0], "CAP_"))
	require.ElementsMatch(t, workloadCaps, added)
	for _, dangerous := range []string{"NET_RAW", "MKNOD", "AUDIT_WRITE", "SYS_ADMIN", "NET_ADMIN"} {
		require.NotContains(t, added, dangerous)
	}
	require.Contains(t, h.SecurityOpt, noNewPrivileges)
	if pids == 0 {
		require.Nil(t, h.PidsLimit, "a negative setting is no limit")
		return
	}
	require.NotNil(t, h.PidsLimit)
	require.Equal(t, pids, *h.PidsLimit)
}

// TestR240_AnAppsContainersAreHeldToAProcessLimitAndKeepNoPrivilegesTheyDoNotNeed
// asserts R-240 for the limit a CPU and memory cap left out: with no process
// limit one app's fork bomb exhausted the process table every app and Pando
// share (finding 8 of the 2026-10-16 security assessment). And the container
// drops the capabilities an ordinary image does not need to start, and cannot
// gain more through a setuid binary.
func TestR240_AnAppsContainersAreHeldToAProcessLimitAndKeepNoPrivilegesTheyDoNotNeed(t *testing.T) {
	ctx := context.Background()
	plan := api.BundlePlan{BundleID: "app_01HQ8", Network: api.NetworkPlan{Private: true}}
	w := api.WorkloadPlan{Name: "web", Image: "nginx:1"}

	f, a := newFakeDaemon(t, nil)
	created := recordCreates(f, false)
	require.NoError(t, a.applyWorkload(ctx, plan, w, "net1", nil))
	got := created()
	require.Len(t, got, 1)
	requireHardened(t, got[0].HostConfig, DefaultPidsLimit)
	require.Equal(t, hardeningLevel, got[0].Labels[labelHardening])

	// The host sets the limit; a negative one removes it.
	f, a = newFakeDaemon(t, map[string]any{"pids_limit": 4096})
	created = recordCreates(f, false)
	require.NoError(t, a.applyWorkload(ctx, plan, w, "net1", nil))
	requireHardened(t, created()[0].HostConfig, 4096)

	f, a = newFakeDaemon(t, map[string]any{"pids_limit": -1})
	created = recordCreates(f, false)
	require.NoError(t, a.applyWorkload(ctx, plan, w, "net1", nil))
	requireHardened(t, created()[0].HostConfig, 0)
}

// TestR097_ATrialRunIsHeldToTheSameLimitsAsTheDeploy asserts that a
// repository's trial run, the least trusted code Pando runs, is not the one
// place without a process limit or with its full capabilities.
func TestR097_ATrialRunIsHeldToTheSameLimitsAsTheDeploy(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	created := recordCreates(f, false)

	_, err := a.startTrialContainer(context.Background(), api.TrialRequest{TrialID: "t1", Image: "example:1"}, "net1")
	require.NoError(t, err)
	got := created()
	require.Len(t, got, 1)
	requireHardened(t, got[0].HostConfig, DefaultPidsLimit)
}

// TestR403_ADiskLimitCapsAContainersOwnLayerWhereTheDaemonCan asserts the
// best-effort half of holding an app to its disk limit: where the storage
// driver can size a container, each workload's own layer is capped at the
// app's limit, so no workload writes past it between two readings. Where it
// cannot, the create is made again without a size, the refusal is remembered
// so it is not asked again, and Capacity says the limit is measured only.
func TestR403_ADiskLimitCapsAContainersOwnLayerWhereTheDaemonCan(t *testing.T) {
	ctx := context.Background()
	plan := api.BundlePlan{BundleID: "app_01HQ8", Network: api.NetworkPlan{Private: true}}
	w := api.WorkloadPlan{Name: "web", Image: "nginx:1", Resources: api.ResourcePlan{DiskBytes: 5 << 30}}

	f, a := newFakeDaemon(t, nil)
	created := recordCreates(f, false)
	require.Contains(t, a.layerQuotaDetail(), "not yet known")
	require.NoError(t, a.applyWorkload(ctx, plan, w, "net1", nil))
	require.Equal(t, map[string]string{"size": "5368709120"}, created()[0].HostConfig.StorageOpt)
	require.Equal(t, "enforced", a.layerQuotaDetail())

	f, a = newFakeDaemon(t, nil)
	created = recordCreates(f, true)
	require.NoError(t, a.applyWorkload(ctx, plan, w, "net1", nil), "a driver that cannot size a layer still runs the app")
	require.NoError(t, a.applyWorkload(ctx, plan, w, "net1", nil))
	got := created()
	require.Len(t, got, 3, "refused once, made again without a size, and not asked again")
	require.NotEmpty(t, got[0].HostConfig.StorageOpt)
	require.Empty(t, got[1].HostConfig.StorageOpt)
	require.Empty(t, got[2].HostConfig.StorageOpt)
	requireHardened(t, got[1].HostConfig, DefaultPidsLimit)
	require.Contains(t, a.layerQuotaDetail(), "not enforced")

	// A daemon that would accept a size and write past it is never given one:
	// Docker with the containerd image store, as Docker Desktop runs.
	f, a = newFakeDaemon(t, nil)
	created = recordCreates(f, false)
	f.on("GET /info", respond(http.StatusOK, map[string]any{
		"Driver": "overlayfs", "DriverStatus": [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}},
	}))
	require.NoError(t, a.applyWorkload(ctx, plan, w, "net1", nil))
	require.Empty(t, created()[0].HostConfig.StorageOpt)
	require.Contains(t, a.layerQuotaDetail(), "not enforced")

	// No limit, no size.
	f, a = newFakeDaemon(t, nil)
	created = recordCreates(f, false)
	require.NoError(t, a.applyWorkload(ctx, plan, api.WorkloadPlan{Name: "web", Image: "nginx:1"}, "net1", nil))
	require.Empty(t, created()[0].HostConfig.StorageOpt)
}

// TestR240_AContainerFromBeforeTheLimitsIsRecreated asserts that an upgrade
// applies the limits to apps already running, rather than only to the next
// deploy: a container without the hardening label does not match its plan.
func TestR240_AContainerFromBeforeTheLimitsIsRecreated(t *testing.T) {
	w := api.WorkloadPlan{Name: "web", Image: "nginx:1"}
	matches := func(labels map[string]string) bool {
		t.Helper()
		f, a := newFakeDaemon(t, nil)
		f.on("GET /containers/c1/json", respond(http.StatusOK, map[string]any{
			"Id": "c1", "Config": map[string]any{"Image": "nginx:1", "Labels": labels}, "HostConfig": map[string]any{"PidsLimit": DefaultPidsLimit},
		}))
		ok, err := a.matchesPlan(context.Background(), "c1", w, "pando-app", nil)
		require.NoError(t, err)
		return ok
	}
	require.True(t, matches(map[string]string{labelHardening: hardeningLevel}))
	require.False(t, matches(map[string]string{}), "created before the limits existed")
	require.False(t, matches(map[string]string{labelHardening: "0"}), "created under an older set")
}

// TestR403_AChangedDiskLimitRecreatesASizedContainer asserts that where the
// driver sizes layers, a container under another cap than the plan's (or a
// cap where the plan has none, or none where it has one) does not match its
// plan, so a change to the disk limit alone reaches the container. Where the
// driver cannot size a layer, the size asks nothing of a container.
func TestR403_AChangedDiskLimitRecreatesASizedContainer(t *testing.T) {
	const limit = 5 << 30
	matches := func(info map[string]any, size string, diskBytes int64) bool {
		t.Helper()
		f, a := newFakeDaemon(t, nil)
		f.on("GET /version", respond(http.StatusOK, map[string]any{"Version": "27.3.1"}))
		f.on("GET /info", respond(http.StatusOK, info))
		hostCfg := map[string]any{"PidsLimit": DefaultPidsLimit}
		if size != "" {
			hostCfg["StorageOpt"] = map[string]string{"size": size}
		}
		f.on("GET /containers/c1/json", respond(http.StatusOK, map[string]any{
			"Id": "c1", "HostConfig": hostCfg,
			"Config": map[string]any{"Image": "nginx:1", "Labels": map[string]string{labelHardening: hardeningLevel}},
		}))
		w := api.WorkloadPlan{Name: "web", Image: "nginx:1", Resources: api.ResourcePlan{DiskBytes: diskBytes}}
		ok, err := a.matchesPlan(context.Background(), "c1", w, "pando-app", nil)
		require.NoError(t, err)
		return ok
	}
	xfs := map[string]any{"Driver": "overlay2", "DriverStatus": [][2]string{{"Backing Filesystem", "xfs"}}}
	snapshotter := map[string]any{"Driver": "overlayfs", "DriverStatus": [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}}}

	require.True(t, matches(xfs, "5368709120", limit), "the same cap")
	require.False(t, matches(xfs, "1073741824", limit), "another cap")
	require.False(t, matches(xfs, "", limit), "a cap the container does not have")
	require.False(t, matches(xfs, "5368709120", 0), "a cap the plan no longer asks for")
	require.True(t, matches(xfs, "", 0), "no cap either way")

	require.True(t, matches(snapshotter, "", limit), "a driver that cannot size a layer asks nothing")
	require.True(t, matches(map[string]any{"Driver": "vfs"}, "", limit))
}

// TestR240_AChangedProcessLimitRecreatesTheContainer asserts that pids_limit
// reaches containers already running: a container under another limit than
// the one configured, including none against one, does not match its plan.
func TestR240_AChangedProcessLimitRecreatesTheContainer(t *testing.T) {
	matches := func(config map[string]any, pids any) bool {
		t.Helper()
		f, a := newFakeDaemon(t, config)
		hostCfg := map[string]any{}
		if pids != nil {
			hostCfg["PidsLimit"] = pids
		}
		f.on("GET /containers/c1/json", respond(http.StatusOK, map[string]any{
			"Id": "c1", "HostConfig": hostCfg,
			"Config": map[string]any{"Image": "nginx:1", "Labels": map[string]string{labelHardening: hardeningLevel}},
		}))
		ok, err := a.matchesPlan(context.Background(), "c1", api.WorkloadPlan{Name: "web", Image: "nginx:1"}, "pando-app", nil)
		require.NoError(t, err)
		return ok
	}
	unlimited := map[string]any{"pids_limit": -1}

	require.True(t, matches(nil, DefaultPidsLimit), "the default, unchanged")
	require.False(t, matches(nil, nil), "created with no limit, now the default")
	require.False(t, matches(nil, -1), "created unlimited, now the default")
	require.False(t, matches(map[string]any{"pids_limit": 4096}, DefaultPidsLimit), "the limit raised")
	require.False(t, matches(unlimited, DefaultPidsLimit), "the limit removed")
	for _, none := range []any{nil, 0, -1} {
		require.True(t, matches(unlimited, none), "no limit either way, however Docker says so: %v", none)
	}
}

func TestSizesLayersOnlyWhereTheDriverAppliesIt(t *testing.T) {
	xfs := [][2]string{{"Backing Filesystem", "xfs"}}
	require.True(t, sizesLayers("overlay2", xfs, false))
	require.True(t, sizesLayers("btrfs", nil, false))
	require.True(t, sizesLayers("zfs", nil, false))
	require.False(t, sizesLayers("overlay2", [][2]string{{"Backing Filesystem", "extfs"}}, false))
	require.False(t, sizesLayers("overlay2", nil, false))
	require.False(t, sizesLayers("overlayfs", [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}}, false))
	require.False(t, sizesLayers("fuse-overlayfs", nil, false))
	require.False(t, sizesLayers("vfs", nil, false))
	require.False(t, sizesLayers("overlay2", xfs, true), "Podman is never claimed")
}

func TestStorageOptUnsupportedReadsTheEnginesRefusal(t *testing.T) {
	for _, msg := range []string{
		"Error response from daemon: --storage-opt is supported only for overlay over xfs with 'pquota' mount option",
		"Error response from daemon: Storage Opt size is not supported on this backend",
		"storage-opt size is not supported for vfs",
		"size option requires backingFS xfs with pquota",
	} {
		require.True(t, storageOptUnsupported(errors.New(msg)), msg)
	}
	for _, msg := range []string{
		"Error response from daemon: No such image: nginx:1",
		"Conflict. The container name \"/web\" is already in use",
		// About a size or a quota, and not this.
		"write /var/lib/docker/tmp: disk quota exceeded; size of layer unknown",
		"shm size exceeds the memory quota",
	} {
		require.False(t, storageOptUnsupported(errors.New(msg)), msg)
	}
	require.False(t, storageOptUnsupported(nil))
}
