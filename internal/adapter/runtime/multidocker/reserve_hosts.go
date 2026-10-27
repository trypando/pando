package multidocker

import (
	"context"
	"fmt"
	"sync"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/errs"
)

// roomToStart refuses to start a standby that does not fit what its host has
// free now, by its workloads' CPU and memory limits (failoverOrder, R-422).
// Under the reservation every standby fits; past it, which host policy
// allow_failover_oversubscription allows, core starts a lost host's apps
// highest priority first, then by app, and this is what skips the ones that
// no longer fit. What the bundle already runs there counts as free, so a
// failover tried again is not refused for its own first try. A host that
// does not report its totals is not checked, as the planner does not check
// it (R-242).
func (a *Adapter) roomToStart(ctx context.Context, target *host, p api.BundlePlan) error {
	c, err := target.rt.Capacity(ctx)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable,
			fmt.Sprintf("Pando could not read what %s has free, so it did not start this app's standby there.", target.cfg.Name), err).
			WithRemedy("Bring the host back, then mark the lost host lost again to start the standby.")
	}
	free, err := target.rt.RoomFor(ctx, p.BundleID)
	if err != nil || free == nil {
		return err
	}
	n := need(p)
	load := standbyLoad{App: p.BundleID, Host: target.cfg.Name, Priority: p.Failover.Priority,
		Need: amount{CPUMillis: n.CPUMillis, MemoryBytes: n.MemoryBytes}}
	total := amount{CPUMillis: c.TotalCPUMillis, MemoryBytes: c.TotalMemoryBytes}
	room := amount{CPUMillis: free.CPUMillis, MemoryBytes: free.MemoryBytes}
	if start, _ := failoverOrder([]standbyLoad{load}, room, total); len(start) == 1 {
		return nil
	}
	return errs.Newf(errs.CapacityWouldOversubscribe,
		"This app's standby was not started on %s: it needs %s of CPU and %s of memory, and %s has %s of CPU and %s of memory free.",
		target.cfg.Name, millis(n.CPUMillis), bytes(n.MemoryBytes), target.cfg.Name,
		millis(max(free.CPUMillis, 0)), bytes(max(free.MemoryBytes, 0))).
		WithDetail("host", target.cfg.Name).
		WithRemedy("Stop or delete an app on " + target.cfg.Name + ", then mark the lost host lost again to start the standby.")
}

// standbyLister lists the standbys on the hosts, for the failover
// reservation (R-422). Nil is listStandbys, which reads them from the hosts;
// tests set their own.
type standbyLister func(ctx context.Context) ([]standbyLoad, error)

// standbyLoads is every standby placed, by Adapter.standbys or else from
// the hosts.
func (a *Adapter) standbyLoads(ctx context.Context) ([]standbyLoad, error) {
	if a.standbys != nil {
		return a.standbys(ctx)
	}
	return a.listStandbys(ctx)
}

// listStandbys reads every host's standbys from their containers' labels
// (docker.StandbyLoads), which ApplyStandby wrote (standbyPlan): the app,
// the host it ran on then, its priority and what it takes. A standby whose
// primary label is missing takes the host placement says the app is on. A
// host that does not answer is left out: it is not open, so it takes no new
// standby, and its own standbys start nowhere while it is down.
func (a *Adapter) listStandbys(ctx context.Context) ([]standbyLoad, error) {
	var mu sync.Mutex
	byHost := map[string][]docker.StandbyLoad{}
	a.eachHost(ctx, func(ctx context.Context, h *host) error {
		side, err := standbyOf(h)
		if err != nil {
			return err
		}
		loads, err := side.StandbyLoads(ctx)
		if err == nil {
			mu.Lock()
			byHost[h.cfg.Name] = loads
			mu.Unlock()
		}
		return err
	})
	var out []standbyLoad
	for _, h := range a.hosts {
		for _, l := range byHost[h.cfg.Name] {
			primary := l.Primary
			if home := a.cached(l.Bundle); primary == "" && home != nil {
				primary = home.cfg.Name
			}
			out = append(out, standbyLoad{
				App: l.Bundle, Primary: primary, Host: h.cfg.Name, Priority: l.Priority,
				Need: amount{CPUMillis: l.Need.CPUMillis, MemoryBytes: l.Need.MemoryBytes, DiskBytes: l.DiskBytes},
			})
		}
	}
	return out, nil
}

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
	loads, err := a.standbyLoads(ctx)
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
