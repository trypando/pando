package docker

import (
	"context"
	"net/http"
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
