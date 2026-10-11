package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR204_VolumesUnderAVolumeRootAreBindsOfIt asserts the Incus runtime's
// use of this adapter: each volume is a bind of a directory the VM has an
// Incus volume attached at, so it outlives the VM.
func TestR204_VolumesUnderAVolumeRootAreBindsOfIt(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	a.config.VolumeRoot = "/srv/pando/volumes"
	var got map[string]any
	f.on("POST /volumes/create", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		respond(http.StatusCreated, map[string]any{"Name": "v"})(w, r)
	})
	_, err := a.CreateVolume(context.Background(), api.VolumeRequest{VolumeID: "vol_1", BundleID: "app_1"})
	require.NoError(t, err)
	require.Equal(t, "local", got["Driver"])
	require.Equal(t, map[string]any{"type": "none", "o": "bind", "device": "/srv/pando/volumes/vol_1"}, got["DriverOpts"])
}
