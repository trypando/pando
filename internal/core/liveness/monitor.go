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
}

// View is what a host answers a probe with: its own view, so that the leader
// learns every host's view from the probes it makes anyway.
type View struct {
	Host string `json:"host"`

	// UnreachedMS is, for each peer, how many milliseconds this host has
	// watched it without contact (Tracker.UnreachedFor). A duration on this
	// host's own clock, never a time.
	UnreachedMS map[string]int64 `json:"unreached_ms"`

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
// counted as hosts like any other, by their agents.
type Monitor struct {
	Self string

	// Peers lists the cluster's other hosts. An error keeps the last list.
	Peers func(ctx context.Context) ([]Peer, error)

	// Probe probes a peer; nil error is contact.
	Probe func(ctx context.Context, p Peer) error

	Fencer Fencer
	Config Config
	Clock  clock.Clock
	Logger *zap.Logger

	once    sync.Once
	mu      sync.Mutex
	now     func() time.Duration
	tracker *Tracker
	peers   []Peer

	// fencing is a fence decided and not yet ended by reaching the cluster
	// again; fenced is that this host's apps were stopped in it.
	fencing bool
	fenced  bool
	fenceID string
	stopped int
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

// Run probes every Interval until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	m.init()
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
// host must.
func (m *Monitor) Round(ctx context.Context) {
	m.init()
	peers := m.refreshPeers(ctx)
	answered := probeAll(ctx, peers, m.Config.Timeout, m.Probe)

	m.mu.Lock()
	now := m.now()
	m.tracker.Round(now, answered)
	first := false
	switch {
	case !m.fencing && m.tracker.MustFence(now):
		m.fencing, first = true, true
		m.fenced, m.stopped = false, 0
		m.fenceID = id.New(id.Fence)
	case m.fencing && !m.tracker.CutOff(now):
		// Reached again. The apps stay stopped: whatever starts them now
		// is the control plane's reconciler, never this host (R-421).
		m.fencing, m.fenced = false, false
		m.Logger.Info("this host reaches the cluster again; its apps stay stopped until Pando starts them",
			zap.String("fence_id", m.fenceID))
	}
	fencing := m.fencing
	m.mu.Unlock()

	if fencing {
		// Every round while cut off, not only the first: stopping is
		// idempotent, a stop that failed is tried again, and anything
		// started on this host meanwhile is stopped too.
		m.fence(ctx, first)
	}
}

func (m *Monitor) refreshPeers(ctx context.Context) []Peer {
	fresh, err := m.Peers(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.Logger.Warn("could not read the cluster's hosts; probing the last ones known", zap.Error(err))
		return m.peers
	}
	m.peers = without(fresh, m.Self)
	ids := make([]string, len(m.peers))
	for i, p := range m.peers {
		ids[i] = p.ID
	}
	m.tracker.SetPeers(m.now(), ids)
	return m.peers
}

func (m *Monitor) fence(ctx context.Context, first bool) {
	stopped, err := m.Fencer.Fence(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.Logger.Error("this host is cut off from most of the cluster and could not stop its apps; trying again next round",
			zap.String("fence_id", m.fenceID), zap.Error(err))
		return
	}
	if first || !m.fenced {
		m.Logger.Warn("this host has reached fewer than half the cluster's hosts for the fencing period; stopped its apps",
			zap.Int("stopped", stopped), zap.String("fence_id", m.fenceID))
	}
	m.stopped += stopped
	m.fenced = true
}

// Answer records contact from the peer that sent a probe, and returns this
// host's View as JSON. from is "" for a probe from the leader's collector,
// which is not a host's monitor.
func (m *Monitor) Answer(from string) []byte {
	m.init()
	m.mu.Lock()
	now := m.now()
	if from != "" {
		m.tracker.Contact(now, from)
	}
	v := View{Host: m.Self, UnreachedMS: map[string]int64{}, Fenced: m.fenced, FenceID: m.fenceID, Stopped: m.stopped}
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

func without(peers []Peer, self string) []Peer {
	out := make([]Peer, 0, len(peers))
	for _, p := range peers {
		if p.ID != self {
			out = append(out, p)
		}
	}
	return out
}

// probeAll probes every peer at once, each within timeout, and returns the IDs
// of those that answered.
func probeAll(ctx context.Context, peers []Peer, timeout time.Duration, probe func(context.Context, Peer) error) []string {
	ok := make([]bool, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			ok[i] = probe(pctx, p) == nil
		}()
	}
	wg.Wait()
	var out []string
	for i, p := range peers {
		if ok[i] {
			out = append(out, p.ID)
		}
	}
	return out
}
