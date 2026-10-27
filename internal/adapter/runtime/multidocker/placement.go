package multidocker

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Placement (R-256) [P], as notes-multi-host-docker-issue-72.md sets out:
//
//  1. Sticky. An app on a host stays there. The record is on the host — the
//     app's network, made first in Apply, and its volumes, which outlive it
//     (R-204, R-206) — and this map is only a cache of it, rebuilt by asking
//     every host. The adapter writes nothing of Pando's (R-027).
//  2. A new app goes to the host open to new apps with the most free memory
//     that fits the whole bundle, by committed limits; ties go to the host
//     with fewer apps, then to the one listed first.
//  3. A bundle never spans hosts.
//  4. No host fits: a CAPACITY_* refusal naming the largest free space.
//
// An app is never moved by the adapter, and Pando offers no action that moves
// one (O-46). Nothing here runs on a timer that could.

// placements is the cache of which host each bundle is on.
type placements struct {
	mu       sync.RWMutex
	byBundle map[string]string // bundle -> host name
	// placedAt is when this process placed a bundle, which its host may not
	// show for a moment.
	placedAt map[string]time.Time
	// unreachable are the hosts that did not answer the last refresh. While
	// any is, a bundle the map does not know might be on it.
	unreachable []string
	refreshed   time.Time
	// ambiguous are bundles several hosts hold that nothing settles (no
	// promotion, no home mark, not where this process had them): located
	// nowhere, and reported, rather than guessed (R-153).
	ambiguous map[string][]string

	// refreshMu makes refreshes one at a time; placeMu makes placements
	// one at a time in this process, so two deploys here never both take the
	// same free space. Two replicas still can (the note accepts it).
	refreshMu sync.Mutex
	placeMu   sync.Mutex
}

// refreshEvery bounds how often a miss asks every host again.
const refreshEvery = 2 * time.Second

// refresh asks every host which bundles it holds. A host that does not answer
// keeps what the map last knew of it, so its apps stay located there and are
// reported unobservable rather than absent.
func (a *Adapter) refresh(ctx context.Context) {
	a.placement.refreshMu.Lock()
	defer a.placement.refreshMu.Unlock()
	a.refreshLocked(ctx)
}

func (a *Adapter) refreshLocked(ctx context.Context) {
	var mu sync.Mutex
	found := map[string]map[string]bool{}
	results := a.eachHost(ctx, func(ctx context.Context, h *host) error {
		bundles, err := h.rt.Bundles(ctx)
		if err != nil {
			return err
		}
		mu.Lock()
		found[h.cfg.Name] = bundles
		mu.Unlock()
		return nil
	})

	homes := map[string]string{}
	a.preferHomes(ctx, homes, found) // standby.go: a promoted app's home wins (R-419)

	a.placement.mu.Lock()
	defer a.placement.mu.Unlock()
	holders := map[string][]string{}
	var unreachable []string
	for _, h := range a.hosts {
		if results[h.cfg.Name] != nil {
			unreachable = append(unreachable, h.cfg.Name)
			for b, on := range a.placement.byBundle {
				if on == h.cfg.Name {
					holders[b] = append(holders[b], on)
				}
			}
			continue
		}
		for b := range found[h.cfg.Name] {
			holders[b] = append(holders[b], h.cfg.Name)
		}
	}
	next, ambiguous := a.settle(holders, homes)
	// A bundle placed here and not yet visible on its host (its network is
	// about to be made) is kept.
	for b, on := range a.placement.byBundle {
		if _, ok := next[b]; !ok && a.placement.pending(b) {
			next[b] = on
		}
	}
	a.placement.byBundle = next
	a.placement.ambiguous = ambiguous
	a.placement.unreachable = unreachable
	a.placement.refreshed = time.Now()
}

// settle is where each bundle is from the hosts that hold it. One host is
// the answer. Several are not something placement makes: a promotion this
// process made or the newest home mark settles it (homes, preferHomes);
// otherwise the host this process had it on, when that is one of them.
// Anything else is ambiguous, never resolved by the order hosts are listed
// in: guessing could start the stale copy a failover left (R-153, R-419).
// Called with mu held.
func (a *Adapter) settle(holders map[string][]string, homes map[string]string) (map[string]string, map[string][]string) {
	next := map[string]string{}
	ambiguous := map[string][]string{}
	for b, on := range holders {
		if home, ok := homes[b]; ok {
			next[b] = home
			continue
		}
		if len(on) == 1 {
			next[b] = on[0]
			continue
		}
		if prev, ok := a.placement.byBundle[b]; ok && slices.Contains(on, prev) {
			next[b] = prev
			continue
		}
		ambiguous[b] = on
	}
	for b, on := range homes {
		next[b] = on
	}
	return next, ambiguous
}

// pending reports a bundle placed in the last minute, which its host may not
// show yet. Called with mu held.
func (p *placements) pending(bundle string) bool {
	at, ok := p.placedAt[bundle]
	return ok && time.Since(at) < time.Minute
}

// locate returns the bundle's host, or nil when no host has it and every host
// answered. When a host did not answer, an unknown bundle might be on it, and
// that is an error rather than "nowhere": answering "nowhere" would have the
// reconciler create the app again on another host.
func (a *Adapter) locate(ctx context.Context, bundle string) (*host, error) {
	if h := a.cached(bundle); h != nil {
		return h, nil
	}
	a.placement.refreshMu.Lock()
	if h := a.cached(bundle); h != nil {
		a.placement.refreshMu.Unlock()
		return h, nil
	}
	a.placement.mu.RLock()
	fresh := time.Since(a.placement.refreshed) < refreshEvery
	a.placement.mu.RUnlock()
	if !fresh {
		a.refreshLocked(ctx)
	}
	a.placement.refreshMu.Unlock()

	if h := a.cached(bundle); h != nil {
		return h, nil
	}
	a.placement.mu.RLock()
	unreachable := append([]string(nil), a.placement.unreachable...)
	several := a.placement.ambiguous[bundle]
	a.placement.mu.RUnlock()
	if len(several) > 0 {
		return nil, errs.Newf(errs.AdapterUnavailable,
			"This app is on %s, and nothing says which of them is where it runs now, so Pando does not start or change it on either.",
			strings.Join(several, " and ")).
			WithRemedy("Mark the host the app was failed over from lost again to start its standby, or remove the copy that is not the app's from that host.")
	}
	if len(unreachable) > 0 {
		return nil, errs.Newf(errs.AdapterUnavailable,
			"Pando cannot reach Docker on %s, so it cannot tell whether this app runs there.",
			strings.Join(unreachable, ", ")).
			WithRemedy("Bring the host back, or remove it from the Docker hosts runtime's host list if it is gone for good. Its apps' data is then restored from backups.")
	}
	return nil, nil
}

// mustLocate is locate for an operation that needs the bundle to exist.
func (a *Adapter) mustLocate(ctx context.Context, bundle string) (*host, error) {
	h, err := a.locate(ctx, bundle)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, errs.New(errs.AdapterFailed, "This app is not running on any Docker host.")
	}
	return h, nil
}

func (a *Adapter) cached(bundle string) *host {
	a.placement.mu.RLock()
	name, ok := a.placement.byBundle[bundle]
	a.placement.mu.RUnlock()
	if !ok {
		return nil
	}
	return a.byName(name)
}

func (a *Adapter) forget(bundle string) {
	a.placement.mu.Lock()
	delete(a.placement.byBundle, bundle)
	delete(a.placement.placedAt, bundle)
	a.placement.mu.Unlock()
}

// need is what a bundle's workloads are limited to, summed: the room it
// takes on one host.
func need(p api.BundlePlan) api.Fit {
	var n api.Fit
	for _, w := range p.Workloads {
		n.CPUMillis += w.Resources.CPUMillis
		n.MemoryBytes += w.Resources.MemoryBytes
	}
	return n
}

// candidate is one host's room for a new app.
type candidate struct {
	host  *host
	free  api.Fit
	known bool // the host reported totals, so free means something
	apps  int
	// disk is what is left of the host's disk after its running apps' disk
	// limits and its standbys' copies, when it reports a total (diskKnown).
	// Disk is never oversubscribed (R-242, R-422).
	disk      int64
	diskKnown bool
}

// place chooses a host for a bundle no host has. Called only after locate
// found it on no host with every host answering.
func (a *Adapter) place(ctx context.Context, p api.BundlePlan) (*host, error) {
	a.placement.placeMu.Lock()
	defer a.placement.placeMu.Unlock()

	// Another deploy in this process may have placed it while this one
	// waited.
	if h := a.cached(p.BundleID); h != nil {
		return h, nil
	}

	loads, _ := a.standbyLoads(ctx) // never an error: a host that does not answer is left out
	copies := copiesOn(loads)
	var mu sync.Mutex
	var candidates []candidate
	results := a.eachHost(ctx, func(ctx context.Context, h *host) error {
		if h.cfg.NoPlacement {
			return nil
		}
		c, err := h.rt.Capacity(ctx)
		if err != nil {
			return err
		}
		bundles, err := h.rt.Bundles(ctx)
		if err != nil {
			return err
		}
		used, err := h.rt.CommittedDisk(ctx)
		if err != nil {
			return err
		}
		cand := candidate{host: h, apps: len(bundles)}
		if c.LargestFit != nil {
			cand.free, cand.known = *c.LargestFit, true
		}
		if c.TotalDiskBytes > 0 {
			cand.disk, cand.diskKnown = c.TotalDiskBytes-used-copies[h.cfg.Name], true
		}
		mu.Lock()
		candidates = append(candidates, cand)
		mu.Unlock()
		return nil
	})
	var down []string
	for _, h := range a.hosts {
		if results[h.cfg.Name] != nil {
			down = append(down, h.cfg.Name)
		}
	}
	// A first deploy has nothing on any host, so the hosts that answer are
	// enough to choose from (BundlePlan.FirstDeploy).
	if len(down) > 0 && !p.FirstDeploy {
		// Placing now could put a second copy of an app beside one on the
		// host that did not answer.
		return nil, errs.Newf(errs.AdapterUnavailable,
			"Pando cannot reach Docker on %s, so it cannot choose a host for this app.", strings.Join(down, ", ")).
			WithRemedy("Bring the host back, or remove it from the Docker hosts runtime's host list.")
	}

	chosen, err := choose(candidates, need(p), diskLimit(p), a.hosts)
	if err != nil {
		return nil, err
	}
	a.placement.mu.Lock()
	a.placement.byBundle[p.BundleID] = chosen.cfg.Name
	if a.placement.placedAt == nil {
		a.placement.placedAt = map[string]time.Time{}
	}
	a.placement.placedAt[p.BundleID] = time.Now()
	a.placement.mu.Unlock()
	return chosen, nil
}

// fitsIn reports whether a bundle needing n fits the candidate's CPU and
// memory. A host that cannot say is not refused on a guess.
func (c candidate) fitsIn(n api.Fit) bool {
	return !c.known || (n.CPUMillis <= c.free.CPUMillis && n.MemoryBytes <= c.free.MemoryBytes)
}

// diskFor reports whether disk fits what is left of the candidate's disk.
func (c candidate) diskFor(disk int64) bool { return !c.diskKnown || disk <= c.disk }

// choose applies the rule: fits, then most free memory, then fewest apps,
// then the order hosts are listed in. Fitting counts the app's disk limit
// against what is left of each host's disk (R-242).
func choose(candidates []candidate, n api.Fit, disk int64, order []*host) (*host, error) {
	rank := map[*host]int{}
	for i, h := range order {
		rank[h] = i
	}
	var ok []candidate
	for _, c := range candidates {
		if c.fitsIn(n) && c.diskFor(disk) {
			ok = append(ok, c)
		}
	}
	if len(ok) == 0 {
		if len(candidates) == 0 {
			return nil, errs.New(errs.AdapterUnavailable, "No Docker host is open to new apps.").
				WithRemedy("Open a host to new apps in the Docker hosts runtime's settings, or add one.")
		}
		if err := noDisk(candidates, n, disk); err != nil {
			return nil, err
		}
		largest := candidates[0]
		for _, c := range candidates[1:] {
			if c.free.MemoryBytes > largest.free.MemoryBytes {
				largest = c
			}
		}
		return nil, errs.Newf(errs.CapacityWouldOversubscribe,
			"No Docker host has room for this app: it asks for %s of memory and %s of CPU, and the most any one host has free is %s of memory and %s of CPU, on %s.",
			bytes(n.MemoryBytes), millis(n.CPUMillis), bytes(largest.free.MemoryBytes), millis(largest.free.CPUMillis), largest.host.cfg.Name).
			WithDetail("largest_free_memory_bytes", largest.free.MemoryBytes).
			WithDetail("largest_free_cpu_millis", largest.free.CPUMillis).
			WithRemedy("Lower what the app asks for, stop or delete an app on a host, or add a host.")
	}
	sort.SliceStable(ok, func(i, j int) bool {
		a, b := ok[i], ok[j]
		if a.free.MemoryBytes != b.free.MemoryBytes {
			return a.free.MemoryBytes > b.free.MemoryBytes
		}
		if a.apps != b.apps {
			return a.apps < b.apps
		}
		return rank[a.host] < rank[b.host]
	})
	return ok[0].host, nil
}

// noDisk is the refusal when some host has the CPU and memory for the app
// and none of those has the disk: disk is never oversubscribed (R-242).
// Nil when no host has the CPU and memory either.
func noDisk(candidates []candidate, n api.Fit, disk int64) error {
	var best *candidate
	for i, c := range candidates {
		if c.fitsIn(n) && (best == nil || c.disk > best.disk) {
			best = &candidates[i]
		}
	}
	if best == nil {
		return nil
	}
	return errs.Newf(errs.CapacityWouldOversubscribe,
		"No Docker host has the disk for this app: it may use %s, and the most any host with room for it has left is %s, on %s, after its apps and the standby copies it keeps.",
		bytes(disk), bytes(max(best.disk, 0)), best.host.cfg.Name).
		WithDetail("largest_free_disk_bytes", best.disk).
		WithRemedy("Lower the app's disk limit, delete an app or its standby on a host, or add a host.")
}

func bytes(n int64) string {
	const gib = 1 << 30
	const mib = 1 << 20
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GB", float64(n)/gib)
	default:
		return fmt.Sprintf("%d MB", n/mib)
	}
}

func millis(n int) string {
	return fmt.Sprintf("%.2f cores", float64(n)/1000)
}
