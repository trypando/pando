package multidocker

import (
	"context"
	"sync"

	"github.com/trypando/pando/internal/adapter/api"
)

// standbyLister lists the standbys on the hosts, for the failover
// reservation (R-422). The standby code sets it; nil is no standbys yet,
// which reserves nothing.
type standbyLister func(ctx context.Context) ([]standbyLoad, error)

// roomOf is a host as the reservation sees it, from what its daemon
// reported. Free CPU and memory are what is left after its running
// containers' limits, which leave stopped standbys out. Free disk is the
// configured total: the adapter does not know what a host's running apps
// hold on disk, so standby copies are counted against the whole of it.
func roomOf(h *host, c api.Capacity, answered bool) hostRoom {
	r := hostRoom{
		Name:  h.cfg.Name,
		Total: amount{CPUMillis: c.TotalCPUMillis, MemoryBytes: c.TotalMemoryBytes, DiskBytes: c.TotalDiskBytes},
		Open:  answered && !h.cfg.NoPlacement,
	}
	r.Free = r.Total
	if c.LargestFit != nil {
		r.Free.CPUMillis, r.Free.MemoryBytes = c.LargestFit.CPUMillis, c.LargestFit.MemoryBytes
	}
	return r
}

// readRooms asks every host for its capacity, at once, and returns each as
// the reservation sees it, in host order. A host that does not answer is
// not open.
func (a *Adapter) readRooms(ctx context.Context) []hostRoom {
	var mu sync.Mutex
	readings := map[string]api.Capacity{}
	results := a.eachHost(ctx, func(ctx context.Context, h *host) error {
		c, err := h.rt.Capacity(ctx)
		if err == nil {
			mu.Lock()
			readings[h.cfg.Name] = c
			mu.Unlock()
		}
		return err
	})
	rooms := make([]hostRoom, 0, len(a.hosts))
	for _, h := range a.hosts {
		rooms = append(rooms, roomOf(h, readings[h.cfg.Name], results[h.cfg.Name] == nil))
	}
	return rooms
}

// failoverDetails adds each host's reservation to its row of Capacity's
// details, so the console can say what each host keeps free and for whom
// (R-243). Nothing is added while there are no standbys or they cannot be
// listed: the reservation is then nothing, which the rows already say.
func (a *Adapter) failoverDetails(ctx context.Context, rows []map[string]any) {
	if a.standbys == nil {
		return
	}
	loads, err := a.standbys(ctx)
	if err != nil || len(loads) == 0 {
		return
	}
	r := reservation{standbys: loads}
	for _, h := range a.hosts {
		r.hosts = append(r.hosts, hostRoom{Name: h.cfg.Name})
	}
	for i, s := range r.summary() {
		if i < len(rows) {
			rows[i]["failover"] = s.details()
		}
	}
}
