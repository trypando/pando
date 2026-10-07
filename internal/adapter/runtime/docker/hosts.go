package docker

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
)

// What the multi-host Docker adapter (internal/adapter/runtime/multidocker)
// needs from this one, which it runs once per host rather than duplicating
// (notes-multi-host-docker-issue-72.md): an adapter on a client it made, what
// a host has committed, and which bundles a host holds.

// Labels recording a workload's limits where a container list can read them,
// so what a host has committed is one list rather than an inspect per
// container. A container made before these existed is inspected instead.
const (
	labelLimitCPU    = "io.pando.limit.cpu"
	labelLimitMemory = "io.pando.limit.memory"
)

// committedTTL is how long a reading of committed limits is reused.
// Capacity is called on every plan, and what is committed changes only when a
// deploy creates or removes containers.
const committedTTL = 10 * time.Second

// NewWithClient builds an adapter that talks to Docker through cli, with
// config as Configure would have read it. The multi-host adapter builds one
// per host, on a client authenticated to that host.
func NewWithClient(cli *client.Client, config Config) *Adapter {
	return &Adapter{cli: cli, config: config}
}

// committedCache is the last reading of Committed.
type committedCache struct {
	mu    sync.Mutex
	at    time.Time
	value api.Fit
}

// Committed is the CPU and memory every app container on this daemon is
// limited to, summed: what R-242's arithmetic counts as taken, from the
// containers' own limits rather than from Pando's database, so each host of a
// multi-host install answers for itself. A container with no limit commits
// nothing. Stopped containers count: the reconciler starts them again.
func (a *Adapter) Committed(ctx context.Context) (api.Fit, error) {
	a.committed.mu.Lock()
	defer a.committed.mu.Unlock()
	if !a.committed.at.IsZero() && time.Since(a.committed.at) < committedTTL {
		return a.committed.value, nil
	}

	list, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelBundle),
	})
	if err != nil {
		return api.Fit{}, err
	}
	var total api.Fit
	for _, c := range list.Items {
		if c.Labels[labelTrial] != "" {
			continue
		}
		cpu, cpuOK := c.Labels[labelLimitCPU]
		mem, memOK := c.Labels[labelLimitMemory]
		if cpuOK && memOK {
			n, _ := strconv.Atoi(cpu)
			m, _ := strconv.ParseInt(mem, 10, 64)
			total.CPUMillis += n
			total.MemoryBytes += m
			continue
		}
		inspect, err := a.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil || inspect.Container.HostConfig == nil {
			continue
		}
		total.CPUMillis += int(inspect.Container.HostConfig.NanoCPUs / 1_000_000)
		total.MemoryBytes += inspect.Container.HostConfig.Memory
	}
	a.committed.value, a.committed.at = total, time.Now()
	return total, nil
}

// forgetCommitted drops the cached reading, after this adapter created or
// removed something.
func (a *Adapter) forgetCommitted() {
	a.committed.mu.Lock()
	a.committed.at = time.Time{}
	a.committed.mu.Unlock()
}

// limitLabels records a workload's limits on its container.
func limitLabels(labels map[string]string, cpuMillis int, memoryBytes int64) {
	labels[labelLimitCPU] = strconv.Itoa(cpuMillis)
	labels[labelLimitMemory] = strconv.FormatInt(memoryBytes, 10)
}

// Bundles names every bundle with a network or a volume on this daemon.
//
// The multi-host adapter's record of where an app is (R-256): an app's
// network is made first in Apply, and its volumes outlive it (R-204), so a
// host holding either is the app's host. Nothing about placement is written
// anywhere else (R-027).
func (a *Adapter) Bundles(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	networks, err := a.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", labelBundle),
	})
	if err != nil {
		return nil, err
	}
	for _, n := range networks.Items {
		if n.Labels[labelTrial] != "" {
			continue
		}
		if b := n.Labels[labelBundle]; b != "" {
			out[b] = true
		}
	}
	volumes, err := a.cli.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", labelBundle),
	})
	if err != nil {
		return nil, err
	}
	for _, v := range volumes.Items {
		if b := v.Labels[labelBundle]; b != "" {
			out[b] = true
		}
	}
	return out, nil
}

// largestFit is what is left of the totals after what is committed. On one
// host it is one place, so it is also the largest.
func (a *Adapter) largestFit(ctx context.Context, total api.Capacity) *api.Fit {
	if total.TotalCPUMillis <= 0 && total.TotalMemoryBytes <= 0 {
		return nil
	}
	committed, err := a.Committed(ctx)
	if err != nil {
		return nil
	}
	return &api.Fit{
		CPUMillis:   max(total.TotalCPUMillis-committed.CPUMillis, 0),
		MemoryBytes: max(total.TotalMemoryBytes-committed.MemoryBytes, 0),
	}
}
