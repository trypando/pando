package detect_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// TestR099_AHostDataDirectoryBecomesAManagedVolume asserts R-099's rewrite
// half. A host directory the app keeps data in becomes a volume of the app's
// own, as a relative one does; the host's clock is dropped; what reaches the
// host's own machinery is still refused.
func TestR099_AHostDataDirectoryBecomesAManagedVolume(t *testing.T) {
	const file = `
services:
  www:
    image: lovasoa/wbo
    volumes:
      - /opt/app/server-data:/opt/app/server-data
      - /path/to/local/dir/for/sqlite/db:/database
      - /etc/localtime:/etc/localtime:ro
`
	draft, err := detect.ImportCompose(memSource{"compose.yaml": file}, "compose.yaml")
	require.NoError(t, err)
	www := draft.Workloads[0]
	var paths []string
	for _, m := range www.Mounts {
		paths = append(paths, m.Path)
	}
	require.ElementsMatch(t, []string{"/opt/app/server-data", "/database"}, paths)
	require.Len(t, draft.Volumes, 2)
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "starts empty"))
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "time zone"))

	for _, refused := range []string{
		"/var/run/docker.sock:/var/run/docker.sock",
		"/etc/ssl/certs:/etc/ssl/certs:ro",
		"/srv/app/config.yml:/app/config.yml",
		"/:/host",
	} {
		_, err := detect.ImportCompose(memSource{"compose.yaml": "services:\n  web:\n    image: nginx\n    volumes:\n      - " + refused + "\n"}, "compose.yaml")
		require.Error(t, err, refused)
	}
}

// TestR023_AComposeFilesOwnSocketProxyIsLeftOut asserts R-023: Pando's proxy
// gives the app its address, so a Traefik that routes by reading the runtime
// socket has nothing to do, and the stack is imported without it rather than
// refused for the socket.
func TestR023_AComposeFilesOwnSocketProxyIsLeftOut(t *testing.T) {
	const file = `
services:
  proxy:
    image: traefik:3.0
    ports: ["80:80"]
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
  backend:
    build: backend
    depends_on: [proxy]
    labels:
      - traefik.http.routers.backend.rule=PathPrefix("/api")
`
	draft, err := detect.ImportCompose(memSource{"compose.yaml": file, "backend/Dockerfile": "FROM golang\n"}, "compose.yaml")
	require.NoError(t, err)
	byName := workloadsByName(draft)
	require.Len(t, byName, 1)
	require.Contains(t, byName, "backend")
	require.Empty(t, byName["backend"].DependsOn, "nothing waits for a service that never starts")
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "reverse proxy"))

	// Alone, it is the app, and the socket is refused as before.
	_, err = detect.ImportCompose(memSource{"compose.yaml": "services:\n  proxy:\n    image: traefik\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n"}, "compose.yaml")
	require.Error(t, err)
}

// TestR131_ADatabaseWithExtensionsIsNotSwappedForAStockOne asserts R-131: a
// compose database Pando provisions is replaced by its stock image, so one
// whose image carries extensions the app needs is run as written.
func TestR131_ADatabaseWithExtensionsIsNotSwappedForAStockOne(t *testing.T) {
	const file = `
services:
  app:
    image: example/app
    environment:
      DATABASE_URL: postgres://u:p@postgres:5432/app
  postgres:
    image: timescale/timescaledb:latest-pg17
`
	draft, err := detect.ImportCompose(memSource{"compose.yaml": file}, "compose.yaml")
	require.NoError(t, err)
	require.Contains(t, workloadsByName(draft), "postgres")
	require.False(t, hasSlot(draft.Slots, spec.SlotPostgres))
}
