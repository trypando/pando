//go:build integration

package docker_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/id"
)

// TestR212_AVolumeRoundTripsThroughSnapshotAndRestore asserts the half of the
// DR bundle that is not a database.
//
// A DR bundle without app volumes restores a Pando that knows about every app
// and has lost all their data, which is a worse outcome than failing loudly.
func TestR212_AVolumeRoundTripsThroughSnapshotAndRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	source := createVolume(t, a)
	defer func() { _ = a.DestroyVolume(ctx, source) }()

	writeIntoVolume(t, a, source, "notes.txt", "the data an app cannot lose")

	var archive bytes.Buffer
	require.NoError(t, a.SnapshotVolume(ctx, source, &archive))
	require.Positive(t, archive.Len(), "a snapshot of a volume with a file in it cannot be empty")
	require.Contains(t, namesIn(t, archive.Bytes()), "./notes.txt")

	// Into a *different* volume, which is what restore actually does — the
	// original may not exist any more. Relative paths in the archive are what
	// makes this work.
	target := createVolume(t, a)
	defer func() { _ = a.DestroyVolume(ctx, target) }()

	require.NoError(t, a.RestoreVolume(ctx, target, bytes.NewReader(archive.Bytes())))
	require.Equal(t, "the data an app cannot lose", readFromVolume(t, a, target, "notes.txt"))
}

// TestR212_RestoreReplacesRatherThanMerges asserts the destructive choice.
//
// A restore that merged would leave a volume matching neither the backup nor
// the previous state, and nobody could tell which files came from where.
func TestR212_RestoreReplacesRatherThanMerges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	source := createVolume(t, a)
	defer func() { _ = a.DestroyVolume(ctx, source) }()
	writeIntoVolume(t, a, source, "kept.txt", "from the backup")

	var archive bytes.Buffer
	require.NoError(t, a.SnapshotVolume(ctx, source, &archive))

	target := createVolume(t, a)
	defer func() { _ = a.DestroyVolume(ctx, target) }()
	writeIntoVolume(t, a, target, "stale.txt", "from before the restore")

	require.NoError(t, a.RestoreVolume(ctx, target, bytes.NewReader(archive.Bytes())))

	require.Equal(t, "from the backup", readFromVolume(t, a, target, "kept.txt"))
	require.Empty(t, readFromVolume(t, a, target, "stale.txt"),
		"a file absent from the backup must be absent after the restore")
}

// TestR212_AVolumePandoDoesNotOwnRoundTrips asserts a Postgres-shaped volume:
// the root is 0700 and owned by uid 999, so the helper's root can enter it only
// with the capabilities the helper adds back. Without them, every app with a
// database fails its hourly backup.
func TestR212_AVolumePandoDoesNotOwnRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "./", Mode: 0o700, Uid: 999, Gid: 999, Typeflag: tar.TypeDir,
	}))
	content := "16"
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "./PG_VERSION", Mode: 0o600, Uid: 999, Gid: 999, Size: int64(len(content)), Typeflag: tar.TypeReg,
	}))
	_, err := tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	source := createVolume(t, a)
	defer func() { _ = a.DestroyVolume(ctx, source) }()
	require.NoError(t, a.RestoreVolume(ctx, source, bytes.NewReader(buf.Bytes())))

	var archive bytes.Buffer
	require.NoError(t, a.SnapshotVolume(ctx, source, &archive))

	tr := tar.NewReader(bytes.NewReader(archive.Bytes()))
	owners := map[string]int{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		owners[header.Name] = header.Uid
	}
	require.Equal(t, map[string]int{"./": 999, "./PG_VERSION": 999}, owners,
		"the restore must put back ownership, and the snapshot must read past it")

	// And back into a fresh volume, which is the disaster-recovery direction.
	target := createVolume(t, a)
	defer func() { _ = a.DestroyVolume(ctx, target) }()
	require.NoError(t, a.RestoreVolume(ctx, target, bytes.NewReader(archive.Bytes())))
	require.Equal(t, content, readFromVolume(t, a, target, "PG_VERSION"))
}

// TestR212_AnEmptyVolumeSnapshotsCleanly — an app that has not written anything
// yet still has to back up, and a tar of nothing is still a valid tar.
func TestR212_AnEmptyVolumeSnapshotsCleanly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	v := createVolume(t, a)
	defer func() { _ = a.DestroyVolume(ctx, v) }()

	var archive bytes.Buffer
	require.NoError(t, a.SnapshotVolume(ctx, v, &archive))

	target := createVolume(t, a)
	defer func() { _ = a.DestroyVolume(ctx, target) }()
	require.NoError(t, a.RestoreVolume(ctx, target, bytes.NewReader(archive.Bytes())))
}

// --- helpers ---------------------------------------------------------------

func createVolume(t *testing.T, a interface {
	CreateVolume(context.Context, api.VolumeRequest) (api.VolumeHandle, error)
}) api.VolumeHandle {
	t.Helper()
	h, err := a.CreateVolume(context.Background(), api.VolumeRequest{
		VolumeID: id.New(id.Volume), BundleID: "bkp-test",
	})
	require.NoError(t, err)
	return h
}

// writeIntoVolume puts a file in a volume by restoring a one-file archive into
// it — using the adapter's own restore path rather than shelling out, so the
// helper cannot pass while the thing under test is broken in the same way.
func writeIntoVolume(t *testing.T, a interface {
	RestoreVolume(context.Context, api.VolumeHandle, io.Reader) error
}, h api.VolumeHandle, name, content string) {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "./" + name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg,
	}))
	_, err := tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	require.NoError(t, a.RestoreVolume(context.Background(), h, bytes.NewReader(buf.Bytes())))
}

func readFromVolume(t *testing.T, a interface {
	SnapshotVolume(context.Context, api.VolumeHandle, io.Writer) error
}, h api.VolumeHandle, name string) string {
	t.Helper()

	var archive bytes.Buffer
	require.NoError(t, a.SnapshotVolume(context.Background(), h, &archive))

	tr := tar.NewReader(bytes.NewReader(archive.Bytes()))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return ""
		}
		require.NoError(t, err)
		if header.Name == "./"+name {
			body, err := io.ReadAll(tr)
			require.NoError(t, err)
			return string(body)
		}
	}
}

func namesIn(t *testing.T, archive []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return names
		}
		require.NoError(t, err)
		names = append(names, header.Name)
	}
}
