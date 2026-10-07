package docker

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
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

// committedCache is the last reading of what is committed.
type committedCache struct {
	mu       sync.Mutex
	at       time.Time
	total    api.Fit
	byBundle map[string]api.Fit
}

// Committed is the CPU and memory every running app container on this daemon
// is limited to, summed: what R-242's arithmetic counts as taken, from the
// containers' own limits rather than from Pando's database, so each host of a
// multi-host install answers for itself. A container with no limit commits
// nothing. Running only, as R-242 counts only running apps: a stopped app
// holds nothing, and counting it would refuse deploys to make room for
// something that is not there.
func (a *Adapter) Committed(ctx context.Context) (api.Fit, error) {
	total, _, err := a.readCommitted(ctx)
	return total, err
}

// CommittedBy is what one bundle's running containers are limited to.
func (a *Adapter) CommittedBy(ctx context.Context, bundleID string) (api.Fit, error) {
	_, byBundle, err := a.readCommitted(ctx)
	return byBundle[bundleID], err
}

func (a *Adapter) readCommitted(ctx context.Context) (api.Fit, map[string]api.Fit, error) {
	a.committed.mu.Lock()
	defer a.committed.mu.Unlock()
	if !a.committed.at.IsZero() && time.Since(a.committed.at) < committedTTL {
		return a.committed.total, a.committed.byBundle, nil
	}

	list, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		Filters: make(client.Filters).Add("label", labelBundle),
	})
	if err != nil {
		return api.Fit{}, nil, err
	}
	var total api.Fit
	byBundle := map[string]api.Fit{}
	for _, c := range list.Items {
		if c.Labels[labelTrial] != "" {
			continue
		}
		var used api.Fit
		cpu, cpuOK := c.Labels[labelLimitCPU]
		mem, memOK := c.Labels[labelLimitMemory]
		if cpuOK && memOK {
			used.CPUMillis, _ = strconv.Atoi(cpu)
			used.MemoryBytes, _ = strconv.ParseInt(mem, 10, 64)
		} else {
			inspect, err := a.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
			if err != nil || inspect.Container.HostConfig == nil {
				continue
			}
			used.CPUMillis = int(inspect.Container.HostConfig.NanoCPUs / 1_000_000)
			used.MemoryBytes = inspect.Container.HostConfig.Memory
		}
		total.CPUMillis += used.CPUMillis
		total.MemoryBytes += used.MemoryBytes
		b := byBundle[c.Labels[labelBundle]]
		b.CPUMillis += used.CPUMillis
		b.MemoryBytes += used.MemoryBytes
		byBundle[c.Labels[labelBundle]] = b
	}
	a.committed.total, a.committed.byBundle, a.committed.at = total, byBundle, time.Now()
	return total, byBundle, nil
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

// LargestFitFor is nil on one host: it is one place, and R-242's check of
// the totals, which already leaves out the app being planned, says
// everything this would. The multi-host adapter asks RoomFor instead.
func (a *Adapter) LargestFitFor(context.Context, string) (*api.Fit, error) { return nil, nil }

// RoomFor is what this host has free for a bundle: what is left of the
// totals, plus what the bundle's own running containers hold, since a
// redeploy replaces them. Nil when the totals are not known.
func (a *Adapter) RoomFor(ctx context.Context, bundleID string) (*api.Fit, error) {
	c, err := a.Capacity(ctx)
	if err != nil {
		return nil, err
	}
	if c.LargestFit == nil {
		return nil, nil
	}
	own, err := a.CommittedBy(ctx, bundleID)
	if err != nil {
		return nil, err
	}
	return &api.Fit{
		CPUMillis:   c.LargestFit.CPUMillis + own.CPUMillis,
		MemoryBytes: c.LargestFit.MemoryBytes + own.MemoryBytes,
	}, nil
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

// DetachProxy disconnects the container this adapter joins to app networks
// from a torn-down bundle's networks, and removes them, so they do not wait
// for that container to be replaced before ReclaimNetworks can collect them.
//
// Only for a container named in ProxyContainer — the multi-host adapter's
// forwarding agent on an app host — and never for Pando's own: disconnecting a
// running container drops its published ports on Docker Desktop, which is why
// Destroy leaves Pando attached (see Destroy). The agent's published port is
// on a network outside the app range, which carries no bundle label and is
// never touched here: only a network labeled as this bundle's is.
func (a *Adapter) DetachProxy(ctx context.Context, bundleID string) error {
	if a.config.ProxyContainer == "" || bundleID == "" {
		return errs.New(errs.Internal, "Pando does not detach its own container from app networks.")
	}
	for _, name := range []string{bundleNetworkName(bundleID), internalNetworkName(bundleID)} {
		n, ok, err := a.findNetwork(ctx, name)
		if err != nil {
			return err
		}
		if !ok || n.Labels[labelBundle] != bundleID || n.Labels[labelManaged] != "true" {
			continue
		}
		_, err = a.cli.NetworkDisconnect(ctx, n.ID, client.NetworkDisconnectOptions{Container: a.config.ProxyContainer})
		if err != nil && !cerrdefs.IsNotFound(err) && !strings.Contains(err.Error(), "is not connected") {
			return errs.Wrap(errs.AdapterFailed, "Could not detach the host agent from a deleted app's network.", err)
		}
		// Anything still attached keeps it; ReclaimNetworks collects it later.
		_, _ = a.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
	}
	return nil
}
