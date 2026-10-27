package liveness

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/clock"
)

type fakeFencer struct {
	mu    sync.Mutex
	calls int
	fail  bool
	each  int
}

func (f *fakeFencer) Fence(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return 0, errors.New("docker is away")
	}
	return f.each, nil
}

func (f *fakeFencer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type monitorRig struct {
	clk     *clock.Fake
	mon     *Monitor
	fencer  *fakeFencer
	mu      sync.Mutex
	reach   map[string]bool
	peerErr error
}

func newMonitorRig(peers ...string) *monitorRig {
	r := &monitorRig{clk: clock.NewFake(time.Time{}), fencer: &fakeFencer{each: 2}, reach: map[string]bool{}}
	list := []Peer{{ID: "self"}}
	for _, p := range peers {
		list = append(list, Peer{ID: p, Address: p + ":7443"})
		r.reach[p] = true
	}
	r.mon = &Monitor{
		Self: "self",
		Peers: func(context.Context) ([]Peer, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.peerErr != nil {
				return nil, r.peerErr
			}
			return list, nil
		},
		Probe: func(_ context.Context, p Peer) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.reach[p.ID] {
				return nil
			}
			return errors.New("no route to host")
		},
		Fencer: r.fencer,
		Clock:  r.clk,
	}
	return r
}

func (r *monitorRig) set(reach bool, peers ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range peers {
		r.reach[p] = reach
	}
}

// rounds runs n rounds, one Interval apart.
func (r *monitorRig) rounds(n int) {
	for range n {
		r.mon.Round(context.Background())
		r.clk.Advance(tInterval)
	}
}

func (r *monitorRig) view() View {
	var v View
	if err := json.Unmarshal(r.mon.Answer(""), &v); err != nil {
		panic(err)
	}
	return v
}

// TestR421_ACutOffHostStopsItsAppsAndNeverRestartsThem asserts R-421 on the
// host's side: cut off for T it stops its apps, keeps them stopped while cut
// off, and when it reaches the cluster again starts nothing.
func TestR421_ACutOffHostStopsItsAppsAndNeverRestartsThem(t *testing.T) {
	r := newMonitorRig("a", "b", "c", "d")
	r.rounds(3)
	require.Zero(t, r.fencer.count())

	r.set(false, "a", "b", "c", "d")
	r.rounds(6) // T
	require.Zero(t, r.fencer.count(), "not before T")
	require.False(t, r.mon.Fenced())

	r.rounds(1)
	require.Equal(t, 1, r.fencer.count())
	require.True(t, r.mon.Fenced())
	v := r.view()
	require.True(t, v.Fenced)
	require.Regexp(t, `^fnc_`, v.FenceID)
	require.Equal(t, 2, v.Stopped)
	require.Equal(t, "self", v.Host)

	r.rounds(2)
	require.Equal(t, 3, r.fencer.count(), "stopped again each round while cut off")
	require.Equal(t, v.FenceID, r.view().FenceID, "one fence")

	r.set(true, "a", "b")
	r.rounds(3)
	require.False(t, r.mon.Fenced())
	require.Equal(t, 3, r.fencer.count(), "reaching the cluster again starts and stops nothing")
	require.Equal(t, v.FenceID, r.view().FenceID, "the fence is still reported, for the leader to audit")

	// A second cut-off is a second fence.
	r.set(false, "a", "b")
	r.rounds(8)
	require.NotEqual(t, v.FenceID, r.view().FenceID)
}

// TestR421_ControlPlaneOrPostgresDownStopsNothing asserts R-421: the host list
// cannot be read (Postgres or every control host is down), but the hosts
// still reach each other, and nothing stops.
func TestR421_ControlPlaneOrPostgresDownStopsNothing(t *testing.T) {
	r := newMonitorRig("a", "b", "c", "d")
	r.rounds(2)
	r.mu.Lock()
	r.peerErr = errors.New("connection refused")
	r.mu.Unlock()
	// The control hosts' Pando processes are down; c and d are control
	// hosts whose agents still answer, and a and b app hosts.
	r.rounds(100)
	require.Zero(t, r.fencer.count())

	// Never told the hosts at all: a cluster of one, which never fences.
	r2 := newMonitorRig("a", "b", "c", "d")
	r2.peerErr = errors.New("connection refused")
	r2.set(false, "a", "b", "c", "d")
	r2.rounds(100)
	require.Zero(t, r2.fencer.count())
}

func TestMonitorCountsProbesItReceives(t *testing.T) {
	r := newMonitorRig("a", "b", "c", "d")
	r.set(false, "a", "b", "c", "d")
	for range 20 {
		// Its own probes fail, but two peers' arrive: a one-way link,
		// which still counts as contact both ways.
		r.mon.Answer("a")
		r.mon.Answer("b")
		r.rounds(1)
	}
	require.Zero(t, r.fencer.count())
}

func TestMonitorRetriesAFenceThatFailed(t *testing.T) {
	r := newMonitorRig("a", "b", "c", "d")
	r.fencer.fail = true
	r.set(false, "a", "b", "c", "d")
	r.rounds(8)
	require.Equal(t, 2, r.fencer.count())
	require.False(t, r.mon.Fenced())
	id := r.view().FenceID
	require.NotEmpty(t, id)

	r.fencer.mu.Lock()
	r.fencer.fail = false
	r.fencer.mu.Unlock()
	r.rounds(1)
	require.True(t, r.mon.Fenced())
	require.Equal(t, id, r.view().FenceID, "the same fence, finished")
}

func TestMonitorRunStopsWithItsContext(t *testing.T) {
	r := newMonitorRig("a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.mon.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func TestMonitorDefaults(t *testing.T) {
	m := &Monitor{Self: "s"}
	m.init()
	require.NotNil(t, m.Logger)
	require.Equal(t, clock.System{}, m.Clock)
	require.False(t, m.Fenced())
}
