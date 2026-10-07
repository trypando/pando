package docker

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR242_ARedeployCountsWhatTheAppHoldsAsFree asserts RoomFor, which the
// multi-host adapter asks of an app's host: what is left of the host, plus
// what the app's own running containers hold there, since a redeploy
// replaces them. A redeploy that fits in place is never refused.
func TestR242_ARedeployCountsWhatTheAppHoldsAsFree(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /info", respond(http.StatusOK, map[string]any{"NCPU": 4, "MemTotal": 8 << 30}))
	f.on("GET /containers/json", respond(http.StatusOK, []map[string]any{
		{"Id": "a", "Labels": map[string]string{labelBundle: "app_1", labelLimitCPU: "1000", labelLimitMemory: "1073741824"}},
		{"Id": "b", "Labels": map[string]string{labelBundle: "app_2", labelLimitCPU: "500", labelLimitMemory: "536870912"}},
		{"Id": "gone", "Labels": map[string]string{labelBundle: "app_2"}}, // not inspectable: counts nothing
	}))

	room, err := a.RoomFor(context.Background(), "app_1")
	require.NoError(t, err)
	require.Equal(t, api.Fit{CPUMillis: 4000 - 1500 + 1000, MemoryBytes: 8<<30 - 1<<30 - 512<<20 + 1<<30}, *room)

	room, err = a.RoomFor(context.Background(), "app_new")
	require.NoError(t, err)
	require.Equal(t, api.Fit{CPUMillis: 2500, MemoryBytes: 8<<30 - 1<<30 - 512<<20}, *room)

	committed, err := a.Committed(context.Background())
	require.NoError(t, err)
	require.Equal(t, api.Fit{CPUMillis: 1500, MemoryBytes: 1<<30 + 512<<20}, committed)

	// On one host the planner's totals check says it all.
	fit, err := a.LargestFitFor(context.Background(), "app_1")
	require.NoError(t, err)
	require.Nil(t, fit)
}

func TestRoomForIsUnknownWithoutTotalsAndAnErrorWithoutDocker(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /info", respond(http.StatusOK, map[string]any{"NCPU": 0, "MemTotal": 0}))
	room, err := a.RoomFor(context.Background(), "app_1")
	require.NoError(t, err)
	require.Nil(t, room, "a host that cannot say how big it is is not refused on a guess")

	// The containers cannot be listed: nothing is known to be free.
	f.on("GET /info", respond(http.StatusOK, map[string]any{"NCPU": 4, "MemTotal": 8 << 30}))
	f.on("GET /containers/json", respond(http.StatusInternalServerError, map[string]string{"message": "boom"}))
	room, err = a.RoomFor(context.Background(), "app_1")
	require.NoError(t, err)
	require.Nil(t, room)
	_, err = a.Committed(context.Background())
	require.Error(t, err)

	f.on("GET /info", respond(http.StatusInternalServerError, map[string]string{"message": "boom"}))
	_, err = a.RoomFor(context.Background(), "app_1")
	require.Error(t, err)
}

func TestBundlesFailWhenDockerDoesNotAnswer(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /networks", respond(http.StatusInternalServerError, map[string]string{"message": "boom"}))
	_, err := a.Bundles(context.Background())
	require.Error(t, err)

	f.on("GET /networks", respond(http.StatusOK, []map[string]any{}))
	f.on("GET /volumes", respond(http.StatusInternalServerError, map[string]string{"message": "boom"}))
	_, err = a.Bundles(context.Background())
	require.Error(t, err)
}

func TestDetachProxyReportsWhatDockerRefuses(t *testing.T) {
	f, a := newFakeDaemon(t, map[string]any{"proxy_container": "pando-agent"})
	f.on("GET /networks", respond(http.StatusInternalServerError, map[string]string{"message": "boom"}))
	require.Error(t, a.DetachProxy(context.Background(), "app_1"))

	f.on("GET /networks", respond(http.StatusOK, []map[string]any{
		{"Id": "n1", "Name": "pando-app_1", "Labels": map[string]string{labelBundle: "app_1", labelManaged: "true"}},
	}))
	f.on("POST /networks/*", respond(http.StatusInternalServerError, map[string]string{"message": "driver failed"}))
	err := a.DetachProxy(context.Background(), "app_1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "detach the host agent")

	// Already detached: not an error, and the network is still removed.
	f.on("POST /networks/*", respond(http.StatusInternalServerError, map[string]string{"message": "container pando-agent is not connected to network"}))
	f.on("DELETE /networks/*", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	require.NoError(t, a.DetachProxy(context.Background(), "app_1"))
	require.Equal(t, 1, f.called("DELETE /networks/n1"))
}
