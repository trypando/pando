package reconciler

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/work"
	"github.com/trypando/pando/internal/errs"
)

// Rolling per-app backups (R-210, R-211) and expiry.
//
// R-211's "daily, 7 retained" had a schema column, a retention default and a
// query that found expired rows — and nothing that ever took a backup or
// deleted one. The requirement was true in the database and false in practice.
//
// A leader job of its own rather than in the reconciler's loop because nothing
// about this is urgent and all of it is destructive, and rather than in the
// GC's pass because a backup is slow and that pass has other work to do.

// BackupRunner takes and prunes per-app backups.
//
// An interface rather than *backup.Service so the collector is testable without
// a destination, and so this package cannot reach anything else the service
// exposes — restore in particular, which the janitor has no business calling.
type BackupRunner interface {
	CreateForApp(ctx context.Context, id string, req backup.AppCreateRequest) (backup.Created, error)
	Discard(ctx context.Context, adapterRef, objectName string) error
}

// RollingBackups takes rolling backups for apps that are due one, and removes
// backups whose retention has passed (R-211). A leader job of its own (issue
// #72), not part of the GC's hourly pass: that walked every app with storage
// in series, so one slow destination held up every other app's backup and the
// rest of the GC with it.
//
// Due-driven: each pass asks the database for a page of apps due a backup and
// takes them a few at a time, so how long a pass takes depends on how many are
// due, not on how many apps there are.
type RollingBackups struct {
	Apps         *state.Apps
	Backups      *state.Backups
	Backup       BackupRunner
	BundleSource *state.BundleSource
	Auditor      Auditor
	Logger       *zap.Logger
	Clock        clock.Clock

	// Every is how often to look for apps due a backup. Five minutes when
	// zero [P]: a backup is due once a day, so this is how late one can be.
	Every time.Duration

	// Concurrency is how many backups run at once. Two when zero [P]: a
	// backup copies a volume, and the disk it reads is the one the apps use.
	Concurrency int

	// RetryAfter is how long after an attempt that was skipped or failed the
	// app is tried again. An hour when zero, which is what the attempt's
	// remedy tells a person ("within the hour").
	RetryAfter time.Duration

	// Batch is the most apps one pass takes. 200 when zero.
	Batch int
}

// backupInterval is how often an app is due a rolling backup.
//
// Daily, measured from the last rolling backup rather than from a fixed clock
// time: an install that is off overnight should take a backup when it comes
// back, not skip the day. R-211's "daily" is an interval, not an appointment.
const backupInterval = 24 * time.Hour

// Run takes backups until ctx ends.
func (b *RollingBackups) Run(ctx context.Context) {
	every := b.Every
	if every <= 0 {
		every = 5 * time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		b.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Pass takes every due backup it is given a page of, then expires old ones.
func (b *RollingBackups) Pass(ctx context.Context) {
	if b.Backups == nil || b.Backup == nil || b.BundleSource == nil {
		return
	}
	b.takeDue(ctx)
	b.pruneExpired(ctx)
}

func (b *RollingBackups) takeDue(ctx context.Context) {
	retry := b.RetryAfter
	if retry <= 0 {
		retry = time.Hour
	}
	batch := b.Batch
	if batch <= 0 {
		batch = 200
	}
	concurrency := b.Concurrency
	if concurrency <= 0 {
		concurrency = 2
	}
	now := b.now()
	due, err := b.Apps.DueForBackup(ctx, now.Add(-backupInterval), now.Add(-retry), batch)
	if err != nil {
		b.Logger.Warn("could not list apps due a rolling backup", zap.Error(err))
		return
	}
	work.Each(ctx, concurrency, due, func(ctx context.Context, app state.AppWithStorage) {
		// Only apps that have data to lose. An app with no volumes has nothing
		// a rolling backup would hold that its spec revisions do not, and
		// taking one anyway would fill the destination with empty bundles.
		if app.Retain <= 0 {
			return
		}
		attempt, cause := b.takeOne(ctx, app)
		b.recordAttempt(ctx, attempt, cause)
	})
}

// takeOne takes one app's rolling backup and says what came of it.
//
// Every way through returns an attempt, because every way through used to be
// a way for a backup not to happen without anybody finding out (issue #87):
// an app whose storage Pando had no record of returned nil, and the caller
// logged "took a rolling backup" for a backup that was never written.
//
// The error, when there is one, is the cause for the log. The attempt carries
// the part a person can act on.
func (g *RollingBackups) takeOne(ctx context.Context, app state.AppWithStorage) (state.BackupAttempt, error) {
	attempt := state.BackupAttempt{AppID: app.AppID, AttemptedAt: g.now()}

	volumes, err := g.BundleSource.VolumesForApp(ctx, app.AppID)
	if err != nil {
		return failed(attempt, err), err
	}
	if len(volumes) == 0 {
		// The spec declares storage and Pando has no record of any. The
		// reconciler records what the runtime holds as it checks the app, so
		// this is usually one pass behind a first deploy — and when it is not,
		// the storage was never created, which a redeploy does.
		attempt.Outcome = state.AttemptSkipped
		attempt.Message = "This app's configuration keeps data in a volume, but Pando has no record of " +
			"that volume being created, so there was nothing to copy. The next backup is attempted within the hour."
		attempt.Remedy = "If this is still the case after an hour, redeploy the app so its storage is created and recorded."
		return attempt, nil
	}

	id := g.Backups.NewID()
	created, err := g.Backup.CreateForApp(ctx, id, backup.AppCreateRequest{
		AppID:   app.AppID,
		Kind:    "rolling",
		Spec:    app.Spec,
		Volumes: volumes,

		// Retained for as many days as the app asks to keep copies. R-211's
		// count is a number of dailies, so the window is that many days — which
		// is what makes "7 retained" mean seven rather than seven-ish.
		RetainFor: time.Duration(app.Retain) * backupInterval,
	})
	if err != nil {
		return failed(attempt, err), err
	}

	if err := g.Backups.Record(ctx, state.Backup{
		ID: id, AppID: app.AppID, Kind: "rolling",
		AdapterRef: created.AdapterRef, ObjectName: created.ObjectName,
		SizeBytes: created.SizeBytes, Manifest: created.Manifest,
		RetainUntil: created.RetainUntil, CreatedBy: "system",
	}); err != nil {
		return failed(attempt, err), err
	}

	attempt.Outcome = state.AttemptTaken
	attempt.BackupID = id
	return attempt, nil
}

// failed is an attempt that went wrong, in the words of the error that did it.
//
// A destination that is down will be up on the next pass, and a backup that
// fails is not a reason to stop backing up every other app — so this is
// recorded and the sweep moves on, rather than retried harder.
func failed(attempt state.BackupAttempt, err error) state.BackupAttempt {
	attempt.Outcome = state.AttemptFailed

	// An envelope's message is written for a person and its wrapped cause is
	// not in it. An error without one names internals, which belong in the
	// log rather than on a screen.
	if e := errs.As(err); e != nil {
		attempt.Message = e.Message
		attempt.Remedy = e.Remedy
	}
	if attempt.Message == "" {
		attempt.Message = "Pando could not take this app's backup."
	}
	if attempt.Remedy == "" {
		attempt.Remedy = "Pando tries again within the hour. If it fails again, search the server log for this app's ID for the cause."
	}
	return attempt
}

// recordAttempt logs an attempt and keeps it where the console can show it.
func (g *RollingBackups) recordAttempt(ctx context.Context, attempt state.BackupAttempt, cause error) {
	fields := []zap.Field{zap.String("app_id", attempt.AppID)}
	switch attempt.Outcome {
	case state.AttemptTaken:
		g.Logger.Info("took a rolling backup", append(fields, zap.String("backup_id", attempt.BackupID))...)
	case state.AttemptSkipped:
		g.Logger.Warn("skipped a rolling backup", append(fields, zap.String("reason", attempt.Message))...)
	default:
		g.Logger.Warn("could not take a rolling backup", append(fields, zap.Error(cause))...)
	}

	if err := g.Backups.RecordAttempt(ctx, attempt); err != nil {
		g.Logger.Warn("could not record the backup attempt", append(fields, zap.Error(err))...)
	}
}

// pruneExpired removes backups whose retention has passed (R-211).
//
// The object first, then the row. The other order leaves an object nothing
// references, which is invisible and accumulates — and an on_delete backup is
// never returned here, because R-204 keeps those until somebody discards them.
func (g *RollingBackups) pruneExpired(ctx context.Context) {
	expired, err := g.Backups.Expired(ctx, g.now())
	if err != nil {
		g.Logger.Warn("could not list expired backups", zap.Error(err))
		return
	}

	for _, b := range expired {
		if err := g.Backup.Discard(ctx, b.AdapterRef, b.ObjectName); err != nil {
			g.Logger.Warn("could not remove an expired backup",
				zap.String("backup_id", b.ID), zap.Error(err))
			continue
		}
		if err := g.Backups.Forget(ctx, b.ID); err != nil {
			g.Logger.Warn("removed an expired backup but could not record it",
				zap.String("backup_id", b.ID), zap.Error(err))
			continue
		}

		g.Logger.Info("removed an expired backup", zap.String("backup_id", b.ID))
		if g.Auditor != nil {
			_ = g.Auditor.Write(ctx, AuditEvent{
				Action: "backup.expire", AppID: b.AppID,
				Detail: map[string]any{"backup_id": b.ID, "kind": b.Kind},
			})
		}
	}
}

func (g *GC) now() time.Time {
	if g.Clock == nil {
		return time.Now().UTC()
	}
	return g.Clock.Now()
}

func (g *RollingBackups) now() time.Time {
	if g.Clock == nil {
		return time.Now().UTC()
	}
	return g.Clock.Now()
}
