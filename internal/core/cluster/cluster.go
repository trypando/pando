// Package cluster is what lets several Pando processes serve one install
// against one database (issue #72).
//
// Each process is a replica. A replica registers itself, heartbeats, and is
// taken to have stopped when it goes quiet; work it had under way is then
// recorded as interrupted by whichever replica leads. One replica at a time
// leads, and only the leader runs the jobs that must happen once per install —
// garbage collection, auto-deploy, audit retention — rather than once per
// process.
//
// This is not scheduling (R-010). A replica is Pando's own process, never a
// place an app runs, and nothing here decides where a workload goes. The apps
// run where the runtime adapter puts them, exactly as with one replica.
package cluster

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/state"
)

// Store is the slice of state.Replicas a member uses.
type Store interface {
	Register(ctx context.Context, rep state.Replica) error
	Heartbeat(ctx context.Context, replicaID string) (bool, error)
	Stop(ctx context.Context, replicaID string) error
	RestartPending(ctx context.Context, replicaID string) (bool, error)
	VerificationKeys(ctx context.Context, validity time.Duration) ([]state.Replica, error)
}

// Member is this process's membership of the install.
type Member struct {
	Store        Store
	ID           string
	Hostname     string
	AdvertiseURL string
	Version      string
	Minter       *assertion.Minter
	Logger       *zap.Logger

	// Interval is how often to heartbeat; state.HeartbeatInterval if zero.
	Interval time.Duration
}

// Join registers this replica and publishes its signing key. Before the
// server listens: an assertion this replica signs must be verifiable from the
// first request it serves.
func (m *Member) Join(ctx context.Context) error {
	key := m.Minter.SigningKey()
	return m.Store.Register(ctx, state.Replica{
		ID:           m.ID,
		Hostname:     m.Hostname,
		AdvertiseURL: m.AdvertiseURL,
		Version:      m.Version,
		AssertionKID: key.ID,
		AssertionKey: key.Public,
	})
}

// PeerKeys reads every replica's published key, for the JWKS.
func (m *Member) PeerKeys(ctx context.Context) ([]assertion.PublicKey, error) {
	reps, err := m.Store.VerificationKeys(ctx, assertion.Lifetime)
	if err != nil {
		return nil, err
	}
	out := make([]assertion.PublicKey, 0, len(reps))
	for _, r := range reps {
		out = append(out, assertion.PublicKey{ID: r.AssertionKID, Public: r.AssertionKey})
	}
	return out, nil
}

// Run heartbeats until ctx ends, and returns early — with the reason — when
// this replica should restart: a restart was asked for (POST /restart reaches
// every replica, not only the one that received it), or the install has taken
// this replica for dead and abandoned its work, after which carrying on under
// the same identity would be a lie.
func (m *Member) Run(ctx context.Context) (restart string) {
	every := m.Interval
	if every <= 0 {
		every = state.HeartbeatInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	failingSince := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-ticker.C:
		}

		alive, err := m.Store.Heartbeat(ctx, m.ID)
		switch {
		case err != nil:
			// The database is away. Nothing this replica does can be
			// recorded either, so it keeps trying; once it has been away
			// longer than the others wait, they have abandoned its work and
			// it must come back as someone new.
			if failingSince.IsZero() {
				failingSince = time.Now()
			}
			m.Logger.Warn("could not heartbeat", zap.Error(err))
			if time.Since(failingSince) > state.ReplicaStale {
				return "this replica could not reach the state database for longer than the others wait for it"
			}
			continue
		case !alive:
			return "the install took this replica for stopped and recorded its work as interrupted"
		}
		failingSince = time.Time{}

		pending, err := m.Store.RestartPending(ctx, m.ID)
		if err == nil && pending {
			return "a restart was asked for"
		}
	}
}

// Leave records this replica as stopped, at shutdown.
func (m *Member) Leave(ctx context.Context) {
	if err := m.Store.Stop(ctx, m.ID); err != nil {
		m.Logger.Warn("could not record this replica as stopped", zap.Error(err))
	}
}

// Leadership is the slice of state.Replicas a leader uses.
type Leadership interface {
	TryLead(ctx context.Context, check time.Duration) (*state.Leadership, bool, error)
}

// Job is something the leader runs until its context ends.
type Job struct {
	Name string
	Run  func(ctx context.Context)
}

// Leader runs Jobs on whichever one replica holds the leader lock.
//
// A lock for the whole set rather than one per job: the jobs are a handful of
// slow loops, and one leader is one place to look when asking "who is running
// the GC". A replica that loses the lock cancels every job and waits for them
// to stop before it tries to lead again, so two never run a job at once.
type Leader struct {
	Store  Leadership
	Jobs   []Job
	Logger *zap.Logger

	// Retry is how often a follower tries to lead, and how often a leader
	// checks it still is. Ten seconds when zero.
	Retry time.Duration

	mu      sync.Mutex
	leading bool
}

// Leading reports whether this replica leads now.
func (l *Leader) Leading() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leading
}

// Run tries to lead until ctx ends.
func (l *Leader) Run(ctx context.Context) {
	retry := l.Retry
	if retry <= 0 {
		retry = 10 * time.Second
	}
	for {
		lead, ok, err := l.Store.TryLead(ctx, retry)
		if err != nil {
			l.Logger.Warn("could not try to lead", zap.Error(err))
		}
		if ok {
			l.lead(ctx, lead)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

func (l *Leader) lead(ctx context.Context, lead *state.Leadership) {
	l.Logger.Info("this replica leads; running the install-wide jobs")
	l.set(true)
	defer l.set(false)

	jobCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, j := range l.Jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			j.Run(jobCtx)
		}(j)
	}

	select {
	case <-ctx.Done():
	case <-lead.Lost():
		l.Logger.Warn("this replica lost the leader lock; stopping the install-wide jobs")
	}
	cancel()
	wg.Wait()
	lead.Resign(ctx)
}

func (l *Leader) set(v bool) {
	l.mu.Lock()
	l.leading = v
	l.mu.Unlock()
}

// LogOwner finds the base URL of the live replica running a deployment, or ""
// when it is this replica, or no live replica is: the answer to "where is this
// deploy's live log" (issue #72).
type LogOwner struct {
	Self        string
	Deployments interface {
		Runner(ctx context.Context, deploymentID string) (string, error)
		Waiting(ctx context.Context, deploymentID string) (bool, error)
	}
	Replicas interface {
		ByID(ctx context.Context, replicaID string) (state.Replica, bool, error)
	}

	// ClaimWait is how long to wait for a queued deploy to be claimed before
	// answering (O-32): until then nobody holds its log, and answering "this
	// replica" would follow a log another replica is about to write. A
	// minute when zero.
	ClaimWait time.Duration
}

// Where answers for one deployment.
func (o LogOwner) Where(ctx context.Context, deploymentID string) (string, error) {
	wait := o.ClaimWait
	if wait <= 0 {
		wait = time.Minute
	}
	deadline := time.Now().Add(wait)
	for {
		waiting, err := o.Deployments.Waiting(ctx, deploymentID)
		if err != nil || !waiting || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(500 * time.Millisecond):
		}
	}

	runner, err := o.Deployments.Runner(ctx, deploymentID)
	if err != nil || runner == "" || runner == o.Self {
		return "", err
	}
	rep, found, err := o.Replicas.ByID(ctx, runner)
	if err != nil || !found || rep.StoppedAt != nil {
		return "", err
	}
	return rep.AdvertiseURL, nil
}

// Sweeper records work left by stopped replicas as interrupted, and forgets
// replicas long gone. A leader job.
type Sweeper struct {
	Abandon []func(ctx context.Context) (int64, error)
	Prune   func(ctx context.Context) error
	Logger  *zap.Logger
	Every   time.Duration
}

// Run sweeps until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	every := s.Every
	if every <= 0 {
		every = 15 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		s.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep is one pass.
func (s *Sweeper) Sweep(ctx context.Context) {
	for _, abandon := range s.Abandon {
		if n, err := abandon(ctx); err != nil {
			s.Logger.Warn("could not record work interrupted by a stopped replica", zap.Error(err))
		} else if n > 0 {
			s.Logger.Info("recorded work interrupted by a stopped replica", zap.Int64("count", n))
		}
	}
	if s.Prune != nil {
		if err := s.Prune(ctx); err != nil {
			s.Logger.Warn("could not prune stopped replicas", zap.Error(err))
		}
	}
}
