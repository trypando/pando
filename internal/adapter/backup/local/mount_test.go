package local_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/adapter/backup/local"
	"github.com/trypando/pando/internal/log"
)

// containerMountinfo is the mount table of Pando in the shipped Compose stack:
// an overlay root, the data volume at /var/lib/pando, a bind-mounted host
// directory at /srv/pando backups (a space, which mountinfo escapes), and a
// tmpfs at /dev/shm.
const containerMountinfo = `620 540 0:52 / / rw,relatime master:250 - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w
621 620 0:55 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
622 620 0:56 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755
630 622 0:60 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k
640 620 8:1 /var/lib/docker/volumes/pando_pando-data/_data /var/lib/pando rw,relatime - ext4 /dev/sda1 rw
641 620 8:1 /srv/pando-backups /srv/pando\040backups rw,relatime - ext4 /dev/sda1 rw
642 620 8:1 /var/lib/docker/volumes/pando_pando-secrets/_data /run/pando-secrets ro,relatime - ext4 /dev/sda1 rw
`

// hostMountinfo is Pando run as a binary on a host: the root is a real disk.
const hostMountinfo = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
23 22 0:21 / /tmp rw,nosuid,nodev shared:2 - tmpfs tmpfs rw
`

func locate(table, path string) local.Location {
	return local.Locate(strings.NewReader(table), path, "/var/lib/pando")
}

func TestLocateClassifiesTheMountABackupDirectoryIsOn(t *testing.T) {
	cases := []struct {
		name, table, path string
		want              local.Location
	}{
		{"the default, on the data volume", containerMountinfo, "/var/lib/pando/backups", local.LocationDataVolume},
		{"the data directory itself", containerMountinfo, "/var/lib/pando", local.LocationDataVolume},
		{"a bind-mounted host directory", containerMountinfo, "/srv/pando backups", local.LocationOtherMount},
		{"inside a bind-mounted host directory", containerMountinfo, "/srv/pando backups/daily", local.LocationOtherMount},
		{"a path mounted nowhere, such as /tmp", containerMountinfo, "/tmp/backups", local.LocationContainerLayer},
		{"a sibling whose name only starts like a mount", containerMountinfo, "/var/lib/pandora", local.LocationContainerLayer},
		{"a tmpfs", containerMountinfo, "/dev/shm/backups", local.LocationMemory},
		{"the root disk of a host install", hostMountinfo, "/srv/backups", local.LocationHostRoot},
		{"a host tmpfs", hostMountinfo, "/tmp/backups", local.LocationMemory},
		{"an empty mount table", "", "/var/lib/pando/backups", local.LocationUnknown},
		{"lines that are not mountinfo", "garbage\n1 2 3 4 /x no-separator\n", "/x", local.LocationUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, locate(tc.table, tc.path))
		})
	}
}

func TestLocatePrefersTheMountMadeLast(t *testing.T) {
	// Two mounts at one point: the second is over the first, and is what a
	// write reaches.
	table := containerMountinfo + "650 620 0:70 / /var/lib/pando rw - tmpfs tmpfs rw\n"
	require.Equal(t, local.LocationMemory, locate(table, "/var/lib/pando/backups"))
}

func TestWarningOnlyForDirectoriesThatDoNotOutliveTheContainer(t *testing.T) {
	for _, loc := range []local.Location{
		local.LocationUnknown, local.LocationDataVolume, local.LocationOtherMount, local.LocationHostRoot,
	} {
		require.Empty(t, local.Warning(loc, "/x"), "location %d", loc)
	}

	w := local.Warning(local.LocationContainerLayer, "/tmp/backups")
	require.Contains(t, w, "/tmp/backups")
	require.Contains(t, w, "lost when the container is recreated")
	require.Contains(t, w, local.DefaultPath, "R-105: the warning names a directory that works")

	w = local.Warning(local.LocationMemory, "/dev/shm/b")
	require.Contains(t, w, "lost when Pando restarts")
}

func fakeMountinfo(t *testing.T, table string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	require.NoError(t, os.WriteFile(p, []byte(table), 0o600))
	t.Cleanup(local.SetMountinfoPath(p))
}

func TestWarningsReadTheMountTable(t *testing.T) {
	fakeMountinfo(t, containerMountinfo)

	require.Empty(t, local.Warnings(nil), "the default is on the data volume")
	require.NotNil(t, local.Info().Warn, "saving one returns them")
	require.Empty(t, local.Warnings(json.RawMessage(`{"path":"/srv/pando backups"}`)))
	require.Empty(t, local.Warnings(json.RawMessage(`{`)), "a malformed configuration is Configure's error, not a warning")
	require.Empty(t, local.Warnings(json.RawMessage(`{"path":""}`)))

	w := local.Warnings(json.RawMessage(`{"path":"/tmp/pando-backups-test-nonexistent"}`))
	require.Len(t, w, 1)
	require.Contains(t, w[0], "/tmp/pando-backups-test-nonexistent")
}

func TestWarningsAreSilentWithoutAMountTable(t *testing.T) {
	// macOS, or any system with no /proc: nothing to read, so nothing to say.
	t.Cleanup(local.SetMountinfoPath(filepath.Join(t.TempDir(), "absent")))
	require.Empty(t, local.Warnings(json.RawMessage(`{"path":"/tmp/backups"}`)))
}

func TestConfigureLogsAWarningForADirectoryInTheContainerLayer(t *testing.T) {
	fakeMountinfo(t, containerMountinfo)
	core, logs := observer.New(zapcore.WarnLevel)
	ctx := log.Into(context.Background(), zap.New(core))

	a := local.New()
	require.NoError(t, a.Configure(ctx, json.RawMessage(`{"path":"/tmp/pando-backups-test-nonexistent"}`)),
		"a warning, never a blocker")
	entries := logs.All()
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].ContextMap()["reason"], "lost when the container is recreated")

	require.NoError(t, a.Configure(ctx, nil))
	require.Len(t, logs.All(), 1, "the default says nothing")
}
