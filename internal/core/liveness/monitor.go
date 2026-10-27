package liveness

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/id"
)

// Peer is another host of the cluster, as a probe reaches it.
type Peer struct {
	ID      string
	Name    string
	Address string

	// Lost is that the host is marked lost (R-421). A lost host is left out
	// of the cluster's size on every side — the leader's and each host's
	// alike, so they agree on it — and is not probed by the hosts; the
	// leader still probes it, to see it come back (R-419).
	Lost bool
}

// View is what a host answers a probe with: its own view, so that the leader
// learns every host's view from the probes it makes anyway.
type View struct {
	Host string `json:"host"`

	// UnreachedMS is, for each peer, how many milliseconds this host has
	// watched it without contact (Tracker.UnreachedFor). A duration on this
	// host's own clock, never a time.
	UnreachedMS map[string]int64 `json:"unreached_ms"`

	// SilentForMS is how long this host had watched the host that sent the
	// probe without contact, until the probe arrived: what the majority
	// would say of the sender. A host that was suspended learns from it, at
	// its first round after it wakes, that it was taken for lost while its
	// own clock stood still (Monitor.Round). Zero for the leader's collector.
	SilentForMS int64 `json:"silent_for_ms,omitempty"`

	// Fenced is whether this host is cut off and has stopped its apps.
	Fenced bool `json:"fenced"`

	// FenceID names this host's latest fence, so the leader audits each one
	// once (cluster.host.fence) however often it hears of it. Kept after the
	// host is reached again, until it fences next.
	FenceID string `json:"fence_id,omitempty"`

	// Stopped is how many app containers the latest fence stopped.
	Stopped int `json:"stopped,omitempty"`
}

// Unreached is UnreachedMS as durations.
func (v View) Unreached() map[string]time.Duration {
	out := make(map[string]time.Duration, len(v.UnreachedMS))
	for k, ms := range v.UnreachedMS {
		out[k] = time.Duration(ms) * time.Millisecond
	}
	return out
}

// Fencer stops this host's app containers: hostagent.DockerFencer. It
// returns how many it stopped. It is never asked to start one: a host that
// has fenced never restarts its apps by itself (R-421).
type Fencer interface {
	Fence(ctx context.Context) (int, error)
}

// Monitor is a host's half of R-421, run by the host's agent: it probes every
// other host, answers their probes with its own view, and stops this host's
// apps when it is the one cut off.
//
// It needs nothing from the control plane but the list of peers, and keeps
// the last list it was given when the control plane cannot be asked: Postgres
// or every control host being down never stops an app. What stops one is
// this host failing to reach most of the cluster's hosts, the control hosts
// counted as hosts like any other, by their agents. The list is read beside
// the round, and the fence runs beside it too: neither a database that hangs
// nor a runtime slow to stop containers keeps the host from probing.
type Monitor struct {
	Self string

	// Peers lists the cluster's hosts. An error keeps the last list.
	Peers func(ctx context.Context) ([]Peer, error)

	// Probe pings a peer and returns the View it answered with, as JSON. An
	// error wrapping ErrContactOnly is contact without a view; any other
	// error is silence.
	Probe func(ctx context.Context, p Peer) ([]byte, error)

	Fencer Fencer
	Config Config
	Clock  clock.Clock
	Logger *zap.Logger

	once    sync.Once
	mu      sync.Mutex
	now     func() time.Duration
	tracker *Tracker
	peers   []Peer

	// fetch is the peer list being read, when one is.
	fetch chan peerList

	// fencing is a fence decided and not yet ended by reaching the cluster
	// again; fenced is that this host's apps were stopped in it; stopping
	// is that a Fencer call is under way.
	fencing  bool
	fenced   bool
	told     bool
	fenceID  string
	stopped  int
	stopping bool
	fences   sync.WaitGroup
}

type peerList struct {
	list []Peer
	err  error
}

func (m *Monitor) init() {
	m.once.Do(func() {
		m.Config = m.Config.Normalize()
		if m.Clock == nil {
			m.Clock = clock.System{}
		}
		if m.Logger == nil {
			m.Logger = zap.NewNop()
		}
		m.now = Elapsed(m.Clock)
		m.tracker = NewTracker(m.Config)
	})
}

// Run probes every Interval until ctx ends, then waits for a fence under way.
func (m *Monitor) Run(ctx context.Context) {
	m.init()
	defer m.Settle()
	for {
		m.Round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.Clock.After(m.Config.Interval):
		}
	}
}

// Round probes every peer once, records who answered, and fences if this
// host must: when it has reached fewer than half the cluster for Period, or
// when more than half the cluster answers that it had not reached this host
// for as long as the leader takes a host for lost.
func (m *Monitor) Round(ctx context.Context) {
	m.init()
	peers := m.refreshPeers(ctx)
	answered, views := probeAll(ctx, peers, m.Config.Timeout, m.Probe)

	m.mu.Lock()
	now := m.now()
	m.tracker.Round(now, answered)
	first := m.decide(now, m.toldLost(views))
	start := m.fencing && !m.stopping
	fenceID := m.fenceID
	if start {
		m.stopping = true
	}
	m.mu.Unlock()

	if start {
		// Every round while cut off, not only the first: stopping is
		// idempotent, a stop that failed is tried again, and anything
		// started on this host meanwhile is stopped too. Beside the round,
		// so probing goes on while containers stop.
		m.fences.Add(1)
		go m.fence(ctx, fenceID, first)
	}
}

// decide starts or ends a fence, and reports whether one started. Called
// with m.mu held.
func (m *Monitor) decide(now time.Duration, told bool) bool {
	switch {
	case !m.fencing && (told || m.tracker.MustFence(now)):
		m.fencing, m.told = true, told
		m.fenced, m.stopped = false, 0
		m.fenceID = id.New(id.Fence)
		if told {
			m.Logger.Warn("most of the cluster's hosts answer that they had not reached this host for long enough to take it for lost; stopping its apps",
				zap.String("fence_id", m.fenceID))
		}
		return true
	case m.fencing && !m.tracker.CutOff(now) && (m.fenced || !m.told):
		// Reached again. The apps stay stopped: whatever starts them now
		// is the control plane's reconciler, never this host (R-421). A
		// host told it was taken for lost keeps trying until they are
		// stopped, since their standbys may be running.
		m.fencing, m.fenced = false, false
		m.Logger.Info("this host reaches the cluster again; its apps stay stopped until Pando starts them",
			zap.String("fence_id", m.fenceID))
	}
	return false
}

// toldLost reports whether more than half the cluster's hosts answered this
// round that they had watched this host without contact for LostAfter: what
// the leader takes a host for lost on (R-421). A host learns so when its own
// clock could not tell it — it was suspended, or stalled, and its window
// started again when it woke — and stops its apps at once, since their
// standbys may be starting. With fewer than three hosts it is never told.
func (m *Monitor) toldLost(views map[string]View) bool {
	n := m.tracker.Hosts()
	if n < MinHosts {
		return false
	}
	need := m.Config.LostAfter().Milliseconds()
	count := 0
	for _, v := range views {
		if v.SilentForMS >= need {
			count++
		}
	}
	return moreThanHalf(count, n)
}

// refreshPeers is the peer list for this round: a fresh one when it comes
// back within PeerWait, else the last one known. At most one read is under
// way at a time, and one that comes back late is taken the next round.
func (m *Monitor) refreshPeers(ctx context.Context) []Peer {
	m.mu.Lock()
	ch := m.fetch
	if ch == nil {
		ch = make(chan peerList, 1)
		m.fetch = ch
		go func() {
			fctx, cancel := context.WithTimeout(ctx, m.Config.Period)
			defer cancel()
			list, err := m.Peers(fctx)
			ch <- peerList{list: list, err: err}
		}()
	}
	m.mu.Unlock()

	wait := time.NewTimer(m.Config.PeerWait)
	defer wait.Stop()
	select {
	case got := <-ch:
		m.mu.Lock()
		m.fetch = nil
		m.setPeers(got)
		m.mu.Unlock()
	case <-wait.C:
		m.Logger.Warn("the cluster's hosts are taking long to read; probing the last ones known")
	case <-ctx.Done():
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peers
}

// setPeers takes a list read, leaving out this host and every host marked
// lost. Called with m.mu held.
func (m *Monitor) setPeers(got peerList) {
	if got.err != nil {
		m.Logger.Warn("could not read the cluster's hosts; probing the last ones known", zap.Error(got.err))
		return
	}
	m.peers = active(got.list, m.Self)
	ids := make([]string, len(m.peers))
	for i, p := range m.peers {
		ids[i] = p.ID
	}
	m.tracker.SetPeers(m.now(), ids)
}

// fenceBound is the longest one fence may take, Period at most: well under
// the two Periods between a host stopping its apps and its standbys starting
// (R-421).
const fenceBound = 20 * time.Second

func (m *Monitor) fence(ctx context.Context, fenceID string, first bool) {
	defer m.fences.Done()
	// A fence under way finishes even as the process stops.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), min(fenceBound, m.Config.Period))
	defer cancel()
	stopped, err := m.Fencer.Fence(fctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopping = false
	if fenceID != m.fenceID {
		return
	}
	m.stopped += stopped
	if err != nil {
		m.Logger.Error("this host is cut off from most of the cluster and could not stop all its apps; trying again next round",
			zap.String("fence_id", m.fenceID), zap.Int("stopped", stopped), zap.Error(err))
		return
	}
	if !m.fencing {
		// Ended while it ran: the host was reached again.
		return
	}
	if first || !m.fenced {
		m.Logger.Warn("this host stopped its apps: it reached fewer than half the cluster's hosts for the fencing period, or most of them took it for lost",
			zap.Int("stopped", stopped), zap.String("fence_id", m.fenceID))
	}
	m.fenced = true
}

// Settle waits for a fence under way to end.
func (m *Monitor) Settle() { m.fences.Wait() }

// Answer records contact from the peer that sent a probe, and returns this
// host's View as JSON. from is "" for a probe from the leader's collector,
// which is not a host's monitor.
func (m *Monitor) Answer(from string) []byte {
	m.init()
	m.mu.Lock()
	now := m.now()
	v := View{Host: m.Self, UnreachedMS: map[string]int64{}, Fenced: m.fenced, FenceID: m.fenceID, Stopped: m.stopped}
	if from != "" {
		// What this host would have said of the sender, before it heard
		// from it now.
		v.SilentForMS = m.tracker.UnreachedFor(now, from).Milliseconds()
		m.tracker.Contact(now, from)
	}
	for k, d := range m.tracker.Unreached(now) {
		v.UnreachedMS[k] = d.Milliseconds()
	}
	m.mu.Unlock()
	out, _ := json.Marshal(v)
	return out
}

// Fenced reports whether this host has fenced and is still cut off.
func (m *Monitor) Fenced() bool {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fenced
}

// UnreachedFor is how long this host has watched peer without contact, and
// whether it watches peer at all: what a person marking peer lost is checked
// against (Witness).
func (m *Monitor) UnreachedFor(peer string) (time.Duration, bool) {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.tracker.Has(peer) {
		return 0, false
	}
	return m.tracker.UnreachedFor(m.now(), peer), true
}

// active is peers without self and without the hosts marked lost.
func active(peers []Peer, self string) []Peer {
	out := make([]Peer, 0, len(peers))
	for _, p := range peers {
		if p.ID != self && !p.Lost {
			out = append(out, p)
		}
	}
	return out
}

// probeAll probes every peer at once, each within timeout, and returns the IDs
// of those it had contact with, and the Views of those that answered with
// one. A View naming another host than the one probed is contact, not a view.
func probeAll(ctx context.Context, peers []Peer, timeout time.Duration, probe func(context.Context, Peer) ([]byte, error)) ([]string, map[string]View) {
	type result struct {
		contact bool
		view    *View
	}
	results := make([]result, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			raw, err := probe(pctx, p)
			results[i].contact = contact(err)
			var v View
			if err == nil && json.Unmarshal(raw, &v) == nil && v.Host == p.ID {
				results[i].view = &v
			}
		}()
	}
	wg.Wait()
	var answered []string
	views := map[string]View{}
	for i, p := range peers {
		if results[i].contact {
			answered = append(answered, p.ID)
		}
		if results[i].view != nil {
			views[p.ID] = *results[i].view
		}
	}
	return answered, views
}
