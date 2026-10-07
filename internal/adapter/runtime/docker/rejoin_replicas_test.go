package docker

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR025_AReplicaRejoinsOnlyNetworksAnAppContainerIsOn asserts R-025 with
// several Pando replicas (issue #72). Each replica is joined to every app
// network, so a deleted app's network still has endpoints — the other
// replicas. Joining it on that account would keep it from ever emptying and
// ReclaimNetworks could never collect it; only a network one of the app's own
// containers is on is joined.
func TestR025_AReplicaRejoinsOnlyNetworksAnAppContainerIsOn(t *testing.T) {
	f, a := newFakeDaemon(t, map[string]any{"proxy_container": "pando-2"})
	networks := map[string]map[string]any{
		// Only the other replica is on it: its app was deleted.
		"n-deleted": {"Name": "pando-app_deleted", "Labels": map[string]string{labelManaged: "true", labelBundle: "app_deleted"},
			"Containers": map[string]any{"pando-1": map[string]string{"Name": "pando-1"}}},
		// The app's own container is on it.
		"n-live": {"Name": "pando-app_live", "Labels": map[string]string{labelManaged: "true", labelBundle: "app_live"},
			"Containers": map[string]any{"web": map[string]string{"Name": "web"}}},
	}
	list := make([]any, 0, len(networks))
	for id := range networks {
		list = append(list, map[string]string{"Id": id})
	}
	f.on("GET /networks", respond(http.StatusOK, list))
	f.on("GET /networks/*", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body := map[string]any{"Id": id}
		for k, v := range networks[id] {
			body[k] = v
		}
		writeJSON(w, http.StatusOK, body)
	})
	f.on("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("filters"), "app_live") {
			writeJSON(w, http.StatusOK, []any{map[string]any{
				"Id": "web", "State": "running",
				"NetworkSettings": map[string]any{"Networks": map[string]any{"pando-app_live": map[string]any{}}},
			}})
			return
		}
		writeJSON(w, http.StatusOK, []any{})
	})
	var joined []string
	f.on("POST /networks/*", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		joined = append(joined, parts[len(parts)-2])
		w.WriteHeader(http.StatusOK)
	})

	n, err := a.RejoinNetworks(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"n-live"}, joined, "a network only other replicas are on is left to empty")
}
