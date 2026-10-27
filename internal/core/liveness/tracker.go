package liveness

import "time"

// Tracker is one host's own view: when it last had contact with each other
// host, and how long it has been probing without a break. Every time it is
// given is a reading of this host's monotonic clock (Elapsed).
//
// Contact is either direction: this host's probe of a peer was answered, or a
// peer's probe of this host arrived. Counting both is what keeps the two
// sides of a link in agreement, so that a host the others have not reached is
// also a host that has not reached them (R-421).
//
// A Tracker is not safe for concurrent use; Monitor holds it under a lock.
type Tracker struct {
	cfg   Config
	peers map[string]*peerContact

	probing   bool
	runStart  time.Duration
	lastRound time.Duration
}

type peerContact struct {
	added     time.Duration
	contact   time.Duration
	contacted bool
}

// NewTracker returns a Tracker with no peers.
func NewTracker(cfg Config) *Tracker {
	return &Tracker{cfg: cfg.Normalize(), peers: map[string]*peerContact{}}
}

// SetPeers makes ids the other hosts of the cluster. A peer new to the set is
// taken as last reached when it was added, so a host that has just joined is
// not counted as silent for longer than it has been known.
func (t *Tracker) SetPeers(now time.Duration, ids []string) {
	keep := make(map[string]bool, len(ids))
	for _, id := range ids {
		keep[id] = true
		if _, ok := t.peers[id]; !ok {
			t.peers[id] = &peerContact{added: now}
		}
	}
	for id := range t.peers {
		if !keep[id] {
			delete(t.peers, id)
		}
	}
}

// Has reports whether peer is one of the cluster's other hosts here.
func (t *Tracker) Has(peer string) bool {
	_, ok := t.peers[peer]
	return ok
}

// Hosts is the cluster's size as this host knows it, itself included.
func (t *Tracker) Hosts() int { return len(t.peers) + 1 }

// Round records one round of this host's probes, finished at now, and the
// peers that answered.
func (t *Tracker) Round(now time.Duration, answered []string) {
	if !t.probing || now-t.lastRound > t.cfg.MaxGap() {
		// The first round, or the first after a break: whatever happened
		// in the break was not watched, so it is not counted as silence.
		t.probing = true
		t.runStart = now
	}
	t.lastRound = now
	for _, id := range answered {
		t.Contact(now, id)
	}
}

// Contact records contact with peer at now: one of this host's probes was
// answered, or the peer's probe arrived. A peer outside the set is ignored.
func (t *Tracker) Contact(now time.Duration, peer string) {
	p, ok := t.peers[peer]
	if !ok {
		return
	}
	if !p.contacted || now > p.contact {
		p.contact = now
		p.contacted = true
	}
}

// Watched is how long, up to now, this host has been probing without a
// break. Zero when it is not probing now.
func (t *Tracker) Watched(now time.Duration) time.Duration {
	if !t.probing || now-t.lastRound > t.cfg.MaxGap() {
		return 0
	}
	return now - t.runStart
}

// Reached is how many of the cluster's hosts this host has had contact with
// in the last Period, itself included.
func (t *Tracker) Reached(now time.Duration) int {
	n := 1
	for _, p := range t.peers {
		if p.contacted && now-p.contact <= t.cfg.Period {
			n++
		}
	}
	return n
}

// CutOff reports whether this host reaches fewer than half the cluster's
// hosts in the last Period. It is not yet a reason to stop anything:
// MustFence also needs the whole Period to have been watched.
func (t *Tracker) CutOff(now time.Duration) bool {
	return belowHalf(t.Reached(now), t.Hosts())
}

// MustFence reports whether this host must stop its own apps (R-421): the
// cluster has at least three hosts, this host has probed without a break for
// at least Period, and in all of that Period it had contact with fewer than
// half the cluster's hosts, itself included.
func (t *Tracker) MustFence(now time.Duration) bool {
	if t.Hosts() < MinHosts {
		return false
	}
	if t.Watched(now) < t.cfg.Period {
		return false
	}
	return t.CutOff(now)
}

// UnreachedFor is how long this host has watched peer without contact, up to
// now: never more than it has been probing without a break, and never more
// than the peer has been known. Zero for a peer outside the set.
func (t *Tracker) UnreachedFor(now time.Duration, peer string) time.Duration {
	p, ok := t.peers[peer]
	if !ok {
		return 0
	}
	since := p.added
	if p.contacted && p.contact > since {
		since = p.contact
	}
	return min(now-since, t.Watched(now))
}

// Unreached is UnreachedFor for every peer: what this host reports to the
// leader.
func (t *Tracker) Unreached(now time.Duration) map[string]time.Duration {
	out := make(map[string]time.Duration, len(t.peers))
	for id := range t.peers {
		out[id] = t.UnreachedFor(now, id)
	}
	return out
}
