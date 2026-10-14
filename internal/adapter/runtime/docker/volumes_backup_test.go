package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// helperDaemon answers enough of the Engine API to create a helper, captures
// the create request, and refuses the attach so the call ends there.
func helperDaemon(t *testing.T) (*Adapter, *struct{ HostConfig container.HostConfig }) {
	t.Helper()
	f, a := newFakeDaemon(t, nil)
	got := &struct{ HostConfig container.HostConfig }{}
	f.on("GET /images/*", respond(http.StatusOK, map[string]any{"Id": "sha256:abc"}))
	f.on("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(got))
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "helper1"})
	})
	f.on("POST /containers/helper1/attach", respond(http.StatusInternalServerError, map[string]string{"message": "no"}))
	f.on("DELETE /containers/helper1", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return a, got
}

// TestR212_ASnapshotCanReadAVolumeItDoesNotOwn asserts the snapshot helper
// keeps exactly the capability that lets root read a directory owned by
// someone else, such as a Postgres data directory, and no write capability.
func TestR212_ASnapshotCanReadAVolumeItDoesNotOwn(t *testing.T) {
	a, got := helperDaemon(t)

	_ = a.SnapshotVolume(context.Background(), api.VolumeHandle{Handle: "v1"}, &bytes.Buffer{})

	require.Equal(t, []string{"ALL"}, []string(got.HostConfig.CapDrop))
	require.Equal(t, []string{"CAP_DAC_READ_SEARCH"}, []string(got.HostConfig.CapAdd))
	require.True(t, got.HostConfig.Mounts[0].ReadOnly)
}

// TestR212_ARestoreCanPutBackOwnership asserts the restore helper can empty a
// volume it does not own and restore each file's owner and mode.
func TestR212_ARestoreCanPutBackOwnership(t *testing.T) {
	a, got := helperDaemon(t)

	_ = a.RestoreVolume(context.Background(), api.VolumeHandle{Handle: "v1"}, strings.NewReader(""))

	require.Equal(t, []string{"ALL"}, []string(got.HostConfig.CapDrop))
	require.ElementsMatch(t, []string{"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID"}, []string(got.HostConfig.CapAdd))
}

func TestHelperFailedQuotesTheHelpersLastLine(t *testing.T) {
	err := helperFailed("back up", 1, "tar: can't change directory to '/volume': Permission denied")
	require.ErrorContains(t, err,
		`Pando could not back up this app's storage: the helper exited with status 1 and said "tar: can't change directory to '/volume': Permission denied".`)

	require.ErrorContains(t, helperFailed("restore", 2, ""),
		"Pando could not restore this app's storage: the helper exited with status 2.")
}

func TestTailBufferKeepsTheLastLineWithinItsBound(t *testing.T) {
	b := &tailBuffer{max: 32}
	_, _ = b.Write([]byte(strings.Repeat("tar: noise\n", 100)))
	_, _ = b.Write([]byte("tar: the reason\n"))

	require.LessOrEqual(t, len(b.buf), 32)
	require.Equal(t, "tar: the reason", b.lastLine())
	require.Empty(t, (&tailBuffer{max: 8}).lastLine())
}
