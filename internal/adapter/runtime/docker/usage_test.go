package docker

import (
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
)

// TestR245_CPUIsCountedAsDockerStatsCountsIt: the CPU used between the
// daemon's two readings, as a share of the whole machine times its cores —
// so one busy core is 1000 millis however many the host has.
func TestR245_CPUIsCountedAsDockerStatsCountsIt(t *testing.T) {
	s := container.StatsResponse{}
	s.PreCPUStats.CPUUsage.TotalUsage = 1_000
	s.CPUStats.CPUUsage.TotalUsage = 1_000 + 250_000_000
	s.PreCPUStats.SystemUsage = 0
	s.CPUStats.SystemUsage = 1_000_000_000 // one second across all cores
	s.CPUStats.OnlineCPUs = 4

	require.Equal(t, 1000, cpuMillis(s), "a quarter of four cores is one core")

	// No second reading yet — the first sample of a new container — is no
	// CPU, not a division by zero.
	require.Equal(t, 0, cpuMillis(container.StatsResponse{}))

	// Older daemons report the cores only per CPU.
	s.CPUStats.OnlineCPUs = 0
	s.CPUStats.CPUUsage.PercpuUsage = []uint64{1, 1}
	require.Equal(t, 500, cpuMillis(s))
}

// Memory as `docker stats` shows it: page cache the kernel can reclaim is not
// memory the app is holding.
func TestR245_MemoryLeavesOutReclaimableCache(t *testing.T) {
	require.Equal(t, int64(300), memoryInUse(container.MemoryStats{Usage: 400, Stats: map[string]uint64{"inactive_file": 100}}))
	require.Equal(t, int64(300), memoryInUse(container.MemoryStats{Usage: 400, Stats: map[string]uint64{"total_inactive_file": 100}}))
	require.Equal(t, int64(400), memoryInUse(container.MemoryStats{Usage: 400}))
	// A cache figure larger than usage is a torn reading; usage stands.
	require.Equal(t, int64(400), memoryInUse(container.MemoryStats{Usage: 400, Stats: map[string]uint64{"inactive_file": 900}}))
}
