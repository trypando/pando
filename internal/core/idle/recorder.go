package idle

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// FlushEvery is how often a replica writes the activity it has seen (R-394).
const FlushEvery = time.Minute

// ActivityWriter is where activity is written. *state.Activity is one.
type ActivityWriter interface {
	RecordActivity(ctx context.Context, seen map[string]time.Time) error
}

// Recorder keeps which apps the proxy let a request through to, and writes
// them in one statement a minute.
//
// In memory, because the request path must never wait on the database for
// this (R-394). A replica that stops loses at most a minute of it, which moves
// an app's idle clock by a minute against a setting counted in days.
type Recorder struct {
	Store  ActivityWriter
	Logger *zap.Logger

	mu   sync.Mutex
	seen map[string]time.Time
}

// Touch records that a request reached appID now. One map write under a lock
// the flush holds only to swap the map out.
func (r *Recorder) Touch(appID string) {
	now := time.Now().UTC()
	r.mu.Lock()
	if r.seen == nil {
		r.seen = map[string]time.Time{}
	}
	r.seen[appID] = now
	r.mu.Unlock()
}

// Run flushes every FlushEvery until ctx is canceled, and once more on the way
// out so a clean stop loses nothing.
func (r *Recorder) Run(ctx context.Context) {
	ticker := time.NewTicker(FlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// The caller's context is done; the last write gets its own.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			r.Flush(flushCtx)
			cancel()
			return
		case <-ticker.C:
			r.Flush(ctx)
		}
	}
}

// Flush writes what has been seen since the last flush. On failure the batch
// is put back, merged with anything newer, for the next one.
func (r *Recorder) Flush(ctx context.Context) {
	r.mu.Lock()
	batch := r.seen
	r.seen = nil
	r.mu.Unlock()
	if len(batch) == 0 {
		return
	}

	if err := r.Store.RecordActivity(ctx, batch); err != nil {
		if r.Logger != nil {
			r.Logger.Warn("could not record app activity; keeping it for the next write", zap.Error(err))
		}
		r.mu.Lock()
		if r.seen == nil {
			r.seen = map[string]time.Time{}
		}
		for id, at := range batch {
			if newer, ok := r.seen[id]; !ok || newer.Before(at) {
				r.seen[id] = at
			}
		}
		r.mu.Unlock()
	}
}
