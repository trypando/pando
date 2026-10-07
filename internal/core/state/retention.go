package state

import (
	"context"
	"time"

	"github.com/trypando/pando/internal/errs"
)

// Retention removes rows from the tables that otherwise only grow (issue #72,
// R-224): one batch per call, so the retention job's deletes are short
// transactions it repeats rather than one long one holding locks a request is
// waiting on.
//
// What each never removes is the point of each, and is written into the
// query rather than left to the caller:
//
//   - Spec revisions are never deleted here (R-152). Their pruning is the
//     GC's, under its own rules.
//   - The audit log is never touched (R-027); its retention is R-347's.
//   - A deploy in flight or waiting for approval is never deleted, nor the
//     newest successful deploy of a revision the app ever pinned: that row is
//     what makes rolling back to the revision free of approval (R-157) and
//     what the reconciler restores its image from.
//   - The newest scan of each revision is never deleted, so a rollback still
//     restores its score (R-319).
type Retention struct{ db *DB }

func NewRetention(db *DB) *Retention { return &Retention{db: db} }

func (r *Retention) exec(ctx context.Context, what, query string, args ...any) (int64, error) {
	tag, err := r.db.Exec(ctx, query, args...)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not remove old "+what+".", err)
	}
	return tag.RowsAffected(), nil
}

// Deployments removes up to limit deployments beyond each app's newest
// keepPerApp, never one in flight, waiting for approval, or the newest
// successful deploy of a revision the app ever pinned.
func (r *Retention) Deployments(ctx context.Context, keepPerApp, limit int) (int64, error) {
	return r.exec(ctx, "deploys", `
		DELETE FROM deployments WHERE id IN (
		    SELECT ranked.id FROM (
		        SELECT d.id, d.app_id, d.spec_id, d.status,
		               row_number() OVER (PARTITION BY d.app_id ORDER BY d.started_at DESC, d.id DESC) AS n,
		               row_number() OVER (PARTITION BY d.app_id, d.spec_id, d.status
		                                  ORDER BY d.started_at DESC, d.id DESC) AS of_revision
		        FROM deployments d
		    ) ranked
		    WHERE ranked.n > $1
		      AND ranked.status NOT IN ('pending', 'building', 'applying', 'awaiting_approval')
		      AND NOT (ranked.status = 'succeeded' AND ranked.of_revision = 1
		               AND EXISTS (SELECT 1 FROM spec_pins p
		                           WHERE p.app_id = ranked.app_id AND p.spec_id = ranked.spec_id))
		    LIMIT $2)`, keepPerApp, limit)
}

// DeletedApps removes the detection and the backup-attempt record of apps
// deleted before before. Apps soft-delete, so the cascade that would have
// removed them never fires; one row per app is still a row per app ever made.
func (r *Retention) DeletedApps(ctx context.Context, before time.Time, limit int) (int64, error) {
	detections, err := r.exec(ctx, "detections", `
		DELETE FROM detections WHERE app_id IN (
		    SELECT d.app_id FROM detections d JOIN apps a ON a.id = d.app_id
		    WHERE a.deleted_at IS NOT NULL AND a.deleted_at < $1
		    LIMIT $2)`, before, limit)
	if err != nil {
		return 0, err
	}
	attempts, err := r.exec(ctx, "backup attempts", `
		DELETE FROM backup_attempts WHERE app_id IN (
		    SELECT t.app_id FROM backup_attempts t JOIN apps a ON a.id = t.app_id
		    WHERE a.deleted_at IS NOT NULL AND a.deleted_at < $1
		    LIMIT $2)`, before, limit)
	return detections + attempts, err
}

// Scans removes up to limit scans that ran before before, except the newest
// scan of each revision of each app (and of each app's source, for scans
// attached to no revision).
func (r *Retention) Scans(ctx context.Context, before time.Time, limit int) (int64, error) {
	return r.exec(ctx, "scans", `
		DELETE FROM app_scans WHERE id IN (
		    SELECT ranked.id FROM (
		        SELECT s.id, s.ran_at,
		               row_number() OVER (PARTITION BY s.app_id, coalesce(s.spec_id, '')
		                                  ORDER BY s.ran_at DESC, s.id DESC) AS n
		        FROM app_scans s
		    ) ranked
		    WHERE ranked.n > 1 AND ranked.ran_at < $1
		    LIMIT $2)`, before, limit)
}

// Sessions removes up to limit sessions that expired or were revoked before
// before. Neither can be used again; the audit log holds the sign-in and the
// sign-out.
func (r *Retention) Sessions(ctx context.Context, before time.Time, limit int) (int64, error) {
	return r.exec(ctx, "sessions", `
		DELETE FROM sessions WHERE id IN (
		    SELECT id FROM sessions
		    WHERE expires_at < $1 OR (revoked_at IS NOT NULL AND revoked_at < $1)
		    LIMIT $2)`, before, limit)
}

// Notifications removes up to limit console notifications past their own
// retain_until. One with none is kept.
func (r *Retention) Notifications(ctx context.Context, now time.Time, limit int) (int64, error) {
	return r.exec(ctx, "notifications", `
		DELETE FROM notifications WHERE id IN (
		    SELECT id FROM notifications
		    WHERE retain_until IS NOT NULL AND retain_until < $1
		    LIMIT $2)`, now, limit)
}

// IdempotencyKeys removes up to limit keys recorded before before. A key
// reused after that is a new request, which is correct: nobody retries
// something from yesterday (migration 000011).
func (r *Retention) IdempotencyKeys(ctx context.Context, before time.Time, limit int) (int64, error) {
	return r.exec(ctx, "idempotency keys", `
		DELETE FROM idempotency_keys WHERE ctid IN (
		    SELECT ctid FROM idempotency_keys WHERE created_at < $1 LIMIT $2)`, before, limit)
}

// SSOFlows removes up to limit sign-in flows that expired before before —
// kept that long so a person arriving late at a failed sign-in can still be
// told why — and the one-time identifiers whose assertions have expired. Both
// used to be deleted on every sign-in, inside the request.
func (r *Retention) SSOFlows(ctx context.Context, before, now time.Time, limit int) (int64, error) {
	flows, err := r.exec(ctx, "sign-in flows", `
		DELETE FROM sso_flows WHERE id IN (
		    SELECT id FROM sso_flows WHERE expires_at < $1 LIMIT $2)`, before, limit)
	if err != nil {
		return 0, err
	}
	replay, err := r.exec(ctx, "sign-in identifiers", `
		DELETE FROM sso_replay WHERE ctid IN (
		    SELECT ctid FROM sso_replay WHERE expires_at < $1 LIMIT $2)`, now, limit)
	return flows + replay, err
}
