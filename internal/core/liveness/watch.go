package liveness

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/clock"
)

// Watch is the leader's half of R-421, run as a leader job: every Interval it
// probes every host's agent, keeps the View each one answers with in a
// Ledger, audits the fences they report, and takes for lost a host more than
// half the cluster has not reached for three Periods.
//
// It is the leader's because only one replica may start a standby. It reads
// the host list from the database, and keeps the last one when it cannot; it
// cannot mark a host lost without the database, so with Postgres away nothing
// fails over.
type Watch struct {
	// Hosts lists every host of the cluster, lost ones included: a lost
	// host still counts toward the cluster's size until it is removed.
	Hosts func(ctx context.Context) ([]Peer, error)

	// Probe asks a host's agent for its View, as JSON.
	Probe func(ctx context.Context, p Peer) ([]byte, error)

	Service *Service
	Config  Config
	Clock   clock.Clock
	Logger  *zap.Logger

	once   sync.Once
	now    func() time.Duration
	ledger *Ledger
	hosts  []Peer
}

func (w *Watch) init() {
	w.once.Do(func() {
		w.Config = w.Config.Normalize()
		if w.Clock == nil {
			w.Clock = clock.System{}
		}
		if w.Logger == nil {
			w.Logger = zap.NewNop()
		}
		w.now = Elapsed(w.Clock)
		w.ledger = NewLedger(w.Config)
	})
}

// Run watches until ctx ends. A new leader starts with an empty Ledger, so it
// takes nobody for lost before three Periods of its own.
func (w *Watch) Run(ctx context.Context) {
	w.init()
	for {
		w.Round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.Clock.After(w.Config.Interval):
		}
	}
}

// Round probes every host once and acts on what the Ledger then says.
func (w *Watch) Round(ctx context.Context) {
	w.init()
	if fresh, err := w.Hosts(ctx); err != nil {
		w.Logger.Warn("could not read the cluster's hosts; watching the last ones known", zap.Error(err))
	} else {
		w.forgetGone(fresh)
		w.hosts = fresh
	}
	if len(w.hosts) == 0 {
		return
	}

	views := w.collect(ctx)
	now := w.now()
	ids := make([]string, len(w.hosts))
	for i, h := range w.hosts {
		ids[i] = h.ID
		if v, ok := views[h.ID]; ok {
			w.ledger.Record(now, h.ID, v.Unreached())
			w.Service.RecordFence(ctx, h.ID, v)
		}
	}
	if len(ids) < MinHosts {
		return
	}

	lost, err := w.Service.Store.LostHosts(ctx)
	if err != nil {
		w.Logger.Warn("could not read which hosts are lost; deciding nothing this round", zap.Error(err))
		return
	}
	for _, x := range ids {
		if !lost[x] && w.ledger.Lost(now, ids, x) {
			w.Service.declare(ctx, x)
		}
	}
}

func (w *Watch) forgetGone(fresh []Peer) {
	keep := make(map[string]bool, len(fresh))
	for _, h := range fresh {
		keep[h.ID] = true
	}
	for _, h := range w.hosts {
		if !keep[h.ID] {
			w.ledger.Forget(h.ID)
		}
	}
}

// collect probes every host at once and returns the Views that came back
// whole. A View naming another host than the one probed is dropped.
func (w *Watch) collect(ctx context.Context) map[string]View {
	var mu sync.Mutex
	out := map[string]View{}
	var wg sync.WaitGroup
	for _, h := range w.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, w.Config.Timeout)
			defer cancel()
			raw, err := w.Probe(pctx, h)
			if err != nil {
				return
			}
			var v View
			if json.Unmarshal(raw, &v) != nil || v.Host != h.ID {
				return
			}
			mu.Lock()
			out[h.ID] = v
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}
