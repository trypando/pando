package liveness

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/clock"
)

// TestR421_HostsJoiningAtOnceNeverMakeAHealthyHostFence asserts R-421: a host
// that reaches every host it has known does not stop its apps because several
// hosts joined at once and have not answered yet. Three hosts, then four join
// together: for a while the three reach three of seven, fewer than half.
func TestR421_HostsJoiningAtOnceNeverMakeAHealthyHostFence(t *testing.T) {
	clk := clock.NewFake(time.Time{})
	fencer := &fakeFencer{}
	var mu sync.Mutex
	list := []Peer{{ID: "self"}, {ID: "a"}, {ID: "b"}}
	reach := map[string]bool{"a": true, "b": true}
	m := &Monitor{
		Self: "self",
		Peers: func(context.Context) ([]Peer, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]Peer(nil), list...), nil
		},
		Probe: func(_ context.Context, p Peer) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			if reach[p.ID] {
				return nil, nil
			}
			return nil, errors.New("no route to host")
		},
		Fencer: fencer,
		Clock:  clk,
	}
	rounds := func(n int) {
		for range n {
			m.Round(context.Background())
			m.Settle()
			clk.Advance(tInterval)
		}
	}
	rounds(12) // 2T of a healthy cluster of three.

	joiners := []string{"c", "d", "e", "f"}
	mu.Lock()
	for _, id := range joiners {
		list = append(list, Peer{ID: id})
	}
	mu.Unlock()
	// The joiners are still starting their agents for a whole Period.
	rounds(6)
	require.Zero(t, fencer.count(), "joining hosts must never make a healthy host fence")
	require.False(t, m.Fenced())

	// They come up: everyone reaches everyone, and nothing ever stopped.
	mu.Lock()
	for _, id := range joiners {
		reach[id] = true
	}
	mu.Unlock()
	rounds(12)
	require.Zero(t, fencer.count())

	// The leader's side over the same window: each side reports the other
	// unreached for as long as it has watched it, which is not yet three
	// Periods, so nobody is taken for lost.
	r := newWatchRig("self", "a", "b", "c", "d", "e", "f")
	for _, joiner := range joiners {
		r.say(joiner, map[string]time.Duration{"self": tPeriod, "a": tPeriod, "b": tPeriod})
	}
	for _, old := range []string{"self", "a", "b"} {
		r.say(old, map[string]time.Duration{"c": tPeriod, "d": tPeriod, "e": tPeriod, "f": tPeriod})
	}
	r.round(context.Background())
	require.Empty(t, r.failover.promoted)
}

// TestTrackerANewPeerCountsAgainstReachOnlyAfterT is the Tracker's half of
// the test above: a peer never reached counts as reached for its first
// Period, and as unreached after it, while a fence already decided ends only
// on contact.
func TestTrackerANewPeerCountsAgainstReachOnlyAfterT(t *testing.T) {
	tr := NewTracker(Config{})
	tr.SetPeers(0, []string{"a", "b"})
	for now := time.Duration(0); now <= 2*tPeriod; now += tInterval {
		tr.Round(now, []string{"a", "b"})
	}
	joined := 2 * tPeriod
	tr.SetPeers(joined, []string{"a", "b", "c", "d", "e", "f"})
	for now := joined; now < joined+tPeriod; now += tInterval {
		tr.Round(now, []string{"a", "b"})
		require.False(t, tr.MustFence(now), "at %s", now-joined)
	}
	require.True(t, tr.CutOff(joined+tPeriod), "a fence ends only on contact")
	after := joined + tPeriod
	tr.Round(after, []string{"a", "b"})
	require.True(t, tr.MustFence(after), "after T a joiner never reached counts as unreached")
}
