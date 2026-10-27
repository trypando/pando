//go:build !windows

package docker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR416_OwnSocketIsTheHostPathPandoReachesDockerThrough asserts what the
// Docker hosts runtime gives the agent on the install's own machine (R-416):
// the host path of the socket mounted into Pando's own container at the path
// its client uses, and the socket's group. Nothing when Pando reaches Docker
// over TCP, is not in a container, or has no such mount.
func TestR416_OwnSocketIsTheHostPathPandoReachesDockerThrough(t *testing.T) {
	ctx := context.Background()
	f, overTCP := newFakeDaemon(t, nil)
	_, ok := overTCP.OwnSocket(ctx)
	require.False(t, ok, "over TCP there is no socket to give")
	_, ok = New().OwnSocket(ctx)
	require.False(t, ok, "not set up")

	dir, err := os.MkdirTemp("", "dos")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	raw, err := json.Marshal(map[string]any{"host": "unix://" + sock})
	require.NoError(t, err)
	a := New()
	require.NoError(t, a.Configure(ctx, raw))

	hostname, err := os.Hostname()
	require.NoError(t, err)
	_, ok = a.OwnSocket(ctx)
	require.False(t, ok, "Pando is not in a container here")

	mounts := []map[string]any{{"Type": "volume", "Name": "pando-data", "Destination": "/var/lib/pando"}}
	f.on("GET /containers/"+hostname+"/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": "c-self", "Mounts": mounts})
	})
	_, ok = a.OwnSocket(ctx)
	require.False(t, ok, "no mount at the socket's path")

	mounts = append(mounts, map[string]any{"Type": "bind", "Source": "/run/user/1000/docker.sock", "Destination": sock})
	got, ok := a.OwnSocket(ctx)
	require.True(t, ok)
	require.Equal(t, "/run/user/1000/docker.sock", got.Source)
	require.NotEmpty(t, got.Group, "the socket's group, as this container sees it")
	require.Empty(t, socketGroup(filepath.Join(dir, "missing")))
}
