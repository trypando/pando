package docker

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

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

// TestR245_SamplingIsBoundedOnABusyHost asserts R-245's reading does not open
// a stats call per running container at once: a host of a hundred apps is
// sampled samplingConcurrency at a time (issue #72), and every one is still
// counted.
func TestR245_SamplingIsBoundedOnABusyHost(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	const running = 100
	list := make([]any, 0, running)
	for i := range running {
		list = append(list, map[string]any{"Id": fmt.Sprintf("c%d", i), "State": "running",
			"Labels": map[string]string{labelBundle: fmt.Sprintf("app_%d", i)}})
	}
	f.on("GET /containers/json", respond(http.StatusOK, list))

	var mu sync.Mutex
	inFlight, peak := 0, 0
	f.on("GET /containers/*", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		now := time.Now()
		writeJSON(w, http.StatusOK, map[string]any{
			"read": now, "preread": now.Add(-time.Second),
			"memory_stats": map[string]any{"usage": 1 << 20},
		})
	})

	got, err := a.InUse(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(running)<<20, got.MemoryBytes, "every container is still counted")
	require.LessOrEqual(t, peak, samplingConcurrency)
	require.Greater(t, peak, 1, "and they are still read in parallel")
}
