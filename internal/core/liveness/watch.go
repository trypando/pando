package liveness

import (
	"context"
	"slices"
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
	// host is still probed, to see it come back, but no longer counts
	// toward the cluster's size.
	Hosts func(ctx context.Context) ([]Peer, error)

	// Probe asks a host's agent for its View, as JSON; as Monitor.Probe.
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

// Round probes every host once and acts on what the Ledger then says. A host
// marked lost counts no more toward the cluster's size (R-421), as on every
// host's side (Monitor); with fewer than three hosts left, nobody else is
// taken for lost. A host whose machine answered the leader's own probe this
// round is never taken for lost, whatever the others report: its apps may be
// running.
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

	contacted, views := probeAll(ctx, w.hosts, w.Config.Timeout, w.Probe)
	now := w.now()
	w.record(ctx, now, views)
	lost, err := w.Service.Store.LostHosts(ctx)
	if err != nil {
		w.Logger.Warn("could not read which hosts are lost; deciding nothing this round", zap.Error(err))
		return
	}
	counted := make([]string, 0, len(w.hosts))
	for _, h := range w.hosts {
		if !lost[h.ID] {
			counted = append(counted, h.ID)
		}
	}
	w.welcomeBack(ctx, now, counted, lost, views)
	// A failover not started for want of its audit event is tried again
	// (R-421, R-228).
	w.Service.refail(ctx, lost)
	if len(counted) < MinHosts {
		return
	}
	for _, x := range counted {
		if !slices.Contains(contacted, x) && w.ledger.Lost(now, counted, x) {
			w.Service.declare(ctx, x)
		}
	}
}

// record keeps each View in the Ledger and audits the fence it reports.
func (w *Watch) record(ctx context.Context, now time.Duration, views map[string]View) {
	for _, h := range w.hosts {
		if v, ok := views[h.ID]; ok {
			w.ledger.Record(now, h.ID, v.Unreached())
			w.Service.RecordFence(ctx, h.ID, v)
		}
	}
}

// welcomeBack hands each host marked lost that answered this round, and that
// the majority no longer takes for lost, to Service.back (R-419). At any
// cluster size: a host a person marked lost in a cluster of two comes back
// the same way. The majority rule is asked too, so a host the leader alone
// still reaches, while most of the cluster does not, stays lost. counted is
// the hosts not marked lost, which the returning host is weighed among.
func (w *Watch) welcomeBack(ctx context.Context, now time.Duration, counted []string, lost map[string]bool, views map[string]View) {
	for x := range lost {
		if _, answered := views[x]; !answered || w.ledger.Lost(now, append(slices.Clone(counted), x), x) {
			continue
		}
		if err := w.Service.back(ctx, x); err != nil {
			w.Logger.Warn("a lost host is back, and its copies of the apps failed over from it could not all be removed; trying again next round",
				zap.String("host_id", x), zap.Error(err))
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
