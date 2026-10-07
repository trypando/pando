// Package retention removes what Pando keeps only for a while: old deploys,
// scans, sessions, notifications, idempotency keys, sign-in flows, the event
// outbox, and what deleted apps left behind (issue #72, R-224).
//
// One leader job, hourly, deleting a batch at a time. Each table's window is
// a setting with a documented default; what each delete never removes —
// revisions (R-152), the audit log (R-027), rollback targets (R-157) — is in
// the state store's queries rather than here, so no setting can reach it.
package retention

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/clock"
)

// Settings is how long each kind of row is kept. A zero field is its default.
type Settings struct {
	// DeploymentsPerApp is how many of an app's newest deploys are kept
	// [P]: 50, what an app's deploy list shows. Older ones go, except one in
	// flight or waiting for approval and the newest successful deploy of
	// each revision the app ever pinned.
	DeploymentsPerApp int

	// Scans is how long a scan is kept [P]: 90 days. The newest scan of
	// each revision is kept however old.
	Scans time.Duration

	// Sessions is how long an expired or revoked session is kept [P]: 30
	// days.
	Sessions time.Duration

	// IdempotencyKeys is how long a key's answer is replayed [P]: a day
	// (migration 000011).
	IdempotencyKeys time.Duration

	// SSOFlows is how long a sign-in flow is kept after it expired [P]: a
	// day, so a person arriving late at a failed sign-in is told why.
	SSOFlows time.Duration

	// Events is how long the outbox keeps an event with nothing left to
	// deliver, and its deliveries and their attempts [P]: 30 days
	// (subscription.Retention).
	Events time.Duration

	// DeletedApps is how long a deleted app's detection and backup-attempt
	// record are kept [P]: 30 days.
	DeletedApps time.Duration

	// Batch is how many rows one delete removes. 1,000 when zero.
	Batch int
}

// Defaults fills in every zero field.
func (s Settings) Defaults() Settings {
	day := 24 * time.Hour
	if s.DeploymentsPerApp <= 0 {
		s.DeploymentsPerApp = 50
	}
	if s.Scans <= 0 {
		s.Scans = 90 * day
	}
	if s.Sessions <= 0 {
		s.Sessions = 30 * day
	}
	if s.IdempotencyKeys <= 0 {
		s.IdempotencyKeys = day
	}
	if s.SSOFlows <= 0 {
		s.SSOFlows = day
	}
	if s.Events <= 0 {
		s.Events = 30 * day
	}
	if s.DeletedApps <= 0 {
		s.DeletedApps = 30 * day
	}
	if s.Batch <= 0 {
		s.Batch = 1000
	}
	return s
}

// Store is the slice of state.Retention and state.Events the job deletes with.
type Store interface {
	Deployments(ctx context.Context, keepPerApp, limit int) (int64, error)
	DeletedApps(ctx context.Context, before time.Time, limit int) (int64, error)
	Scans(ctx context.Context, before time.Time, limit int) (int64, error)
	Sessions(ctx context.Context, before time.Time, limit int) (int64, error)
	Notifications(ctx context.Context, now time.Time, limit int) (int64, error)
	IdempotencyKeys(ctx context.Context, before time.Time, limit int) (int64, error)
	SSOFlows(ctx context.Context, before, now time.Time, limit int) (int64, error)
}

// Outbox is the event outbox's prune.
type Outbox interface {
	Prune(ctx context.Context, before time.Time, limit int) (int64, error)
}

// Job removes old rows, hourly. A leader job: once per install.
type Job struct {
	Store    Store
	Outbox   Outbox
	Settings Settings
	Clock    clock.Clock
	Logger   *zap.Logger

	// Every is how often it runs. An hour when zero.
	Every time.Duration
}

// Run removes old rows until ctx ends, starting at once.
func (j *Job) Run(ctx context.Context) {
	every := j.Every
	if every <= 0 {
		every = time.Hour
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		j.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Pass is one run over every table, and returns how many rows went from each.
func (j *Job) Pass(ctx context.Context) map[string]int64 {
	s := j.Settings.Defaults()
	now := time.Now().UTC()
	if j.Clock != nil {
		now = j.Clock.Now()
	}
	logger := j.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	steps := []struct {
		name string
		run  func(ctx context.Context, limit int) (int64, error)
	}{
		{"deployments", func(ctx context.Context, n int) (int64, error) {
			return j.Store.Deployments(ctx, s.DeploymentsPerApp, n)
		}},
		{"scans", func(ctx context.Context, n int) (int64, error) { return j.Store.Scans(ctx, now.Add(-s.Scans), n) }},
		{"sessions", func(ctx context.Context, n int) (int64, error) {
			return j.Store.Sessions(ctx, now.Add(-s.Sessions), n)
		}},
		{"notifications", func(ctx context.Context, n int) (int64, error) { return j.Store.Notifications(ctx, now, n) }},
		{"idempotency_keys", func(ctx context.Context, n int) (int64, error) {
			return j.Store.IdempotencyKeys(ctx, now.Add(-s.IdempotencyKeys), n)
		}},
		{"sso_flows", func(ctx context.Context, n int) (int64, error) {
			return j.Store.SSOFlows(ctx, now.Add(-s.SSOFlows), now, n)
		}},
		{"deleted_apps", func(ctx context.Context, n int) (int64, error) {
			return j.Store.DeletedApps(ctx, now.Add(-s.DeletedApps), n)
		}},
	}
	if j.Outbox != nil {
		steps = append(steps, struct {
			name string
			run  func(ctx context.Context, limit int) (int64, error)
		}{"events", func(ctx context.Context, n int) (int64, error) { return j.Outbox.Prune(ctx, now.Add(-s.Events), n) }})
	}

	out := map[string]int64{}
	for _, step := range steps {
		total, err := drain(ctx, s.Batch, step.run)
		out[step.name] = total
		if err != nil && ctx.Err() == nil {
			logger.Warn("could not remove old rows", zap.String("table", step.name), zap.Error(err))
		}
		if total > 0 {
			logger.Info("removed old rows", zap.String("table", step.name), zap.Int64("count", total))
		}
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

// drain repeats one batched delete until a batch comes back short, so a
// backlog goes in one pass, a batch at a time.
func drain(ctx context.Context, batch int, run func(ctx context.Context, limit int) (int64, error)) (int64, error) {
	var total int64
	for ctx.Err() == nil {
		n, err := run(ctx, batch)
		total += n
		if err != nil {
			return total, err
		}
		if n < int64(batch) {
			return total, nil
		}
	}
	return total, ctx.Err()
}
