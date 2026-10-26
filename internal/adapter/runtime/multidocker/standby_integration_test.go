//go:build integration

package multidocker

import (
	"archive/tar"
	stdbytes "bytes"
	"context"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/id"
)

// TestR420_AVolumeShipsIncrementallyBetweenRealDockerVolumes runs the
// shipping helpers against a real daemon, standing in for both hosts: the
// standby's copies are their own volumes, so one daemon is enough to check
// what each helper does.
func TestR420_AVolumeShipsIncrementallyBetweenRealDockerVolumes(t *testing.T) {
	ctx := context.Background()
	rt := docker.New()
	require.NoError(t, rt.Configure(ctx, nil))
	if err := rt.HealthCheck(ctx); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	bundle := "sbtest-" + id.New(id.App)
	volumeID := id.New(id.Volume)
	vol, err := rt.CreateVolume(ctx, api.VolumeRequest{VolumeID: volumeID, BundleID: bundle})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = rt.RemoveStandbyStorage(context.Background(), bundle)
		_ = rt.DestroyVolume(context.Background(), vol)
	})

	a := &Adapter{placement: placements{byBundle: map[string]string{bundle: "home"}}}
	a.hosts = []*host{{cfg: HostConfig{Name: "home", Control: true}, rt: rt}, {cfg: HostConfig{Name: "standby"}, rt: rt}}
	a.control = a.hosts[0]
	ship := func() api.ShipResult {
		t.Helper()
		got, err := a.ShipVolumes(ctx, api.ShipRequest{BundleID: bundle, Host: "standby", VolumeIDs: []string{volumeID}})
		require.NoError(t, err)
		return got
	}

	put(t, rt, vol, map[string]string{"first.txt": "one", "keep.txt": "same"})
	time.Sleep(1100 * time.Millisecond) // past the listing's second
	got := ship()
	require.Equal(t, 1, got.Copies)
	require.Equal(t, 3, got.FilesSent, "the first copy is everything: the root and two files")

	time.Sleep(1100 * time.Millisecond)
	require.True(t, ship().Unchanged)

	for i := 0; i < 3; i++ {
		put(t, rt, vol, map[string]string{"keep.txt": "same", "second.txt": "two"})
		time.Sleep(1100 * time.Millisecond)
		got = ship()
	}
	require.LessOrEqual(t, got.Copies, 3, "three copies are kept")

	// The oldest copy kept still holds what was in the volume then.
	copies, err := rt.StandbyCopies(ctx, bundle, volumeID)
	require.NoError(t, err)
	require.Len(t, copies.Seqs, got.Copies)
	require.NoError(t, rt.RestoreStandbyCopy(ctx, bundle, volumeID, copies.Seqs[len(copies.Seqs)-1]))
	require.Equal(t, []string{"./", "./keep.txt", "./second.txt"}, names(t, rt, vol))
}

func put(t *testing.T, rt *docker.Adapter, vol api.VolumeHandle, files map[string]string) {
	t.Helper()
	var buf stdbytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o644, Size: int64(len(content)),
			Typeflag: tar.TypeReg, ModTime: time.Now()}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, rt.RestoreVolume(context.Background(), vol, &buf))
}

func names(t *testing.T, rt *docker.Adapter, vol api.VolumeHandle) []string {
	t.Helper()
	var archive stdbytes.Buffer
	require.NoError(t, rt.SnapshotVolume(context.Background(), vol, &archive))
	var out []string
	tr := tar.NewReader(&archive)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		out = append(out, h.Name)
	}
	sort.Strings(out)
	return out
}
