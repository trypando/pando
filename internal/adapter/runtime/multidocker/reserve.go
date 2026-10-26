package multidocker

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/trypando/pando/internal/errs"
)

// Failover capacity (R-422), as notes-cluster.md "Capacity" sets out:
// standbys reserve room so that any one host can be lost and its apps fit
// on the others.
//
// The check is per pair of hosts. For a lost host H and another host G, the
// standbys on G of the apps that run on H start on G when H is lost, so
// together they must fit what G has free beside its own running apps. Only
// one host is lost at a time, so what G keeps free for H and what it keeps
// free for another host are the same room, and G keeps the largest of them.
// That is how hosts of different sizes come out right: with A at 12 CPUs and
// B and C at 32, losing B needs most of B's load free on C, and losing A
// costs little anywhere.
//
// A standby is created and stopped, so it holds no CPU or memory until it
// starts: what a host has free (its running containers' limits subtracted)
// already leaves standbys out. Disk is different. A standby's copies are on
// its host's disk all the time, so they are counted against it always, and
// policy never lets disk be oversubscribed (R-242).
//
// Everything here is pure: it is handed what the hosts reported and decides.
// A total of 0 is a resource the host does not report, which is not checked,
// as the planner does not check it (R-242).

// standbyCopies is how many copies of a volume a standby host keeps, and so
// how much disk a standby takes when it cannot say what its copies hold
// [P: notes-cluster.md, Data].
const standbyCopies = 3

// amount is some CPU, memory and disk.
type amount struct {
	CPUMillis   int
	MemoryBytes int64
	DiskBytes   int64
}

func (a amount) plus(b amount) amount {
	return amount{a.CPUMillis + b.CPUMillis, a.MemoryBytes + b.MemoryBytes, a.DiskBytes + b.DiskBytes}
}

func (a amount) minus(b amount) amount {
	return amount{a.CPUMillis - b.CPUMillis, a.MemoryBytes - b.MemoryBytes, a.DiskBytes - b.DiskBytes}
}

// hostRoom is one host as the reservation sees it.
type hostRoom struct {
	Name string

	// Total is what the host has; a field of 0 is not reported.
	Total amount

	// Free is what is left of Total after the apps running on the host, by
	// their committed limits, and after the disk they hold. Standbys are not
	// subtracted here: the reservation counts them.
	Free amount

	// Open is whether the host takes new standbys: open to new apps and
	// answering.
	Open bool
}

// standbyLoad is one standby: the app it is for, the host the app runs on,
// the host the standby is on, and what it takes. Need's CPU and memory are
// what the app's workloads are limited to, taken only when the standby
// starts; its disk is what the standby's copies hold, taken all the time.
type standbyLoad struct {
	App      string
	Primary  string
	Host     string
	Need     amount
	Priority int
}

// standbyDisk is the disk a standby's copies are counted at: what they hold
// when the standby can say (reported > 0), otherwise three copies of what
// the app may use [P].
func standbyDisk(appDiskBytes, reportedBytes int64) int64 {
	if reportedBytes > 0 {
		return reportedBytes
	}
	return standbyCopies * appDiskBytes
}

// reservation is the hosts and the standbys already on them.
type reservation struct {
	hosts    []hostRoom
	standbys []standbyLoad
}

// reserved is the CPU and memory the standbys on host `on` of the apps that
// run on `lost` take when `lost` is lost.
func (r reservation) reserved(lost, on string) amount {
	var out amount
	for _, s := range r.standbys {
		if s.Primary == lost && s.Host == on {
			out.CPUMillis += s.Need.CPUMillis
			out.MemoryBytes += s.Need.MemoryBytes
		}
	}
	return out
}

// copiesOn is the disk every standby on a host holds.
func (r reservation) copiesOn(on string) int64 {
	var out int64
	for _, s := range r.standbys {
		if s.Host == on {
			out += s.Need.DiskBytes
		}
	}
	return out
}

// countOn is how many standbys a host holds.
func (r reservation) countOn(on string) int {
	n := 0
	for _, s := range r.standbys {
		if s.Host == on {
			n++
		}
	}
	return n
}

// roomFor is what host g still has for a standby of an app that runs on
// primary: CPU and memory past what g keeps for primary's loss, and disk
// past every standby copy g holds.
func (r reservation) roomFor(primary string, g hostRoom) amount {
	held := r.reserved(primary, g.Name)
	held.DiskBytes = r.copiesOn(g.Name)
	return g.Free.minus(held)
}

// fits reports whether need fits room on a host with these totals. CPU and
// memory are skipped when oversubscription is allowed; disk never is.
func fits(need, room, total amount, oversubscribe bool) bool {
	if total.DiskBytes > 0 && need.DiskBytes > room.DiskBytes {
		return false
	}
	if oversubscribe {
		return true
	}
	if total.CPUMillis > 0 && need.CPUMillis > room.CPUMillis {
		return false
	}
	return total.MemoryBytes <= 0 || need.MemoryBytes <= room.MemoryBytes
}

// standbyAsk is a new app's standby, to be placed.
type standbyAsk struct {
	App     string
	Primary string
	Need    amount

	// Oversubscribe is host policy allow_failover_oversubscription: CPU and
	// memory may be placed past the reservation. Disk never is.
	Oversubscribe bool
}

// option is one host's room for a standby.
type option struct {
	host  hostRoom
	room  amount
	count int
	order int
}

// noRoom is why a standby fits nowhere, with the largest room any host has,
// for the app's notice. Never a blocker: the app deploys without one.
type noRoom struct {
	Need    amount
	Primary string

	// Largest is the host with the most room, and its room; empty Host when
	// no host other than the app's own takes standbys.
	Host string
	Room amount
}

// envelope is noRoom as the error envelope, for whoever turns it into a
// notice: its message is the notice's text, held to R-105, with the numbers. Its code is CAPACITY_WOULD_OVERSUBSCRIBE because that is what
// placing it would do; the caller must not refuse the deploy for it.
func (n *noRoom) envelope() *errs.Error {
	if n.Host == "" {
		return errs.Newf(errs.CapacityWouldOversubscribe,
			"This app runs without a standby: no Docker host other than %s takes new apps, so none can hold one.", n.Primary).
			WithRemedy("Add a host, or open another host to new apps in the Docker hosts runtime's settings.")
	}
	return errs.Newf(errs.CapacityWouldOversubscribe,
		"This app runs without a standby: it needs %s of CPU, %s of memory and %s of disk on a host other than %s, kept free for when %s is lost, and the most any host has is %s of CPU, %s of memory and %s of disk, on %s.",
		millis(n.Need.CPUMillis), bytes(n.Need.MemoryBytes), bytes(n.Need.DiskBytes), n.Primary, n.Primary,
		millis(max(n.Room.CPUMillis, 0)), bytes(max(n.Room.MemoryBytes, 0)), bytes(max(n.Room.DiskBytes, 0)), n.Host).
		WithDetail("largest_room_host", n.Host).
		WithDetail("largest_room_cpu_millis", n.Room.CPUMillis).
		WithDetail("largest_room_memory_bytes", n.Room.MemoryBytes).
		WithDetail("largest_room_disk_bytes", n.Room.DiskBytes).
		WithRemedy("Lower what the app asks for, stop or delete an app on another host, or add a host. Host policy allow_failover_oversubscription places standbys past the reservation.")
}

// chooseReserve picks the host for a new standby: one other than the app's
// own, open, where it fits. Among those, the most room left (memory, then
// CPU), then the fewest standbys, then the order hosts are listed in. A
// standby already placed is never moved or dropped to make room (R-422).
func chooseReserve(r reservation, ask standbyAsk) (string, *noRoom) {
	var options []option
	for i, g := range r.hosts {
		if !g.Open || g.Name == ask.Primary {
			continue
		}
		options = append(options, option{host: g, room: r.roomFor(ask.Primary, g), count: r.countOn(g.Name), order: i})
	}
	slices.SortStableFunc(options, func(a, b option) int {
		return cmp.Or(
			cmp.Compare(b.room.MemoryBytes, a.room.MemoryBytes),
			cmp.Compare(b.room.CPUMillis, a.room.CPUMillis),
			cmp.Compare(a.count, b.count),
			cmp.Compare(a.order, b.order))
	})
	for _, o := range options {
		if fits(ask.Need, o.room, o.host.Total, ask.Oversubscribe) {
			return o.host.Name, nil
		}
	}
	miss := &noRoom{Need: ask.Need, Primary: ask.Primary}
	if len(options) > 0 {
		miss.Host, miss.Room = options[0].host.Name, options[0].room
	}
	return "", miss
}

// shortfall is a pair of hosts the reservation does not cover: when Lost is
// lost, its apps' standbys on On need Need and On has Free.
type shortfall struct {
	Lost, On string
	Need     amount
	Free     amount
}

// shortfalls is every pair of hosts, and every host's disk, where the
// reservation does not hold. Empty is the invariant R-422 keeps.
func (r reservation) shortfalls() []shortfall {
	var out []shortfall
	for _, g := range r.hosts {
		if disk := r.copiesOn(g.Name); g.Total.DiskBytes > 0 && disk > g.Free.DiskBytes {
			out = append(out, shortfall{On: g.Name, Need: amount{DiskBytes: disk}, Free: g.Free})
		}
		for _, h := range r.hosts {
			if h.Name == g.Name {
				continue
			}
			need := r.reserved(h.Name, g.Name)
			if !fits(need, g.Free, amount{CPUMillis: g.Total.CPUMillis, MemoryBytes: g.Total.MemoryBytes}, false) {
				out = append(out, shortfall{Lost: h.Name, On: g.Name, Need: need, Free: g.Free})
			}
		}
	}
	return out
}

// keptFor is what one host keeps free for the loss of another.
type keptFor struct {
	Lost string
	amount
}

// hostReserve is one host's part in the reservation, for Capacity's details
// (R-243): what it keeps free for each other host's loss, the most of those,
// and the disk its standbys' copies hold.
type hostReserve struct {
	Name    string
	For     []keptFor
	Largest amount
	Copies  int64
}

// summary is each host's reservation, in host order. A host keeping nothing
// for another is left out of For.
func (r reservation) summary() []hostReserve {
	out := make([]hostReserve, 0, len(r.hosts))
	for _, g := range r.hosts {
		row := hostReserve{Name: g.Name, Copies: r.copiesOn(g.Name)}
		for _, h := range r.hosts {
			kept := r.reserved(h.Name, g.Name)
			if h.Name == g.Name || (kept.CPUMillis == 0 && kept.MemoryBytes == 0) {
				continue
			}
			row.For = append(row.For, keptFor{Lost: h.Name, amount: kept})
			row.Largest.CPUMillis = max(row.Largest.CPUMillis, kept.CPUMillis)
			row.Largest.MemoryBytes = max(row.Largest.MemoryBytes, kept.MemoryBytes)
		}
		out = append(out, row)
	}
	return out
}

// details renders the summary for Capacity.Details: Docker hosts' own
// shape, shown and never interpreted by core (R-243).
func (h hostReserve) details() map[string]any {
	kept := make([]map[string]any, 0, len(h.For))
	for _, k := range h.For {
		kept = append(kept, map[string]any{
			"lost_host":    k.Lost,
			"cpu_millis":   k.CPUMillis,
			"memory_bytes": k.MemoryBytes,
			"summary":      fmt.Sprintf("%s's failover needs %s and %s free on %s.", k.Lost, millis(k.CPUMillis), bytes(k.MemoryBytes), h.Name),
		})
	}
	return map[string]any{
		"kept_free_for":         kept,
		"reserved_cpu_millis":   h.Largest.CPUMillis,
		"reserved_memory_bytes": h.Largest.MemoryBytes,
		"standby_disk_bytes":    h.Copies,
	}
}

// failoverOrder is which of a lost host's standbys on one host start, when
// that host has free: highest priority first, then by app, each started if
// it fits what is left. One that does not fit is skipped and the next is
// tried, so a smaller app of lower priority still starts. Used when
// allow_failover_oversubscription let the standbys be placed past what the
// host can hold; under the reservation every one fits.
func failoverOrder(standbys []standbyLoad, free amount, total amount) (start, left []standbyLoad) {
	ordered := slices.Clone(standbys)
	slices.SortStableFunc(ordered, func(a, b standbyLoad) int {
		return cmp.Or(cmp.Compare(b.Priority, a.Priority), cmp.Compare(a.App, b.App))
	})
	total.DiskBytes = 0 // the copies are already on the host's disk
	for _, s := range ordered {
		need := amount{CPUMillis: s.Need.CPUMillis, MemoryBytes: s.Need.MemoryBytes}
		if !fits(need, free, total, false) {
			left = append(left, s)
			continue
		}
		start = append(start, s)
		free = free.minus(need)
	}
	return start, left
}
