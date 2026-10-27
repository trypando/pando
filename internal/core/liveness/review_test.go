package liveness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// errRefused is what a probe of a machine whose Pando process is down returns:
// the connection was refused, which is an answer from the machine.
var errRefused = fmt.Errorf("%w: connect: connection refused", ErrContactOnly)

// TestR421_AMachineThatRefusesTheProbeIsNotLost asserts R-421's "a host that
// is not cut off is never taken for lost": a host whose Pando process is
// down (crash loop, slow restart, waiting for Postgres) while its machine
// runs turns every probe away. That is contact, on every side: its peers do
// not report it unreached, it does not fence for refusals, and the leader
// never takes it for lost, even against reports that it was unreached.
func TestR421_AMachineThatRefusesTheProbeIsNotLost(t *testing.T) {
	// The leader: three of five report x unreached for an hour, but x's
	// machine refused the leader's own probe this round.
	r := newWatchRig("a", "b", "c", "d", "x")
	ctx := context.Background()
	silent := map[string]time.Duration{"x": time.Hour}
	r.say("a", silent)
	r.say("b", silent)
	r.say("c", silent)
	probe := r.watch.Probe
	r.watch.Probe = func(ctx context.Context, p Peer) ([]byte, error) {
		if p.ID == "x" {
			return nil, errRefused
		}
		return probe(ctx, p)
	}
	for range 30 {
		r.round(ctx)
	}
	require.Empty(t, r.store.lost, "x answered the leader's probe")
	require.Empty(t, r.failover.promoted)

	// x's machine goes silent: now the majority's word stands.
	r.watch.Probe = probe
	r.round(ctx)
	require.Equal(t, "liveness", r.store.lost["x"])

	// A host whose every peer refuses its probes reaches them all.
	m := newMonitorRig("a", "b", "c", "d")
	m.set(false, "a", "b", "c", "d")
	m.mon.Probe = func(context.Context, Peer) ([]byte, error) { return nil, errRefused }
	m.rounds(30)
	require.Zero(t, m.fencer.count())
	for _, d := range m.view().UnreachedMS {
		require.Less(t, d, tInterval.Milliseconds()+1)
	}
}

// TestR421_ACutOffHostWhosePeerListHangsStillFences asserts R-421 on a
// control host cut off from everything, its database included: reading its
// peers hangs, and it still probes the last ones known and fences after T.
func TestR421_ACutOffHostWhosePeerListHangsStillFences(t *testing.T) {
	r := newMonitorRig("a", "b", "c", "d")
	r.mon.Config.PeerWait = 5 * time.Millisecond
	r.rounds(2)

	hang := make(chan struct{})
	defer close(hang)
	peers := r.mon.Peers
	var calls sync.WaitGroup
	calls.Add(1)
	r.mon.Peers = func(ctx context.Context) ([]Peer, error) {
		calls.Done()
		<-hang // a database that never answers, and ignores its context
		return peers(ctx)
	}
	r.set(false, "a", "b", "c", "d")
	start := time.Now()
	r.rounds(7)
	calls.Wait()
	require.Equal(t, 1, r.fencer.count(), "fenced after T")
	require.True(t, r.mon.Fenced())
	require.Less(t, time.Since(start), 5*time.Second, "no round waited on the database")
}

// slowFencer stops containers until released.
type slowFencer struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (f *slowFencer) Fence(ctx context.Context) (int, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	select {
	case <-f.release:
		return 20, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// TestR421_ASlowFenceDoesNotStallProbing asserts R-421's timing: a fence that
// takes long runs beside the rounds, which go on probing and answering, and
// is not started twice at once.
func TestR421_ASlowFenceDoesNotStallProbing(t *testing.T) {
	r := newMonitorRig("a", "b", "c", "d")
	f := &slowFencer{release: make(chan struct{})}
	r.mon.Fencer = f
	r.set(false, "a", "b", "c", "d")
	for range 7 {
		r.mon.Round(context.Background())
		r.clk.Advance(tInterval)
	}
	done := make(chan struct{})
	go func() {
		for range 5 {
			r.mon.Round(context.Background())
			r.clk.Advance(tInterval)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rounds waited on the fence")
	}
	f.mu.Lock()
	require.Equal(t, 1, f.calls, "one fence at a time")
	f.mu.Unlock()
	require.False(t, r.mon.Fenced(), "not yet stopped")
	require.True(t, r.mon.Fencing(), "but fencing, so nothing starts meanwhile")
	close(f.release)
	r.mon.Settle()
	require.True(t, r.mon.Fenced())
	require.Equal(t, 20, r.view().Stopped)
}

// answer is a peer's View, saying how long it had not reached the sender.
func answer(host string, silentFor time.Duration) []byte {
	out, _ := json.Marshal(View{Host: host, SilentForMS: silentFor.Milliseconds()})
	return out
}

// TestR421_ASuspendedHostFencesWhenItWakesIfTheMajorityTookItForLost asserts
// R-421 for a host whose clock stood still: a suspended machine counts no
// silence of its own, so it learns from its peers' answers, at its first
// round awake, that more than half of them had not reached it for 3T, and
// stops its apps at once. Fewer than that, or a cluster under three hosts,
// stops nothing.
func TestR421_ASuspendedHostFencesWhenItWakesIfTheMajorityTookItForLost(t *testing.T) {
	r := newMonitorRig("a", "b", "c", "d")
	r.rounds(3)
	// Asleep for ten minutes: its clock did not move. On waking it reaches
	// everyone, and three of the five hosts say they had not reached it.
	r.answers["a"] = answer("a", 10*time.Minute)
	r.answers["b"] = answer("b", 10*time.Minute)
	r.answers["c"] = answer("c", 3*tPeriod)
	r.answers["d"] = answer("d", 0)
	r.rounds(1)
	require.Equal(t, 1, r.fencer.count())
	require.True(t, r.mon.Fenced())

	// Answered as usual from then on: the fence ends, nothing restarts.
	for _, p := range []string{"a", "b", "c", "d"} {
		r.answers[p] = answer(p, 0)
	}
	r.rounds(5)
	require.Equal(t, 1, r.fencer.count())
	require.False(t, r.mon.Fenced())

	// Two of five is not a majority.
	r2 := newMonitorRig("a", "b", "c", "d")
	r2.answers["a"] = answer("a", time.Hour)
	r2.answers["b"] = answer("b", time.Hour)
	r2.rounds(20)
	require.Zero(t, r2.fencer.count())

	// Two hosts never decide it.
	r3 := newMonitorRig("a")
	r3.answers["a"] = answer("a", time.Hour)
	r3.rounds(20)
	require.Zero(t, r3.fencer.count())
}

// TestR421_AnAnswerSaysHowLongTheSenderWentUnreached asserts the other half:
// a host answering a ping says how long it had not reached the sender before
// the ping arrived, not after.
func TestR421_AnAnswerSaysHowLongTheSenderWentUnreached(t *testing.T) {
	r := newMonitorRig("a", "b", "c", "d")
	r.set(false, "a")
	r.rounds(13) // 2T without contact with a
	var v View
	require.NoError(t, json.Unmarshal(r.mon.Answer("a"), &v))
	require.GreaterOrEqual(t, v.SilentForMS, (2 * tPeriod).Milliseconds())
	require.Zero(t, v.UnreachedMS["a"], "and a is reached now")
	var again View
	require.NoError(t, json.Unmarshal(r.mon.Answer("a"), &again))
	require.Zero(t, again.SilentForMS)
}

// TestR421_LostHostsLeaveTheClusterSize asserts R-421's count: a host marked
// lost no longer counts toward the cluster's size, on the leader's side and
// each host's alike. Five hosts with two lost are three: a host that still
// reaches one of the other two is not cut off, and does not fence, though it
// reaches two of the five. Three with one lost are two, and nothing else is
// taken for lost or fences by itself.
func TestR421_LostHostsLeaveTheClusterSize(t *testing.T) {
	list := []Peer{{ID: "self"}, {ID: "b"}, {ID: "c"}, {ID: "d", Lost: true}, {ID: "e", Lost: true}}
	m := newMonitorRig()
	m.mon.Peers = func(context.Context) ([]Peer, error) { return list, nil }
	m.reach["b"] = true
	m.rounds(30)
	require.Zero(t, m.fencer.count())
	require.ElementsMatch(t, []string{"b", "c"}, keys(m.view().UnreachedMS), "lost hosts are not watched")

	// Counted as five, the same reach would have fenced: what the lost
	// hosts did to the count before.
	for i := range list {
		list[i].Lost = false
	}
	m2 := newMonitorRig()
	m2.mon.Peers = func(context.Context) ([]Peer, error) { return list, nil }
	m2.reach["b"] = true
	m2.rounds(30)
	require.Positive(t, m2.fencer.count())

	// The leader: a, b and x, with d lost. a and b have not reached x for an
	// hour: two of the three counted hosts, more than half, so x is lost.
	// With d and a both lost, two hosts remain, and x is not.
	r := newWatchRig("a", "b", "d", "x")
	ctx := context.Background()
	r.store.lost["d"] = "liveness"
	silent := map[string]time.Duration{"x": time.Hour}
	r.say("a", silent)
	r.say("b", silent)
	r.round(ctx)
	require.Contains(t, r.store.lost, "x", "two of three counted hosts")

	r = newWatchRig("a", "b", "d", "x")
	r.store.lost["d"] = "liveness"
	r.store.lost["a"] = "liveness"
	r.say("b", silent)
	r.round(ctx)
	require.NotContains(t, r.store.lost, "x", "two counted hosts decide nothing")
}

func keys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// blockingFailover starts a standby only when released.
type blockingFailover struct {
	release chan struct{}
	started chan string
}

func (f blockingFailover) PromoteStandby(ctx context.Context, appID string) error {
	f.started <- appID
	select {
	case <-f.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestR421_AFailoverDoesNotStallTheLeader asserts R-421's leader goes on
// watching while a lost host's standbys start: rounds keep probing and
// auditing fences, and a host being failed over from is not welcomed back
// until its failover ends (R-419).
func TestR421_AFailoverDoesNotStallTheLeader(t *testing.T) {
	r := newWatchRig("a", "b", "c", "x")
	f := blockingFailover{release: make(chan struct{}), started: make(chan string, 8)}
	r.svc.Failover = f
	returns := &fakeReturns{}
	r.svc.Returns = returns
	ctx := context.Background()
	silent := map[string]time.Duration{"x": time.Hour}
	r.say("a", silent)
	r.say("b", silent)
	r.say("c", silent)
	r.watch.Round(ctx)
	require.Equal(t, "app_ok", <-f.started)
	require.Contains(t, r.store.lost, "x")

	// x answers again, the majority reaches it, and a fence is reported;
	// the rounds go on while the standby is still starting.
	r.say("a", nil)
	r.say("b", nil)
	r.mu.Lock()
	r.views["c"] = View{Host: "c", FenceID: "fnc_1"}
	r.mu.Unlock()
	r.say("x", nil)
	done := make(chan struct{})
	go func() {
		r.watch.Round(ctx)
		r.watch.Round(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader's round waited on a failover")
	}
	require.Contains(t, r.audit.actions(), "cluster.host.fence")
	require.Contains(t, r.store.lost, "x", "not welcomed back while its failover runs")

	close(f.release)
	r.svc.Settle()
	r.watch.Round(ctx)
	require.NotContains(t, r.store.lost, "x")
}

// witnessRig is a cluster as a person marking x lost finds it.
type witnessRig struct {
	w        *Witness
	list     []Peer
	probes   map[string]func() ([]byte, error)
	local    time.Duration
	watching bool
}

func newWitnessRig() *witnessRig {
	r := &witnessRig{
		list:     []Peer{{ID: "a"}, {ID: "b"}, {ID: "x"}},
		probes:   map[string]func() ([]byte, error){},
		local:    time.Hour,
		watching: true,
	}
	silentView := func(host string) func() ([]byte, error) {
		return func() ([]byte, error) {
			return json.Marshal(View{Host: host, UnreachedMS: map[string]int64{"x": time.Hour.Milliseconds()}})
		}
	}
	r.probes["a"] = silentView("a")
	r.probes["b"] = silentView("b")
	r.probes["x"] = func() ([]byte, error) { return nil, errors.New("i/o timeout") }
	r.w = &Witness{
		Hosts: func(context.Context) ([]Peer, error) { return r.list, nil },
		Probe: func(_ context.Context, p Peer) ([]byte, error) { return r.probes[p.ID]() },
		Local: func(string) (time.Duration, bool) { return r.local, r.watching },
	}
	return r
}

func (r *witnessRig) silent() error {
	return r.w.Silent(context.Background(), state.Host{ID: "x", Name: "x"})
}

// TestR421_MarkingAHostLostByHandNeedsSustainedSilence asserts R-421 and
// R-153 on the manual path: a person may mark a host lost only when this
// control host has watched it without contact for T, its machine does not
// answer now, not even by refusing the connection, and no other host had
// contact with it within T.
func TestR421_MarkingAHostLostByHandNeedsSustainedSilence(t *testing.T) {
	r := newWitnessRig()
	require.NoError(t, r.silent())

	r.local = tPeriod - time.Second
	err := r.silent()
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "This control host had contact with it 29s ago")
	require.Contains(t, errs.As(err).Remedy, "turn the machine off or disconnect it from the network")
	require.NotContains(t, errs.As(err).Remedy, "agent")

	r = newWitnessRig()
	r.probes["x"] = func() ([]byte, error) { return nil, errRefused }
	require.Contains(t, errs.As(r.silent()).Message, "Its machine answered Pando's check just now")

	r = newWitnessRig()
	r.probes["b"] = func() ([]byte, error) {
		return json.Marshal(View{Host: "b", UnreachedMS: map[string]int64{"x": 5000}})
	}
	require.Contains(t, errs.As(r.silent()).Message, "Host b had contact with it 5s ago")

	r = newWitnessRig()
	r.watching = false
	require.Contains(t, errs.As(r.silent()).Message, "is not one this control host watches")
	r.w.Local = nil
	require.Contains(t, errs.As(r.silent()).Message, "not watching the cluster's hosts yet")

	// A host already lost is no longer watched here, and marking it again
	// (to retry its failover) asks the rest.
	r.list[2].Lost = true
	require.NoError(t, r.silent())

	r.list = r.list[:2]
	require.Equal(t, errs.NotFound, errs.CodeOf(r.silent()))
	r.w.Hosts = func(context.Context) ([]Peer, error) { return nil, errors.New("database away") }
	require.Equal(t, errs.Internal, errs.CodeOf(r.silent()))
}

func TestMonitorUnreachedFor(t *testing.T) {
	r := newMonitorRig("a", "b")
	r.set(false, "a")
	r.rounds(7)
	d, ok := r.mon.UnreachedFor("a")
	require.True(t, ok)
	require.GreaterOrEqual(t, d, tPeriod)
	_, ok = r.mon.UnreachedFor("self")
	require.False(t, ok)
}
