package deploy

import (
	"context"
	"runtime"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/work"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// DefaultConcurrency is how many deploys one replica runs at once when
// configuration does not say [P]: one per CPU, and at least two. A deploy is a
// clone, a build and a wait for health; the build is the expensive part and
// BuildKit spreads one build across cores, so more than one per core mostly
// makes every build slower.
func DefaultConcurrency() int {
	if n := runtime.NumCPU(); n > 2 {
		return n
	}
	return 2
}

// Revisions reads the revision a queued deployment is for.
type Revisions interface {
	RevisionByID(ctx context.Context, specID string) (state.Revision, bool, error)
}

// Queue runs queued deployments on this replica (issue #72, O-32).
//
// A deployment is created pending and unclaimed (state.Deployments.Create).
// Every replica's queue claims what it has room for, so a deploy runs on
// whichever replica is free rather than the one whose request created it, no
// replica runs more than Limit at once, and a deploy queued when every
// replica was busy — or when the one that took the request restarted — runs
// when one has room.
//
// Deploys of one app are still never concurrent: an app with a deploy in
// flight, queued included, refuses the next one (design 05 §5).
type Queue struct {
	Runner      *Runner
	Deployments *state.Deployments
	Revisions   Revisions

	// Limit is how many deploys this replica runs at once. DefaultConcurrency
	// when zero. Concurrency, when set, is read instead.
	Limit int

	// Concurrency reads the limit from host policy's max_concurrent_deploys
	// each time the queue looks for work (issue #93): zero for the default, or
	// an error. On an error the last limit read stays in force rather than
	// the queue stopping, or running without one.
	Concurrency func(ctx context.Context) (int, error)

	// Poll is how often to look for deploys another replica queued. Two
	// seconds when zero.
	Poll time.Duration

	Logger *zap.Logger

	pool work.Pool[state.Deployment]
}

// Start is the approval service's Starter: the deployment is already queued,
// so this only tells this replica's queue to look now rather than at its next
// poll. ctx and rev are not kept — a queued deploy outlives the request that
// made it, and reads its revision when it runs.
func (q *Queue) Start(context.Context, state.Deployment, state.Revision) { q.pool.Kick() }

// Kick tells the queue there may be a deploy waiting.
func (q *Queue) Kick() { q.pool.Kick() }

// Running is how many deploys this replica is running now.
func (q *Queue) Running() int { return q.pool.Running() }

// Serve runs queued deploys until ctx ends. Deploys still running then are
// stopped and put back in the queue for another replica.
func (q *Queue) Serve(ctx context.Context) {
	logger := q.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultConcurrency()
	}
	q.pool.Name = "deploys"
	q.pool.Limit = limit
	if q.Concurrency != nil {
		q.pool.LimitFunc = policyLimit(q.Concurrency, limit, logger)
	}
	q.pool.Poll = q.Poll
	q.pool.Logger = logger
	q.pool.Claim = q.Deployments.Claim
	q.pool.Run = func(ctx context.Context, dep state.Deployment) { q.run(log.Into(ctx, logger), dep) }
	q.pool.Release = func(ctx context.Context, deps []state.Deployment) {
		ids := make([]string, len(deps))
		for i, d := range deps {
			ids[i] = d.ID
		}
		if n, err := q.Deployments.Release(ctx, ids); err != nil {
			logger.Warn("could not return stopped deploys to the queue", zap.Error(err))
		} else if n > 0 {
			logger.Info("returned stopped deploys to the queue", zap.Int64("count", n))
		}
	}
	logger.Info("running queued deploys", zap.Int("concurrency", limit))
	q.pool.Serve(ctx)
}

func (q *Queue) run(ctx context.Context, dep state.Deployment) {
	l := log.From(ctx).With(zap.String("deployment_id", dep.ID), zap.String("app_id", dep.AppID))
	ctx = log.Into(ctx, l)

	rev, found, err := q.Revisions.RevisionByID(ctx, dep.SpecID)
	if err == nil && !found {
		err = errs.New(errs.NotFound, "The spec revision this deploy was for no longer exists.").
			WithRemedy("Deploy the app again.")
	}
	if err != nil {
		if ctx.Err() != nil {
			return // stopping; the deploy goes back in the queue
		}
		e := errs.As(err)
		message := "Pando could not read the spec revision this deploy was for."
		if e != nil && e.Code != errs.Internal {
			message = e.Message
		}
		_ = q.Deployments.Finish(ctx, dep.ID, state.DeployFailed, string(errs.CodeOf(err)), message)
		l.Warn("a queued deploy could not start", zap.Error(err))
		return
	}

	// The reason is not repeated here. Run has already put it where it
	// belongs — the deployment's record and the log its user is watching —
	// and an error from a deploy can carry what it was doing with the app's
	// secrets, which a server log line must never risk (R-194).
	if err := q.Runner.Run(ctx, dep, rev); err != nil {
		if ctx.Err() != nil {
			l.Info("deploy stopped by this replica's shutdown; it goes back in the queue")
			return
		}
		l.Warn("deployment ended in failure; the reason is on the deployment")
	}
}

// policyLimit is the pool's limit read from host policy each time it is asked
// (issue #93): zero for the default, and on a failed read the last limit read,
// rather than the queue stopping or running without one. Starts from first.
func policyLimit(read func(context.Context) (int, error), first int, logger *zap.Logger) func(context.Context) int {
	last := first
	return func(ctx context.Context) int {
		n, err := read(ctx)
		switch {
		case err != nil:
			logger.Warn("could not read the deploy limit from host policy; keeping the last one",
				zap.Int("concurrency", last), zap.Error(err))
			return last
		case n <= 0:
			n = DefaultConcurrency()
		}
		if n != last {
			logger.Info("deploy limit changed", zap.Int("from", last), zap.Int("to", n))
			last = n
		}
		return n
	}
}
