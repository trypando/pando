//go:build integration

package docker_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR420_ABusyVolumeWithVanishingFilesShips asserts R-420's live copy on a
// real daemon: while the app makes and removes files as fast as it can, a
// listing and a read of what it listed never fail for a file that went
// between being found and being read — that file is left out, as the next
// listing shows it removed.
func TestR420_ABusyVolumeWithVanishingFilesShips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := "test-busy-" + time.Now().Format("150405.000")
	cleanup(t, a, id)

	p := bundle(id, nil)
	p.Volumes = []api.VolumePlan{{VolumeID: "vol_busy", Name: "data"}}
	p.Workloads[0].Mounts = []api.MountPlan{{VolumeID: "vol_busy", Path: "/data"}}
	p.Workloads[0].Command = []string{"sh", "-c",
		`while true; do mkdir -p /data/d; i=0; while [ $i -lt 400 ]; do echo x > /data/d/f$i; i=$((i+1)); done; rm -rf /data/d; done`}
	_, err := a.Apply(ctx, p)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = a.Destroy(context.Background(), api.BundleRef{BundleID: id}, api.DestroyOptions{KeepVolumes: false})
	})
	time.Sleep(500 * time.Millisecond)

	for range 8 {
		listing, err := a.ListVolume(ctx, id, "vol_busy")
		require.NoError(t, err, "a file gone between find and stat is not an error")
		paths := make([]string, 0, len(listing.Entries))
		for _, e := range listing.Entries {
			if e.Path != "." {
				paths = append(paths, e.Path)
			}
		}
		require.NoError(t, a.ReadVolumeFiles(ctx, id, "vol_busy", paths, io.Discard),
			"a file gone before tar reads it is left out")
	}
}
