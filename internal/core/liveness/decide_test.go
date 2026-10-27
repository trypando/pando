package liveness

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/clock"
)

const (
	tPeriod   = DefaultPeriod
	tInterval = DefaultPeriod / 6
	cut       = 60 * time.Second
	long      = 15 * time.Minute
)

// TestR421_ScenariosDecideAsTheDesignTableSays asserts R-421 over every case
// in the design's table and the partitions around it: who stops its apps, and
// who is taken for lost. The expectation in every row that is not a host cut
// off is the same — nobody stops anything.
func TestR421_ScenariosDecideAsTheDesignTableSays(t *testing.T) {
	cases := []struct {
		name   string
		n      int
		leader int
		link   func(a, b int, now time.Duration) bool
		run    func(h int, now time.Duration) bool
		fenced []int
		lost   []int
	}{
		{name: "healthy five", n: 5, leader: 0},
		{name: "healthy three", n: 3, leader: 0},
		{
			// Design table row 1: the control plane gone, app hosts reaching
			// each other. No leader decides anything, and nobody stops.
			name: "control plane and Postgres down, hosts reach each other", n: 5, leader: -1,
		},
		{
			// The control hosts' Pando processes are down but their agents
			// answer: they still count as reached.
			name: "control processes down, agents up", n: 5, leader: -1,
		},
		{
			// Design table row 2.
			name: "one link broken in five", n: 5, leader: 0,
			link: func(a, b int, now time.Duration) bool { return a != 1 || b != 2 || now < cut },
		},
		{
			name: "one link broken in three", n: 3, leader: 2,
			link: func(a, b int, now time.Duration) bool { return a != 0 || b != 1 || now < cut },
		},
		{
			name: "two links of one host broken in five", n: 5, leader: 0,
			link: func(a, b int, now time.Duration) bool { return now < cut || b != 4 || (a != 1 && a != 2) },
		},
		{
			// Design table row 3.
			name: "a host's network is cut", n: 5, leader: 0,
			link:   cutAt([]int{4}, cut, 0),
			fenced: []int{4}, lost: []int{4},
		},
		{
			name: "a host's network is cut in three", n: 3, leader: 0,
			link:   cutAt([]int{2}, cut, 0),
			fenced: []int{2}, lost: []int{2},
		},
		{
			name: "the leader's own host is cut", n: 5, leader: 4,
			link:   cutAt([]int{4}, cut, 0),
			fenced: []int{4},
		},
		{
			name: "2|3 of five, leader with the three", n: 5, leader: 0,
			link:   partition(cut, []int{0, 1, 2}, []int{3, 4}),
			fenced: []int{3, 4}, lost: []int{3, 4},
		},
		{
			name: "2|3 of five, leader with the two", n: 5, leader: 4,
			link:   partition(cut, []int{0, 1, 2}, []int{3, 4}),
			fenced: []int{3, 4},
		},
		{
			name: "1|1|1 of three", n: 3, leader: 0,
			link:   partition(cut, []int{0}, []int{1}, []int{2}),
			fenced: []int{0, 1, 2},
		},
		{
			name: "2|2 of four", n: 4, leader: 0,
			link: partition(cut, []int{0, 1}, []int{2, 3}),
		},
		{
			name: "1|3 of four", n: 4, leader: 0,
			link:   partition(cut, []int{3}, []int{0, 1, 2}),
			fenced: []int{3}, lost: []int{3},
		},
		{
			name: "a host that is turned off", n: 5, leader: 0,
			run:  func(h int, now time.Duration) bool { return h != 3 || now < cut },
			lost: []int{3},
		},
		{
			// Fewer than three hosts: never by itself.
			name: "two hosts, link cut", n: 2, leader: 0,
			link: cutAt([]int{1}, cut, 0),
		},
		{
			name: "two hosts, one off", n: 2, leader: 0,
			run: func(h int, now time.Duration) bool { return h != 1 || now < cut },
		},
		{name: "one host", n: 1, leader: 0},
		{
			// A link that is up once every 20 s — less than T — counts as
			// reached the whole time.
			name: "a flapping host, up every 20 s", n: 5, leader: 0,
			link: func(a, b int, now time.Duration) bool {
				return now < cut || b != 4 || (now/tInterval)%4 == 0
			},
		},
		{
			name: "every link flapping out of step", n: 5, leader: 0,
			link: func(a, b int, now time.Duration) bool {
				return now < cut || (int(now/tInterval)+a+b)%3 != 0
			},
		},
		{
			// Up once every 40 s: more than T without contact, so it fences
			// in each gap, but contact every 40 s is far short of 3T, so it
			// is never lost.
			name: "a flapping host, up every 40 s", n: 5, leader: 0,
			link: func(a, b int, now time.Duration) bool {
				return now < cut || b != 4 || (now/tInterval)%8 == 0
			},
			fenced: []int{4},
		},
		{
			name: "a host that rejoins before T", n: 5, leader: 0,
			link: cutAt([]int{4}, cut, cut+tPeriod/2),
		},
		{
			name: "a host that rejoins after T, before 3T", n: 5, leader: 0,
			link:   cutAt([]int{4}, cut, cut+2*tPeriod),
			fenced: []int{4},
		},
		{
			// A machine suspended for ten minutes: the others take it for
			// lost (it answered nobody for 3T), but when it wakes it reaches
			// them at once and does not stop its apps on account of the
			// minutes it slept through. Its copies are then the returning
			// host's to remove (R-419).
			name: "a host suspended and resumed", n: 5, leader: 0,
			run:  func(h int, now time.Duration) bool { return h != 4 || now < cut || now >= cut+10*time.Minute },
			lost: []int{4},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(tc.n, tc.leader)
			if tc.link != nil {
				s.link = tc.link
			}
			if tc.run != nil {
				s.running = tc.run
			}
			s.run(long)
			require.Equal(t, tc.fenced, s.fenced(), "hosts that stopped their apps")
			require.Equal(t, tc.lost, s.lost(), "hosts taken for lost")
		})
	}
}

// TestR421_ACutOffHostFencesAfterTNotBefore asserts R-421: a host cut off
// stops its apps once it has reached nobody for T, measured from its last
// contact, and not a round sooner.
func TestR421_ACutOffHostFencesAfterTNotBefore(t *testing.T) {
	s := newSim(5, 0)
	s.link = cutAt([]int{4}, cut, 0)
	s.run(long)

	lastContact := cut - tInterval
	require.Equal(t, lastContact+tPeriod+tInterval, s.fencedAt[4],
		"the first round more than T after the last contact")
	require.Greater(t, s.fencedAt[4], lastContact+tPeriod)

	// Not before: replay up to the round before and see nobody fence.
	s2 := newSim(5, 0)
	s2.link = cutAt([]int{4}, cut, 0)
	s2.run(lastContact + tPeriod)
	require.Empty(t, s2.fenced())
}

// TestR421_FailoverOnlyAfter3TByMajority asserts R-421: the leader takes a
// host for lost only after more than half the hosts have not reached it for
// 3T, so the lost host has stopped its apps at least 2T before.
func TestR421_FailoverOnlyAfter3TByMajority(t *testing.T) {
	s := newSim(5, 0)
	s.link = cutAt([]int{4}, cut, 0)
	s.run(long)

	lastContact := cut - tInterval
	require.GreaterOrEqual(t, s.lostAt[4], lastContact+3*tPeriod)
	require.LessOrEqual(t, s.lostAt[4], lastContact+3*tPeriod+s.cfg.Fresh())
	require.GreaterOrEqual(t, s.lostAt[4]-s.fencedAt[4], 2*tPeriod-tInterval,
		"the standby starts long after the cut-off host stopped its apps")
}

// TestR421_AHostReachingAMajorityNeverFences asserts R-421: whatever else
// fails, a host that reaches at least half the cluster does not stop its
// apps. Every pair of broken links in a five-host cluster, and every single
// broken link in four, leaves every host reaching half, and nobody fences.
func TestR421_AHostReachingAMajorityNeverFences(t *testing.T) {
	type link struct{ a, b int }
	var pairs [][2]link
	var all []link
	for a := range 5 {
		for b := a + 1; b < 5; b++ {
			all = append(all, link{a, b})
		}
	}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			pairs = append(pairs, [2]link{all[i], all[j]})
		}
	}
	for _, p := range pairs {
		s := newSim(5, 0)
		s.link = func(a, b int, now time.Duration) bool {
			return now < cut || (link{a, b} != p[0] && link{a, b} != p[1])
		}
		s.run(5 * time.Minute)
		for i, tr := range s.trackers {
			if tr.Reached(s.local(i, 5*time.Minute))*2 >= 5 {
				require.Negative(t, s.fencedAt[i], "host %d reaches a majority with links %v broken", i, p)
			}
		}
		require.Empty(t, s.lost(), "links %v broken", p)
	}
}

// TestR421_FewerThanThreeHostsNeverFenceOrFailOver asserts R-421: with one or
// two hosts, whatever the links do, nothing stops and nothing is lost.
func TestR421_FewerThanThreeHostsNeverFenceOrFailOver(t *testing.T) {
	for n := 1; n <= 2; n++ {
		s := newSim(n, 0)
		s.link = func(int, int, time.Duration) bool { return false }
		s.run(long)
		require.Empty(t, s.fenced())
		require.Empty(t, s.lost())

		tr := NewTracker(Config{})
		tr.SetPeers(0, []string{"a"}[:n-1])
		for now := time.Duration(0); now < long; now += tInterval {
			tr.Round(now, nil)
			require.False(t, tr.MustFence(now))
		}
	}
	l := NewLedger(Config{})
	l.Record(0, "a", map[string]time.Duration{"b": time.Hour})
	require.False(t, l.Lost(0, []string{"a", "b"}, "b"))
}

// TestR421_OneBrokenLinkStopsNothing asserts R-421 (design table row 2):
// every single broken link, in clusters of three to seven hosts.
func TestR421_OneBrokenLinkStopsNothing(t *testing.T) {
	for n := 3; n <= 7; n++ {
		for a := range n {
			for b := a + 1; b < n; b++ {
				s := newSim(n, 0)
				s.link = func(x, y int, now time.Duration) bool { return now < cut || x != a || y != b }
				s.run(5 * time.Minute)
				require.Empty(t, s.fenced(), "n=%d, link %d-%d", n, a, b)
				require.Empty(t, s.lost(), "n=%d, link %d-%d", n, a, b)
			}
		}
	}
}

func TestTrackerWatchesWithoutABreakBeforeFencing(t *testing.T) {
	tr := NewTracker(Config{})
	tr.SetPeers(0, []string{"a", "b", "c", "d"})
	require.Equal(t, 5, tr.Hosts())

	// Probing starts at 0 and nobody answers.
	for now := time.Duration(0); now < tPeriod; now += tInterval {
		tr.Round(now, nil)
		require.False(t, tr.MustFence(now), "at %s", now)
	}
	tr.Round(tPeriod, nil)
	require.True(t, tr.MustFence(tPeriod))

	// A gap longer than MaxGap — the machine slept, the clock jumped —
	// starts the window again.
	resume := tPeriod + time.Hour
	require.Zero(t, tr.Watched(resume))
	require.False(t, tr.MustFence(resume))
	tr.Round(resume, nil)
	require.False(t, tr.MustFence(resume))
	require.Equal(t, time.Duration(0), tr.Watched(resume))
	require.Equal(t, time.Duration(0), tr.UnreachedFor(resume, "a"))
	tr.Round(resume+tPeriod/2, nil)
	require.False(t, tr.MustFence(resume+tPeriod/2))
}

func TestTrackerContactEitherWayCounts(t *testing.T) {
	tr := NewTracker(Config{})
	tr.SetPeers(0, []string{"a", "b", "c", "d"})
	for now := time.Duration(0); now <= 2*tPeriod; now += tInterval {
		// Nobody answers this host's probes, but two peers' probes arrive.
		tr.Contact(now, "a")
		tr.Contact(now, "b")
		tr.Contact(now, "stranger")
		tr.Round(now, nil)
	}
	require.Equal(t, 3, tr.Reached(2*tPeriod))
	require.False(t, tr.MustFence(2*tPeriod))

	// A contact recorded out of order does not move the last contact back.
	tr.Contact(time.Second, "a")
	require.Equal(t, time.Duration(0), tr.UnreachedFor(2*tPeriod, "a"))
	require.Equal(t, 2*tPeriod, tr.UnreachedFor(2*tPeriod, "c"))
	require.Zero(t, tr.UnreachedFor(2*tPeriod, "stranger"))
}

func TestTrackerNewPeerIsNotSilentForLongerThanKnown(t *testing.T) {
	tr := NewTracker(Config{})
	tr.SetPeers(0, []string{"a", "b"})
	for now := time.Duration(0); now <= 5*tPeriod; now += tInterval {
		tr.Round(now, []string{"a", "b"})
	}
	tr.SetPeers(5*tPeriod, []string{"a", "c"})
	require.Equal(t, 3, tr.Hosts())
	tr.Round(5*tPeriod+tInterval, nil)
	u := tr.Unreached(5*tPeriod + tInterval)
	require.Equal(t, map[string]time.Duration{"a": tInterval, "c": tInterval}, u)
}

func TestLedgerCountsOnlyFreshReports(t *testing.T) {
	cfg := Config{}.Normalize()
	l := NewLedger(cfg)
	hosts := []string{"a", "b", "c", "x"}
	silent := map[string]time.Duration{"x": 3 * tPeriod}
	l.Record(0, "a", silent)
	l.Record(0, "b", silent)
	l.Record(0, "x", map[string]time.Duration{"a": 3 * tPeriod})
	require.Equal(t, 2, l.Silent(0, hosts, "x"))
	require.False(t, l.Lost(0, hosts, "x"), "two of four is not more than half")

	l.Record(0, "c", silent)
	require.True(t, l.Lost(0, hosts, "x"))
	require.True(t, l.Lost(cfg.Fresh(), hosts, "x"))
	require.False(t, l.Lost(cfg.Fresh()+time.Millisecond, hosts, "x"), "stale reports say nothing")

	l.Forget("c")
	require.False(t, l.Lost(0, hosts, "x"))

	// Just short of 3T does not count.
	l.Record(0, "c", map[string]time.Duration{"x": 3*tPeriod - time.Millisecond})
	require.False(t, l.Lost(0, hosts, "x"))

	// A report from the leader's future — its clock was held back — does not
	// count either.
	l.Record(time.Hour, "c", silent)
	require.False(t, l.Lost(0, hosts, "x"))
}

func TestConfigNormalize(t *testing.T) {
	c := Config{}.Normalize()
	require.Equal(t, DefaultPeriod, c.Period)
	require.Equal(t, 5*time.Second, c.Interval)
	require.Equal(t, 2500*time.Millisecond, c.Timeout)
	require.Equal(t, 90*time.Second, c.LostAfter())
	require.Equal(t, 15*time.Second, c.MaxGap())
	require.Equal(t, 15*time.Second, c.Fresh())

	c = Config{Period: time.Minute, Interval: time.Minute, Timeout: time.Hour}.Normalize()
	require.Equal(t, 10*time.Second, c.Interval)
	require.Equal(t, 5*time.Second, c.Timeout)

	c = Config{Period: time.Minute, Interval: 2 * time.Second, Timeout: time.Second}.Normalize()
	require.Equal(t, 2*time.Second, c.Interval)
	require.Equal(t, time.Second, c.Timeout)
}

// TestR421_ClockJumpsMoveNothing asserts R-421's "durations are measured on
// each host's own monotonic clock": a clock set backward stops time rather
// than reversing it, and the system clock is read monotonically.
func TestR421_ClockJumpsMoveNothing(t *testing.T) {
	fake := clock.NewFake(time.Time{})
	read := Elapsed(fake)
	require.Zero(t, read())
	fake.Advance(10 * time.Second)
	require.Equal(t, 10*time.Second, read())
	fake.Set(fake.Now().Add(-time.Hour))
	require.Equal(t, 10*time.Second, read(), "backward is held")
	fake.Advance(time.Hour + 5*time.Second)
	require.Equal(t, 15*time.Second, read())

	sys := Elapsed(clock.System{})
	a := sys()
	b := sys()
	require.GreaterOrEqual(t, b, a)

	// A forward jump in the middle of a cut-off window restarts the window
	// rather than completing it.
	tr := NewTracker(Config{})
	tr.SetPeers(0, []string{"a", "b", "c", "d"})
	for now := time.Duration(0); now < tPeriod/2; now += tInterval {
		tr.Round(now, nil)
	}
	jumped := tPeriod/2 + 2*time.Hour
	tr.Round(jumped, nil)
	require.False(t, tr.MustFence(jumped))
	require.Less(t, tr.Watched(jumped), tPeriod)
}
