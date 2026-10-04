//go:build integration

package docker_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR245_UsageIsReadFromTheRuntime asserts a real reading: a container
// spinning one loop under a half-core limit reports CPU near that limit, its
// memory limit, and the 8 MB it wrote to its own layer.
func TestR245_UsageIsReadFromTheRuntime(t *testing.T) {
	ctx := context.Background()
	a := adapter(t)
	id := "test-usage-" + time.Now().Format("150405")
	cleanup(t, a, id)

	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.True(t, caps.ReportsUsage)

	plan := bundle(id, nil)
	plan.Workloads[0].Command = []string{"sh", "-c", "dd if=/dev/zero of=/big bs=1M count=8 2>/dev/null; while :; do :; done"}
	plan.Workloads[0].Resources = api.ResourcePlan{CPUMillis: 500, MemoryBytes: 64 << 20}
	_, err = a.Apply(ctx, plan)
	require.NoError(t, err)

	var web api.WorkloadUsage
	require.Eventually(t, func() bool {
		reading, err := a.Usage(ctx, api.BundleRef{BundleID: id})
		if err != nil || len(reading.Workloads) != 1 {
			return false
		}
		web = reading.Workloads[0]
		return web.DiskBytes >= 8<<20 && web.CPUMillis > 200
	}, 30*time.Second, time.Second, "last reading: %+v", web)

	require.Equal(t, "web", web.Workload)
	require.True(t, web.Running)
	require.Equal(t, 500, web.CPULimitMillis)
	require.LessOrEqual(t, web.CPUMillis, 650, "the limit holds, give or take a sample")
	require.Equal(t, int64(64<<20), web.MemoryLimitBytes)
	require.Greater(t, web.MemoryBytes, int64(0))

	// The runtime-wide sum the Capacity screen shows counts this workload. At
	// least it: other tests may have workloads running on the same daemon.
	var inUse api.InUse
	require.Eventually(t, func() bool {
		inUse, err = a.InUse(ctx)
		return err == nil && inUse.CPUMillis > 200 && inUse.MemoryBytes > 0
	}, 30*time.Second, time.Second, "last reading: %+v", inUse)
	require.False(t, inUse.Reported.IsZero())
}
