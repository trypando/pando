//go:build integration

package docker_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR422_AStoppedStandbyIsReadBackForTheReservation asserts, on a real
// daemon, that a standby made by ApplyStopped is listed by StandbyLoads with
// what it is for and what it takes, though none of it runs, and that making
// it again for another primary relabels it rather than keeping the old one.
func TestR422_AStoppedStandbyIsReadBackForTheReservation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	app := "test-sbload-" + time.Now().Format("150405")
	id := app + "-standby"
	cleanup(t, a, id)

	p := bundle(id, nil)
	p.Workloads[0].Resources = api.ResourcePlan{CPUMillis: 250, MemoryBytes: 64 << 20}
	p.Labels["pando.standby"] = app
	p.Labels["pando.standby.primary"] = "app-1"
	p.Labels["pando.standby.priority"] = "3"
	p.Labels["pando.standby.disk"] = "1024"
	require.NoError(t, a.ApplyStopped(ctx, p))

	find := func() (out []string) {
		loads, err := a.StandbyLoads(ctx)
		require.NoError(t, err)
		for _, l := range loads {
			if l.Bundle == app {
				require.Equal(t, api.Fit{CPUMillis: 250, MemoryBytes: 64 << 20}, l.Need)
				require.Equal(t, 3, l.Priority)
				require.Equal(t, int64(1024), l.DiskBytes)
				out = append(out, l.Primary)
			}
		}
		return out
	}
	require.Equal(t, []string{"app-1"}, find())

	observed, err := a.Observe(ctx, api.BundleRef{BundleID: id})
	require.NoError(t, err)
	for _, w := range observed.Workloads {
		require.False(t, w.Running, "a standby is never started (R-153)")
	}

	p.Labels["pando.standby.primary"] = "app-2"
	require.NoError(t, a.ApplyStopped(ctx, p))
	require.Equal(t, []string{"app-2"}, find())
}
