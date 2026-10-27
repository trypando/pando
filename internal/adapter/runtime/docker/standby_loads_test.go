package docker

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

func standbyPlanLabels() api.BundlePlan {
	return api.BundlePlan{BundleID: "app_a-standby", Labels: map[string]string{
		"pando.app": "app_a", "pando.standby": "app_a", "pando.standby.primary": "app-1",
		"pando.standby.priority": "5", "pando.standby.disk": "300",
	}}
}

// TestR422_AStandbysContainersSayWhatItIsFor asserts that a standby's plan
// labels reach its containers, and that a container made for another primary
// is not taken for the standby the plan asks for.
func TestR422_AStandbysContainersSayWhatItIsFor(t *testing.T) {
	p := standbyPlanLabels()
	want := map[string]string{
		"io.pando.standby": "app_a", "io.pando.standby.primary": "app-1",
		"io.pando.standby.priority": "5", "io.pando.standby.disk": "300",
	}
	require.Equal(t, want, standbyLabels(p))
	require.Empty(t, standbyLabels(api.BundlePlan{Labels: map[string]string{"pando.app": "app_a"}}), "an app's own plan has none")

	have := map[string]string{"io.pando.bundle": "app_a-standby"}
	for k, v := range want {
		have[k] = v
	}
	require.True(t, sameStandbyLabels(have, p))
	have["io.pando.standby.primary"] = "app-2"
	require.False(t, sameStandbyLabels(have, p))
	delete(have, "io.pando.standby.primary")
	require.False(t, sameStandbyLabels(have, p))
	require.False(t, sameStandbyLabels(map[string]string{"io.pando.standby.extra": "x"}, api.BundlePlan{}))
	require.True(t, sameStandbyLabels(map[string]string{"io.pando.bundle": "app_a"}, api.BundlePlan{}))
}

// TestR422_StandbyLoadsSumsEachStandbysContainers asserts what the failover
// reservation reads of one host: each standby once, its workloads' limits
// summed, stopped containers included.
func TestR422_StandbyLoadsSumsEachStandbysContainers(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	labels := func(bundle, cpu, mem string) map[string]string {
		return map[string]string{
			labelStandby: bundle, labelStandbyPrimary: "app-1", labelStandbyPriority: "2", labelStandbyDisk: "900",
			labelLimitCPU: cpu, labelLimitMemory: mem,
		}
	}
	f.on("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "1", r.URL.Query().Get("all"), "a standby is stopped")
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"Id": "1", "State": "created", "Labels": labels("app_b", "500", "1000")},
			map[string]any{"Id": "2", "State": "created", "Labels": labels("app_a", "250", "2000")},
			map[string]any{"Id": "3", "State": "created", "Labels": labels("app_a", "250", "2000")},
			map[string]any{"Id": "4", "State": "created", "Labels": map[string]string{labelStandby: ""}},
		})
	})
	loads, err := a.StandbyLoads(context.Background())
	require.NoError(t, err)
	require.Equal(t, []StandbyLoad{
		{Bundle: "app_a", Primary: "app-1", Priority: 2, Need: api.Fit{CPUMillis: 500, MemoryBytes: 4000}, DiskBytes: 900},
		{Bundle: "app_b", Primary: "app-1", Priority: 2, Need: api.Fit{CPUMillis: 500, MemoryBytes: 1000}, DiskBytes: 900},
	}, loads)

	f.on("GET /containers/json", respond(http.StatusInternalServerError, map[string]string{"message": "down"}))
	_, err = a.StandbyLoads(context.Background())
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
}
