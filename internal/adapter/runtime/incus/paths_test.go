package incus

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/incus/incusapi"
	"github.com/trypando/pando/internal/errs"
)

// TestR242_ASizeIncusCannotApplyLiveIsAppliedByRestarting asserts that a
// change Incus refuses on a running VM is applied stopped, then started.
func TestR242_ASizeIncusCannotApplyLiveIsAppliedByRestarting(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	p := webPlan("app_1")
	_, err := a.Apply(ctx, p)
	require.NoError(t, err)
	f.refuseLive = true
	f.states = nil
	p.Workloads[0].Resources.MemoryBytes = 256 << 20
	_, err = a.Apply(ctx, p)
	require.NoError(t, err)
	require.Equal(t, []string{"pando-app-1 stop", "pando-app-1 start"}, f.states)
	require.Equal(t, "768MiB", f.instances["pando-app-1"].Config["limits.memory"])
}

// TestR148_AVMThatWillNotStopCleanlyIsStoppedForcibly asserts Stop's
// fallback, so a stopped app holds no capacity (R-242).
func TestR148_AVMThatWillNotStopCleanlyIsStoppedForcibly(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	_, err := a.Apply(ctx, webPlan("app_1"))
	require.NoError(t, err)
	f.refuseCleanStop = true
	f.states = nil
	require.NoError(t, a.Stop(ctx, api.BundleRef{BundleID: "app_1"}))
	require.Equal(t, []string{"pando-app-1 stop", "pando-app-1 stop force"}, f.states)
	require.NoError(t, a.Stop(ctx, api.BundleRef{BundleID: "app_none"}))
}

// TestR212_ARestoreIntoAVolumeThatIsGoneCreatesIt asserts R-212's restore
// onto a new install, where the volume does not exist yet.
func TestR212_ARestoreIntoAVolumeThatIsGoneCreatesIt(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	f.files["default/pando-vol-9"] = newMemFS(t)
	var empty bytes.Buffer
	require.NoError(t, tar.NewWriter(&empty).Close())
	require.NoError(t, a.RestoreVolume(ctx, api.VolumeHandle{VolumeID: "vol_9"}, bytes.NewReader(empty.Bytes())))
	require.Contains(t, f.volumes, "default/pando-vol-9")

	err := a.RestoreVolume(ctx, api.VolumeHandle{}, &empty)
	require.Equal(t, errs.ValidInvalid, errs.As(err).Code)
	err = a.SnapshotVolume(ctx, api.VolumeHandle{}, io.Discard)
	require.Equal(t, errs.ValidInvalid, errs.As(err).Code)
	err = a.SnapshotVolume(ctx, api.VolumeHandle{VolumeID: "vol_none"}, io.Discard)
	require.Equal(t, errs.NotFound, errs.As(err).Code)
	require.NoError(t, a.DestroyVolume(ctx, api.VolumeHandle{VolumeID: "vol_none"}))
}

// TestR023_UpstreamFindsTheAddressOfAVMItHasNotSeen asserts that a replica
// that did not deploy an app still reaches it (R-023).
func TestR023_UpstreamFindsTheAddressOfAVMItHasNotSeen(t *testing.T) {
	f := newFakeIncus()
	a, inners, _ := testAdapter(t, f, nil)
	_, err := a.Apply(ctx, webPlan("app_1"))
	require.NoError(t, err)
	a.forget("app_1")
	up, err := a.Upstream(ctx, api.WorkloadRef{BundleID: "app_1", Workload: "web"}, 80)
	require.NoError(t, err)
	require.NotNil(t, up.Dial)
	require.Contains(t, inners, "pando-app-1")

	_, err = a.Upstream(ctx, api.WorkloadRef{BundleID: "app_none", Workload: "web"}, 80)
	require.Equal(t, errs.NotFound, errs.As(err).Code)
}

// TestR254_AVMWithoutAnAddressSaysSo asserts R-105: the readiness timeout
// names what it was waiting for.
func TestR254_AVMWithoutAnAddressSaysSo(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	a.readyWait = 20 * time.Millisecond
	require.NoError(t, f.CreateInstance(ctx, "", incusapi.InstancesPost{Name: "pando-x"}))
	f.instances["pando-x"].Config["volatile.eth0.hwaddr"] = "ff:ff:ff:ff:ff:ff"
	require.NoError(t, f.SetState(ctx, "pando-x", "start", false))
	_, err := a.waitReady(ctx, "pando-x")
	require.Contains(t, errs.As(err).Message, "got no IPv4 address")

	f.instances["pando-x"].Config["volatile.eth0.hwaddr"] = "00:16:3e:00:00:01"
	f.execCode = 1
	_, err = a.waitReady(ctx, "pando-x")
	require.Contains(t, errs.As(err).Message, "Podman inside it did not answer")
}

// TestR151_StopLeavesAnInstanceThatIsNotTheAppsAlone asserts that Stop, like
// every other method, touches only the app's own VM.
func TestR151_StopLeavesAnInstanceThatIsNotTheAppsAlone(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	require.NoError(t, f.CreateInstance(ctx, "", incusapi.InstancesPost{Name: "pando-app-1"}))
	require.NoError(t, f.SetState(ctx, "pando-app-1", "start", false))
	f.states = nil
	require.NoError(t, a.Stop(ctx, api.BundleRef{BundleID: "app_1"}))
	require.Empty(t, f.states)
	require.Equal(t, "Running", f.instances["pando-app-1"].Status)

	f.fail = map[string]error{"Instance": io.ErrUnexpectedEOF}
	require.Equal(t, errs.AdapterUnavailable, errs.As(a.Stop(ctx, api.BundleRef{BundleID: "app_1"})).Code)
}

// TestR204_ARestoredVolumeIsAdoptedByTheAppThatUsesIt asserts that a volume
// a restore made, with no owner yet, becomes the app's when it is deployed,
// so destroying the app with its volumes removes it; and that another app's
// volume is never taken (R-134).
func TestR204_ARestoredVolumeIsAdoptedByTheAppThatUsesIt(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	f.files["default/pando-vol-1"] = newMemFS(t)
	var empty bytes.Buffer
	require.NoError(t, tar.NewWriter(&empty).Close())
	require.NoError(t, a.RestoreVolume(ctx, api.VolumeHandle{VolumeID: "vol_1"}, &empty))
	require.Empty(t, f.volumes["default/pando-vol-1"].Config[keyBundle])

	p := webPlan("app_1")
	p.Volumes = []api.VolumePlan{{VolumeID: "vol_1"}}
	_, err := a.Apply(ctx, p)
	require.NoError(t, err)
	require.Equal(t, "app_1", f.volumes["default/pando-vol-1"].Config[keyBundle])
	require.Equal(t, "vol_1", f.volumes["default/pando-vol-1"].Config[keyVolume], "the rest of its settings are kept")

	other := webPlan("app_2")
	other.Volumes = []api.VolumePlan{{VolumeID: "vol_1"}}
	_, err = a.Apply(ctx, other)
	require.Contains(t, errs.As(err).Message, "belongs to another app")

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: "app_1"}, api.DestroyOptions{KeepVolumes: false}))
	require.NotContains(t, f.volumes, "default/pando-vol-1")
}

// TestR212_ARestoreCannotWriteToALinkItMade asserts that an entry named like
// a link the archive made earlier is refused, not written through the link.
func TestR212_ARestoreCannotWriteToALinkItMade(t *testing.T) {
	c := memorySFTP(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./passwd", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}))
	_, _ = tw.Write([]byte("x"))
	require.NoError(t, tw.Close())
	err := extract(ctx, c, tar.NewReader(&buf))
	require.ErrorContains(t, err, "through a link it made")
}
