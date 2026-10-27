package liveness

import (
	"fmt"
	"time"
)

// sim runs the decision rules over a cluster of hosts, one probe round per
// Interval, with links and hosts that come and go as a scenario says. Each
// host has its own monotonic clock, offset from the others by an arbitrary
// amount, so a rule that compared one host's time with another's would show.
type sim struct {
	cfg      Config
	n        int
	leader   int // -1: no leader, as with Postgres or every control host down
	link     func(a, b int, now time.Duration) bool
	running  func(h int, now time.Duration) bool
	trackers []*Tracker
	ledger   *Ledger
	offsets  []time.Duration

	fencedAt []time.Duration // first time each host had to fence, or -1
	lostAt   []time.Duration // first time the leader took each host for lost, or -1
}

func hostName(i int) string { return fmt.Sprintf("h%d", i) }

func newSim(n, leader int) *sim {
	s := &sim{
		cfg:     Config{}.Normalize(),
		n:       n,
		leader:  leader,
		link:    func(int, int, time.Duration) bool { return true },
		running: func(int, time.Duration) bool { return true },
	}
	s.ledger = NewLedger(s.cfg)
	for i := range n {
		s.offsets = append(s.offsets, time.Duration(i*i*7919)*time.Second+time.Duration(i)*time.Millisecond)
		s.fencedAt = append(s.fencedAt, -1)
		s.lostAt = append(s.lostAt, -1)
		tr := NewTracker(s.cfg)
		var peers []string
		for j := range n {
			if j != i {
				peers = append(peers, hostName(j))
			}
		}
		tr.SetPeers(s.local(i, 0), peers)
		s.trackers = append(s.trackers, tr)
	}
	return s
}

func (s *sim) local(h int, now time.Duration) time.Duration { return now + s.offsets[h] }

func (s *sim) up(a, b int, now time.Duration) bool {
	return s.running(a, now) && s.running(b, now) && s.link(min(a, b), max(a, b), now)
}

func (s *sim) hosts() []string {
	out := make([]string, s.n)
	for i := range s.n {
		out[i] = hostName(i)
	}
	return out
}

// run steps until end.
func (s *sim) run(end time.Duration) *sim {
	for now := time.Duration(0); now <= end; now += s.cfg.Interval {
		s.step(now)
	}
	return s
}

func (s *sim) step(now time.Duration) {
	for i := range s.n {
		if !s.running(i, now) {
			continue
		}
		var answered []string
		for j := range s.n {
			if j != i && s.up(i, j, now) {
				// The probe arrives, and is answered.
				s.trackers[j].Contact(s.local(j, now), hostName(i))
				answered = append(answered, hostName(j))
			}
		}
		s.trackers[i].Round(s.local(i, now), answered)
		if s.fencedAt[i] < 0 && s.trackers[i].MustFence(s.local(i, now)) {
			s.fencedAt[i] = now
		}
	}
	if s.leader < 0 || !s.running(s.leader, now) {
		return
	}
	lnow := s.local(s.leader, now)
	for j := range s.n {
		if j == s.leader || s.up(s.leader, j, now) {
			s.ledger.Record(lnow, hostName(j), s.trackers[j].Unreached(s.local(j, now)))
		}
	}
	for x := range s.n {
		if s.lostAt[x] < 0 && s.ledger.Lost(lnow, s.hosts(), hostName(x)) {
			s.lostAt[x] = now
		}
	}
}

// Scenario helpers.

// cutAt cuts every link of the hosts in cut from the time at on, until back
// (zero: never).
func cutAt(cut []int, at, back time.Duration) func(a, b int, now time.Duration) bool {
	in := map[int]bool{}
	for _, h := range cut {
		in[h] = true
	}
	return func(a, b int, now time.Duration) bool {
		if now < at || (back > 0 && now >= back) {
			return true
		}
		return !in[a] && !in[b]
	}
}

// partition splits the hosts into groups from at on: a link holds only within
// a group.
func partition(at time.Duration, groups ...[]int) func(a, b int, now time.Duration) bool {
	group := map[int]int{}
	for g, hs := range groups {
		for _, h := range hs {
			group[h] = g
		}
	}
	return func(a, b int, now time.Duration) bool {
		return now < at || group[a] == group[b]
	}
}

func (s *sim) fenced() []int {
	var out []int
	for i, t := range s.fencedAt {
		if t >= 0 {
			out = append(out, i)
		}
	}
	return out
}

func (s *sim) lost() []int {
	var out []int
	for i, t := range s.lostAt {
		if t >= 0 {
			out = append(out, i)
		}
	}
	return out
}
