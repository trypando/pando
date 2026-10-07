package docker

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR243_OneHostsLargestFitIsWhatItsContainersLeave asserts the single-host
// half of Capacity.LargestFit: the totals less what app containers are
// limited to, read from their labels, or from an inspect for a container made
// before the labels existed. Trial containers do not count.
func TestR243_OneHostsLargestFitIsWhatItsContainersLeave(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /info", respond(http.StatusOK, map[string]any{"NCPU": 4, "MemTotal": 8 << 30}))
	f.on("GET /containers/json", respond(http.StatusOK, []map[string]any{
		{"Id": "labeled", "Labels": map[string]string{
			labelBundle: "app_1", labelLimitCPU: "1000", labelLimitMemory: "1073741824"}},
		{"Id": "older", "Labels": map[string]string{labelBundle: "app_2"}},
		{"Id": "trial", "Labels": map[string]string{labelBundle: "app_3", labelTrial: "t1",
			labelLimitCPU: "4000", labelLimitMemory: "8589934592"}},
	}))
	f.on("GET /containers/older/json", respond(http.StatusOK, map[string]any{
		"Id": "older", "HostConfig": map[string]any{"NanoCpus": 500_000_000, "Memory": 512 << 20},
	}))

	c, err := a.Capacity(context.Background())
	require.NoError(t, err)
	require.NotNil(t, c.LargestFit)
	require.Equal(t, api.Fit{CPUMillis: 2500, MemoryBytes: 8<<30 - 1<<30 - 512<<20}, *c.LargestFit)

	// Read once and reused: Capacity is called on every plan.
	_, err = a.Capacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, f.called("GET /containers/json"))
}

func TestBundlesAreReadFromNetworksAndVolumes(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /networks", respond(http.StatusOK, []map[string]any{
		{"Name": "pando-app_1", "Labels": map[string]string{labelBundle: "app_1"}},
		{"Name": "pando-trial-x", "Labels": map[string]string{labelBundle: "app_t", labelTrial: "x"}},
	}))
	f.on("GET /volumes", respond(http.StatusOK, map[string]any{"Volumes": []map[string]any{
		{"Name": "pando-app_2-vol", "Labels": map[string]string{labelBundle: "app_2"}},
	}}))
	got, err := a.Bundles(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"app_1": true, "app_2": true}, got)
}

// TestR224_DetachProxyLeavesOnlyTheDeletedBundlesNetworks asserts the guard
// on detaching the host agent: only networks labeled as the bundle's are
// touched, never one that merely has a matching name, and Pando's own
// container is never detached.
func TestR224_DetachProxyLeavesOnlyTheDeletedBundlesNetworks(t *testing.T) {
	f, a := newFakeDaemon(t, map[string]any{"proxy_container": "pando-agent"})
	f.on("GET /networks", respond(http.StatusOK, []map[string]any{
		{"Id": "n1", "Name": "pando-app_1", "Labels": map[string]string{labelBundle: "app_1", labelManaged: "true"}},
		{"Id": "n2", "Name": "pando-app_1-internal", "Labels": map[string]string{labelBundle: "app_2", labelManaged: "true"}},
		{"Id": "n3", "Name": "pando-agent", "Labels": map[string]string{labelManaged: "true"}},
	}))
	var mu sync.Mutex
	var disconnected, removed []string
	f.on("POST /networks/*", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		disconnected = append(disconnected, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	f.on("DELETE /networks/*", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		removed = append(removed, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	require.NoError(t, a.DetachProxy(context.Background(), "app_1"))
	require.Len(t, disconnected, 1)
	require.Contains(t, disconnected[0], "/networks/n1/disconnect")
	require.Len(t, removed, 1)
	require.Contains(t, removed[0], "/networks/n1")

	// The bundle "agent" would name the agent's own network; it carries no
	// bundle label, so it is left alone.
	disconnected, removed = nil, nil
	require.NoError(t, a.DetachProxy(context.Background(), "agent"))
	require.Empty(t, disconnected)
	require.Empty(t, removed)

	_, self := newFakeDaemon(t, nil)
	require.Error(t, self.DetachProxy(context.Background(), "app_1"), "Pando's own container is never detached")
}
