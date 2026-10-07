package detection

import (
	"context"
	"runtime"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/work"
	"github.com/trypando/pando/internal/log"
)

// Timeout bounds one detection run.
//
// Generous: it covers a clone, an auction and a trial run that may pull a base
// image over a slow connection. The cost of being too short is a detection that
// fails for a large repository on a home connection, which is exactly the user
// R-005 describes.
const Timeout = 10 * time.Minute

// DefaultConcurrency is how many detections one replica runs at once when
// configuration does not say [P]: one per CPU, at least two. A detection is a
// clone and, sometimes, a trial run — lighter than a deploy, and just as
// unbounded when every app added at once starts one.
func DefaultConcurrency() int {
	if n := runtime.NumCPU(); n > 2 {
		return n
	}
	return 2
}

// Queue runs queued detections on this replica (issue #72, O-32).
//
// Enqueue marks an app's detection running and unclaimed; every replica's
// queue claims what it has room for. Creating a hundred apps at once used to
// start a hundred clones in the process that took the requests.
type Queue struct {
	Detections *state.Detections

	// Detect runs one claimed detection to completion. In production,
	// Runner.RunQueued.
	Detect func(ctx context.Context, appID string) (state.Detection, error)

	// Limit is how many detections this replica runs at once.
	// DefaultConcurrency when zero.
	Limit int

	// Poll is how often to look for detections another replica queued. Two
	// seconds when zero.
	Poll time.Duration

	// Timeout overrides the package Timeout. Zero is the default.
	Timeout time.Duration

	Logger *zap.Logger

	pool work.Pool[string]
}

// Enqueue queues a detection for an app — marked running, so the first poll
// cannot read the previous outcome as this one's — and tells this replica's
// queue to look now.
func (q *Queue) Enqueue(ctx context.Context, appID string) error {
	if err := q.Detections.Start(ctx, appID); err != nil {
		return err
	}
	q.pool.Kick()
	return nil
}

// Running is how many detections this replica is running now.
func (q *Queue) Running() int { return q.pool.Running() }

// Serve runs queued detections until ctx ends.
func (q *Queue) Serve(ctx context.Context) {
	logger := q.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultConcurrency()
	}
	q.pool.Name = "detections"
	q.pool.Limit = limit
	q.pool.Poll = q.Poll
	q.pool.Logger = logger
	q.pool.Claim = q.Detections.Claim
	q.pool.Run = func(ctx context.Context, appID string) { q.run(log.Into(ctx, logger), appID) }
	q.pool.Release = func(ctx context.Context, appIDs []string) {
		if _, err := q.Detections.Release(ctx, appIDs); err != nil {
			logger.Warn("could not return stopped detections to the queue", zap.Error(err))
		}
	}
	q.pool.Serve(ctx)
}

func (q *Queue) run(parent context.Context, appID string) {
	timeout := q.Timeout
	if timeout <= 0 {
		timeout = Timeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	_, err := q.Detect(ctx, appID)
	if err == nil || parent.Err() != nil {
		// Done, or stopped by this replica's shutdown: a stopped detection
		// goes back in the queue rather than being recorded as failed.
		return
	}
	// Already recorded against the app as a failed detection, which is where
	// a user will look for it — unless the detector failed before it got as
	// far as recording anything (a source the allowlist no longer permits),
	// which would otherwise leave it running for good. Logged as well, because
	// a detection that fails for every app is an install problem rather than
	// an app problem, and nobody finds that by reading one app's page.
	log.From(parent).Warn("detection failed", zap.String("app_id", appID), zap.Error(err))
	_ = q.Detections.FailIfRunning(context.WithoutCancel(ctx), appID, err)
}
