package multidocker

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// cores is n CPUs with two GiB of memory each, so the worked example reads
// in CPUs.
func cores(n int) amount {
	return amount{CPUMillis: n * 1000, MemoryBytes: int64(n) * 2 * gib}
}

func roomHost(name string, total, active amount) hostRoom {
	return hostRoom{Name: name, Total: total, Free: total.minus(active), Open: true}
}

// place adds a standby for app on primary, the way the standby code would:
// on the chosen host, or nowhere.
func (r *reservation) place(t *testing.T, app, primary string, need amount, over bool) (string, *noRoom) {
	t.Helper()
	host, miss := chooseReserve(*r, standbyAsk{App: app, Primary: primary, Need: need, Oversubscribe: over})
	if miss == nil {
		r.standbys = append(r.standbys, standbyLoad{App: app, Primary: primary, Host: host, Need: need})
	}
	return host, miss
}

// TestR422_TheWorkedExampleKeepsRoomByPairOfHosts asserts the design's
// example (notes-cluster.md, Capacity): A at 12 CPUs, B and C at 32. Losing
// B needs most of B's load free on C, losing A costs little, and C keeps
// the larger of the two, not their sum.
func TestR422_TheWorkedExampleKeepsRoomByPairOfHosts(t *testing.T) {
	r := reservation{hosts: []hostRoom{
		roomHost("a", cores(12), cores(6)),
		roomHost("b", cores(32), cores(24)),
		roomHost("c", cores(32), cores(8)),
	}}
	// B's apps go where most room is left for B's loss: C until what C
	// has left for it is less than A's 6.
	for i, want := range []string{"c", "c", "c", "c", "c", "a"} {
		host, miss := r.place(t, fmt.Sprintf("b-%d", i), "b", cores(4), false)
		require.Nil(t, miss)
		require.Equal(t, want, host, "B's app %d", i)
	}
	for i := range 3 {
		host, miss := r.place(t, fmt.Sprintf("a-%d", i), "a", cores(2), false)
		require.Nil(t, miss)
		require.Equal(t, "c", host, "C's room for A's loss is not B's: only one host is lost at a time")
	}
	require.Empty(t, r.shortfalls())

	sum := r.summary()
	require.Equal(t, "c", sum[2].Name)
	require.Equal(t, cores(20).CPUMillis, sum[2].Largest.CPUMillis, "the largest, not the sum")
	d := sum[2].details()
	kept := d["kept_free_for"].([]map[string]any)
	require.Len(t, kept, 2)
	require.Equal(t, "a", kept[0]["lost_host"])
	require.Equal(t, "b's failover needs 20.00 cores and 40.0 GB free on c.", kept[1]["summary"])
	require.Equal(t, 20000, d["reserved_cpu_millis"])
	require.Equal(t, cores(4).CPUMillis, sum[0].Largest.CPUMillis, "losing B costs A little")

	// B's next app fills C's room for B's loss, and the one after has none.
	host, miss := r.place(t, "b-6", "b", cores(4), false)
	require.Nil(t, miss)
	require.Equal(t, "c", host)
	_, miss = r.place(t, "b-7", "b", cores(4), false)
	require.NotNil(t, miss)
	require.Equal(t, "a", miss.Host, "the largest room any host has for B's loss")
	require.Equal(t, cores(2).CPUMillis, miss.Room.CPUMillis)
	require.Empty(t, r.shortfalls())
}

// TestR422_AnyOneHostLostFitsOnTheRest asserts the invariant over random
// clusters: however standbys are asked for, after each is placed every
// host's apps' standbys fit what each other host has free, and no host's
// disk holds more standby copies than it has.
func TestR422_AnyOneHostLostFitsOnTheRest(t *testing.T) {
	rng := rand.New(rand.NewPCG(422, 1))
	for trial := range 200 {
		r := randomCluster(rng)
		placed, missed := 0, 0
		for i := range 60 {
			primary := r.hosts[rng.IntN(len(r.hosts))].Name
			need := amount{
				CPUMillis:   250 * (1 + rng.IntN(16)),
				MemoryBytes: int64(1+rng.IntN(16)) * gib / 2,
				DiskBytes:   standbyDisk(int64(rng.IntN(8))*gib, 0),
			}
			if _, miss := r.place(t, fmt.Sprintf("app-%d", i), primary, need, false); miss != nil {
				missed++
			} else {
				placed++
			}
			require.Empty(t, r.shortfalls(), "trial %d after app %d", trial, i)
		}
		for _, s := range r.standbys {
			require.NotEqual(t, s.Primary, s.Host, "a standby is never on its app's own host")
		}
		require.Equal(t, 60, placed+missed)
	}
}

func randomCluster(rng *rand.Rand) reservation {
	var r reservation
	for i := range 2 + rng.IntN(5) {
		total := amount{
			CPUMillis:   1000 * (2 + rng.IntN(31)),
			MemoryBytes: int64(2+rng.IntN(63)) * gib,
			DiskBytes:   int64(20+rng.IntN(500)) * gib,
		}
		active := amount{
			CPUMillis:   rng.IntN(total.CPUMillis + 1),
			MemoryBytes: rng.Int64N(total.MemoryBytes + 1),
		}
		h := roomHost(fmt.Sprintf("h%d", i), total, active)
		h.Open = rng.IntN(6) != 0
		r.hosts = append(r.hosts, h)
	}
	return r
}

// TestR422_AStandbyWithNoRoomIsANoticeNamingTheLargestRoom asserts that a
// standby that fits nowhere is placed nowhere, with a notice that has the
// numbers, and that the choice is not an error: the app still deploys.
func TestR422_AStandbyWithNoRoomIsANoticeNamingTheLargestRoom(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}},
		"app-1":   {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 1000, MemoryBytes: 2 * gib}},
		"app-2":   {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 500, MemoryBytes: 1 * gib}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1", "app-2")
	req := standbyRequest{App: "app_big", Primary: "control", Need: api.Fit{CPUMillis: 2000, MemoryBytes: 4 * gib}}

	choice, err := a.chooseStandbyHost(context.Background(), req, nil)
	require.NoError(t, err, "no room is the app's notice, never a refusal")
	require.Nil(t, choice.Host)
	require.NotNil(t, choice.NoRoom)
	require.Equal(t, errs.CapacityWouldOversubscribe, choice.NoRoom.Code)
	require.Equal(t,
		"This app runs without a standby: it needs 2.00 cores of CPU, 4.0 GB of memory and 0 MB of disk on a host other than control, kept free for when control is lost, and the most any host has is 1.00 cores of CPU, 2.0 GB of memory and 0 MB of disk, on app-1.",
		choice.NoRoom.Message)
	require.Equal(t, "app-1", choice.NoRoom.Details["largest_room_host"])
	require.NotEmpty(t, choice.NoRoom.Remedy)

	// One that fits goes to the roomiest other host.
	req.Need = api.Fit{CPUMillis: 500, MemoryBytes: gib}
	choice, err = a.chooseStandbyHost(context.Background(), req, nil)
	require.NoError(t, err)
	require.Nil(t, choice.NoRoom)
	require.Equal(t, "app-1", choice.Host.cfg.Name)
}

// TestR422_NoOtherHostTakingStandbysIsSaidSo asserts the notice when the
// app's host is the only one open, and the error when the others are only
// unreachable, which says nothing about room.
func TestR422_NoOtherHostTakingStandbysIsSaidSo(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}},
		"app-1":   {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1")
	a.hosts[1].cfg.NoPlacement = true
	req := standbyRequest{App: "app_x", Primary: "control", Need: api.Fit{CPUMillis: 100, MemoryBytes: gib}}

	choice, err := a.chooseStandbyHost(context.Background(), req, nil)
	require.NoError(t, err)
	require.Nil(t, choice.Host)
	require.Contains(t, choice.NoRoom.Message, "no Docker host other than control takes new apps")

	a.hosts[1].cfg.NoPlacement = false
	hosts["app-1"].down = true
	_, err = a.chooseStandbyHost(context.Background(), req, nil)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
}

// TestR422_ExistingReservationsAreKept asserts that a new app's standby
// never takes room an existing standby reserves, and that nothing already
// placed is moved or dropped to make room for it — even when a host has
// since shrunk below what it keeps.
func TestR422_ExistingReservationsAreKept(t *testing.T) {
	r := reservation{hosts: []hostRoom{
		roomHost("a", cores(16), cores(8)),
		roomHost("b", cores(16), cores(8)),
	}}
	_, miss := r.place(t, "old", "a", cores(8), false)
	require.Nil(t, miss)
	before := append([]standbyLoad(nil), r.standbys...)

	_, miss = r.place(t, "new", "a", cores(1), false)
	require.NotNil(t, miss, "b's room for a's loss is taken")
	require.Equal(t, before, r.standbys, "the existing standby is kept as it was")

	// B shrinks: the existing standby no longer fits, and it is still not
	// given up; a new one is still not placed past it.
	r.hosts[1].Free = cores(4)
	require.Len(t, r.shortfalls(), 1)
	_, miss = r.place(t, "newer", "a", cores(1), false)
	require.NotNil(t, miss)
	require.Equal(t, before, r.standbys)

	// An app on b takes room on a, which keeps nothing for b yet.
	host, miss := r.place(t, "on-b", "b", cores(8), false)
	require.Nil(t, miss)
	require.Equal(t, "a", host)
}

// TestR422_FailoverOversubscriptionSkipsCPUAndMemoryButNeverDisk asserts
// host policy allow_failover_oversubscription: standbys go past the CPU and
// memory kept free, and never past a host's disk.
func TestR422_FailoverOversubscriptionSkipsCPUAndMemoryButNeverDisk(t *testing.T) {
	disk := func(n int64) amount { return amount{DiskBytes: n * gib} }
	r := reservation{hosts: []hostRoom{
		roomHost("a", cores(8).plus(disk(100)), cores(8)),
		roomHost("b", cores(8).plus(disk(100)), cores(8)),
	}}
	need := cores(4).plus(disk(standbyDisk(20, 0)))
	require.Equal(t, int64(60)*gib, need.DiskBytes, "three copies when the standby cannot say")

	_, miss := r.place(t, "x", "a", need, false)
	require.NotNil(t, miss, "b has no CPU or memory free")

	host, miss := r.place(t, "x", "a", need, true)
	require.Nil(t, miss)
	require.Equal(t, "b", host)

	_, miss = r.place(t, "y", "a", need, true)
	require.NotNil(t, miss, "b's disk holds 60 GB of copies and has 40 GB left")
	require.Equal(t, int64(40)*gib, miss.Room.DiskBytes)
	r.hosts[1].Free.DiskBytes = 10 * gib
	short := r.shortfalls()
	require.Len(t, short, 2, "the oversubscribed CPU and memory, and the disk")
	require.Equal(t, shortfall{On: "b", Need: amount{DiskBytes: 60 * gib}, Free: r.hosts[1].Free}, short[0])

	require.Equal(t, int64(5), standbyDisk(1, 5), "what the standby reports, when it can")
}

// TestR422_AtFailoverAppsStartByPriorityThenName asserts the order a host
// starts a lost host's apps in when not all fit: highest priority first,
// then by app, each started if it fits what is left.
func TestR422_AtFailoverAppsStartByPriorityThenName(t *testing.T) {
	standbys := []standbyLoad{
		{App: "zeta", Need: cores(4), Priority: 0},
		{App: "alpha", Need: cores(4), Priority: 0},
		{App: "big", Need: cores(8), Priority: 5},
		{App: "vip", Need: cores(2), Priority: 10},
		{App: "tiny", Need: cores(1), Priority: -1},
	}
	start, left := failoverOrder(standbys, cores(8), cores(32))
	names := func(s []standbyLoad) []string {
		var out []string
		for _, x := range s {
			out = append(out, x.App)
		}
		return out
	}
	require.Equal(t, []string{"vip", "alpha", "tiny"}, names(start))
	require.Equal(t, []string{"big", "zeta"}, names(left))

	start, left = failoverOrder(standbys, amount{}, amount{})
	require.Len(t, start, 5, "a host that reports no totals is not refused on a guess")
	require.Empty(t, left)
}

// TestR243_CapacityDetailsShowWhatEachHostKeepsFreeForFailover asserts that
// the reservation reaches Capacity's details, per host, through the path
// core already shows as it is (R-243).
func TestR243_CapacityDetailsShowWhatEachHostKeepsFreeForFailover(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}},
		"app-1":   {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1")

	c, err := a.Capacity(context.Background())
	require.NoError(t, err)
	require.NotContains(t, c.Details["hosts"].([]map[string]any)[1], "failover", "no standbys, nothing reserved")

	a.standbys = func(context.Context) ([]standbyLoad, error) {
		return []standbyLoad{{App: "app_1", Primary: "control", Host: "app-1", Need: amount{CPUMillis: 1000, MemoryBytes: gib, DiskBytes: 3 * gib}}}, nil
	}
	c, err = a.Capacity(context.Background())
	require.NoError(t, err)
	rows := c.Details["hosts"].([]map[string]any)
	f := rows[1]["failover"].(map[string]any)
	require.Equal(t, 1000, f["reserved_cpu_millis"])
	require.Equal(t, gib, f["reserved_memory_bytes"])
	require.Equal(t, 3*gib, f["standby_disk_bytes"])
	require.Equal(t, "control's failover needs 1.00 cores and 1.0 GB free on app-1.",
		f["kept_free_for"].([]map[string]any)[0]["summary"])
	require.Empty(t, rows[0]["failover"].(map[string]any)["kept_free_for"])
}

// diskHost is a fake host that reports a disk total.
type diskHost struct {
	*fakeHost
	disk int64
}

func (d diskHost) Capacity(ctx context.Context) (api.Capacity, error) {
	c, err := d.fakeHost.Capacity(ctx)
	c.TotalDiskBytes = d.disk
	return c, err
}

// TestR242_AStandbysCopiesAreCountedOnItsHostsDisk asserts that the
// adapter counts the disk a host reports against standby copies.
func TestR242_AStandbysCopiesAreCountedOnItsHostsDisk(t *testing.T) {
	free := api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}
	hosts := map[string]*fakeHost{"control": {total: free, free: free}, "app-1": {total: free, free: free}}
	a := newTestAdapter(t, hosts, "control", "app-1")
	a.hosts[1].rt = diskHost{hosts["app-1"], 10 * gib}

	req := standbyRequest{App: "app_x", Primary: "control", Need: api.Fit{CPUMillis: 100, MemoryBytes: gib},
		DiskBytes: 6 * gib, AllowOversubscription: true}
	choice, err := a.chooseStandbyHost(context.Background(), req, nil)
	require.NoError(t, err)
	require.Equal(t, "app-1", choice.Host.cfg.Name)

	existing := []standbyLoad{{App: "app_y", Primary: "control", Host: "app-1", Need: amount{DiskBytes: 6 * gib}}}
	choice, err = a.chooseStandbyHost(context.Background(), req, existing)
	require.NoError(t, err)
	require.Nil(t, choice.Host, "disk is never oversubscribed, whatever policy says")
	require.Contains(t, choice.NoRoom.Message, "4.0 GB of disk")
}
