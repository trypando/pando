package docker

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// volumeSizesFor is how long a reading of volume sizes is reused. Docker
// computes them by walking every volume on the host, which is the one part of
// a usage reading that grows with the host rather than with the app; once a
// minute is often enough for a number that changes slowly.
const volumeSizesFor = time.Minute

// Usage reads what an app's containers are using now (R-245).
//
// CPU is sampled by the daemon over about a second — the stats call without
// streaming waits for a second reading to diff against — so the containers are
// read in parallel, and the whole reading takes about a second however many
// parts the app has. A stopped container reports no CPU or memory, only the
// disk it wrote.
func (a *Adapter) Usage(ctx context.Context, ref api.BundleRef) (api.BundleUsage, error) {
	listed, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelBundle+"="+ref.BundleID),
	})
	if err != nil {
		return api.BundleUsage{}, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}

	// The egress gateway is Pando's, not one of the app's workloads (egress.go).
	workloads := listed.Items[:0]
	for _, c := range listed.Items {
		if !isGateway(c.Labels) {
			workloads = append(workloads, c)
		}
	}
	containers := workloads

	out := api.BundleUsage{Reported: time.Now().UTC(), Workloads: make([]api.WorkloadUsage, len(containers))}
	var wg sync.WaitGroup
	for i, c := range containers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out.Workloads[i] = a.workloadUsage(ctx, c.ID, c.Labels[labelWorkload])
		}()
	}
	wg.Wait()

	vols, err := a.cli.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", labelBundle+"="+ref.BundleID),
	})
	if err == nil {
		sizes := a.volumeSizes(ctx)
		for _, v := range vols.Items {
			size, known := sizes[v.Name]
			if !known {
				size = -1
			}
			out.Volumes = append(out.Volumes, api.VolumeUsage{VolumeID: v.Labels["io.pando.volume"], Bytes: size})
		}
	}
	return out, nil
}

func (a *Adapter) workloadUsage(ctx context.Context, id, name string) api.WorkloadUsage {
	u := api.WorkloadUsage{Workload: name, DiskBytes: -1}

	// With sizes: SizeRw is what the container wrote to its own layer, which
	// is the disk it uses outside its volumes.
	res, err := a.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{Size: true})
	if err != nil {
		return u
	}
	inspect := res.Container
	u.Running = inspect.State != nil && inspect.State.Running
	if inspect.SizeRw != nil {
		u.DiskBytes = *inspect.SizeRw
	}
	if inspect.HostConfig != nil {
		u.CPULimitMillis = int(inspect.HostConfig.NanoCPUs / 1_000_000)
		u.MemoryLimitBytes = inspect.HostConfig.Memory
	}
	if !u.Running {
		return u
	}

	s, ok := a.readStats(ctx, id)
	if !ok {
		return u
	}

	u.CPUMillis = cpuMillis(s)
	u.MemoryBytes = memoryInUse(s.MemoryStats)

	// Docker's one-shot reading carries the previous sample to diff against.
	// Podman's does not — no preread, no previous CPU — and diffed against
	// nothing, CPU came out as its average since the container started over
	// the machine's since it booted: near zero for a loop pinned at its limit.
	// So take the second sample here, a second later, as Docker would have.
	if s.PreRead.IsZero() && s.PreCPUStats.CPUUsage.TotalUsage == 0 {
		select {
		case <-ctx.Done():
			return u
		case <-time.After(statsSampleGap):
		}
		if next, ok := a.readStats(ctx, id); ok {
			u.CPUMillis = cpuMillisBetween(s, next)
			u.MemoryBytes = memoryInUse(next.MemoryStats)
		}
	}
	return u
}

// cpuMillisBetween is the CPU a container used between two readings Pando took
// itself, per second of the wall time between them.
//
// Against wall time rather than the engine's system counter, which is what
// cpuMillis divides by: Docker's counts every CPU's time, so the ratio times
// the CPU count is cores in use, but Podman's (4.9) advances by about the
// container's own use — measured, a loop limited to half a core moved it
// 517ms in a second — and that ratio read as every core the host has. The
// clocks between two readings are the engine's own timestamps, so a slow
// round trip does not stretch the interval.
func cpuMillisBetween(first, second container.StatsResponse) int {
	used := float64(second.CPUStats.CPUUsage.TotalUsage) - float64(first.CPUStats.CPUUsage.TotalUsage)
	wall := float64(second.Read.Sub(first.Read))
	if used <= 0 || wall <= 0 {
		return 0
	}
	return int(used / wall * 1000)
}

// statsSampleGap is how far apart two CPU samples are when the engine does not
// take them itself: Docker's own gap for a one-shot reading.
const statsSampleGap = time.Second

func (a *Adapter) readStats(ctx context.Context, id string) (container.StatsResponse, bool) {
	// One reading, with the daemon's previous sample to diff against. Without
	// IncludePreviousSample the client asks for one-shot, which leaves PreCPUStats
	// empty on Docker too and sends every reading down the Podman path above.
	stats, err := a.cli.ContainerStats(ctx, id, client.ContainerStatsOptions{IncludePreviousSample: true})
	if err != nil {
		return container.StatsResponse{}, false
	}
	defer func() { _ = stats.Body.Close() }()
	var s container.StatsResponse
	if err := json.NewDecoder(stats.Body).Decode(&s); err != nil {
		return container.StatsResponse{}, false
	}
	return s, true
}

// cpuMillis is the CPU a container used between the daemon's two readings, in
// thousandths of a core — the same arithmetic as `docker stats`, which reports
// it as a percentage of one core.
func cpuMillis(s container.StatsResponse) int {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpuDelta <= 0 || sysDelta <= 0 || cpus == 0 {
		return 0
	}
	return int(cpuDelta / sysDelta * cpus * 1000)
}

// memoryInUse is memory the container is using, less the page cache the
// kernel can take back — again as `docker stats` counts it, so the number
// matches what an operator sees there. cgroup v2 calls the reclaimable part
// inactive_file; v1, total_inactive_file.
func memoryInUse(m container.MemoryStats) int64 {
	used := m.Usage
	for _, key := range []string{"inactive_file", "total_inactive_file"} {
		if v, ok := m.Stats[key]; ok && v < used {
			used -= v
			break
		}
	}
	// Unsigned from the daemon, signed on the wire; no machine has 8 EiB.
	if used > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(used)
}

// volumeSizes is every volume's size on the host, by name, reused for
// volumeSizesFor (see there). An error leaves the sizes unknown rather than
// failing the reading: CPU and memory are still worth showing.
func (a *Adapter) volumeSizes(ctx context.Context) map[string]int64 {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	if a.volumeSizesAt.After(time.Now().Add(-volumeSizesFor)) {
		return a.volumeSizesCache
	}
	// Verbose, because from API 1.52 the per-volume list comes back only when
	// asked for; without it every volume read as an unknown size.
	du, err := a.cli.DiskUsage(ctx, client.DiskUsageOptions{Volumes: true, Verbose: true})
	if err != nil {
		return a.volumeSizesCache
	}
	sizes := map[string]int64{}
	for _, v := range du.Volumes.Items {
		if v.UsageData != nil && v.UsageData.Size >= 0 {
			sizes[v.Name] = v.UsageData.Size
		}
	}
	a.volumeSizesCache, a.volumeSizesAt = sizes, time.Now()
	return sizes
}
