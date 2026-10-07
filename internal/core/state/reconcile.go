package state

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/trypando/pando/internal/egress"
	"github.com/trypando/pando/internal/errs"
)

// Reconcilable is an app the loop should look at, with the bookkeeping it needs.
type Reconcilable struct {
	App

	ConsecutiveFailures int
	LastFailureAt       *time.Time
	NextAttemptAt       *time.Time
	UnobservableSince   *time.Time
	AppliedEnvHash      string

	// WorkloadImages is what each part of the app ran, for an app whose parts
	// are built separately. Empty for a single-image app, where ImageRef says
	// everything, and for a deployment from before it was recorded.
	//
	// The reconciler restores a missing workload from the recorded image, and
	// with only ImageRef to go on it restored every workload from one image —
	// which for a compose app meant replacing the application with a second
	// copy of whichever service happened to be primary.
	WorkloadImages map[string]WorkloadImage

	// ImageRef is what the last successful deployment ran. The reconciler
	// restores a missing workload with this rather than rebuilding: correcting
	// drift must not become "ship whatever is on the branch now".
	ImageRef string

	// ImageDigest is what the runtime resolved that reference to. A running
	// container reports a digest, not a reference, so this is the only thing
	// that can be compared against one — and a tag can point somewhere new
	// without the reference changing at all.
	ImageDigest string

	// EgressRules is what the last successful deployment ran with. Rules
	// take effect at a deploy (R-183, O-10), so the reconciler restores these
	// rather than resolving policy afresh and changing a running app.
	EgressRules egress.Rules

	// ClaimedAt is the database's time when this pass claimed the app. The
	// visit is released with it, so it is when the app was last looked at.
	ClaimedAt time.Time
}

// Reconciles reads and writes the reconciler's view of an app.
type Reconciles struct{ db *DB }

// NewReconciles returns a store over db.
func NewReconciles(db *DB) *Reconciles { return &Reconciles{db: db} }

// States the reconciler acts on.
//
// The exclusions are the design: `draft` and `proposed` have nothing running,
// `deploying` belongs to the deployment, `archived` is gone — and `failed` is
// absent because R-151 is true by there being no code path, not by a check.
// Adding it to this list is how that requirement gets broken.
var reconcilableStates = []string{StateRunning, StateDegraded, StateStopped}

// Lease is one reconciliation pass's hold on the apps it claimed.
//
// Two passes must never work on one app at once, on one replica or across
// several. That used to be a session advisory lock, which held a pooled
// connection for the whole reconciliation — Apply included, which can take
// minutes — while the reconciliation took more connections of its own. A lease
// is a column, so holding one holds nothing (issue #72).
type Lease struct {
	db     *DB
	holder string
}

// leaseCounter makes each pass's holder unique within this process; the
// replica ID makes it unique across processes and restarts.
var leaseCounter atomic.Uint64

// Lease starts a pass. Nothing is claimed until Claim.
func (r *Reconciles) Lease() *Lease {
	return &Lease{db: r.db, holder: r.db.Replica() + ":" + strconv.FormatUint(leaseCounter.Add(1), 10)}
}

// Holder identifies this pass in the apps it holds.
func (l *Lease) Holder() string { return l.holder }

// reconcilableIn is reconcilableStates as SQL. Literal rather than a parameter
// so the claim can use the partial index whose predicate names them.
var reconcilableIn = func() string {
	quoted := make([]string, len(reconcilableStates))
	for i, s := range reconcilableStates {
		quoted[i] = "'" + s + "'"
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}()

// Cutoff is the database's time minus revisit: an app visited after it is not
// due again in the pass that asked.
//
// A pass fixes this once, before its first claim. Every app it visits is
// released with the time its visit started, which is after the cutoff, so the
// pass cannot claim the same app twice and ends when nothing older is left.
// revisit spreads the same guarantee across replicas: an app another replica
// visited less than revisit ago is left for later rather than visited twice.
//
// The database's clock and not the caller's, because replicas' clocks differ
// and the lease is compared against the database's.
func (r *Reconciles) Cutoff(ctx context.Context, revisit time.Duration) (time.Time, error) {
	var cutoff time.Time
	if err := r.db.QueryRow(ctx, `SELECT now() - $1::interval`, revisit).Scan(&cutoff); err != nil {
		return time.Time{}, errs.Wrap(errs.Internal, "Could not read which apps need attention.", err)
	}
	return cutoff, nil
}

// Claim leases up to limit apps the loop should reconcile now.
//
// Due means: in a state the loop acts on, out of backoff, and neither held by
// another pass nor visited since cutoff. Least recently visited first, so every
// app gets its turn: ordering by updated_at, as this used to, visited the same
// 200 apps forever because a healthy pass changes nothing on the row. SKIP
// LOCKED hands each concurrent claimant — another replica, usually — a disjoint
// set rather than the same rows to contend for.
//
// Apps in backoff are filtered here rather than skipped in the loop, so an app
// waiting five minutes costs one row in a WHERE clause instead of a goroutine
// per tick that wakes up and does nothing. Backoff is measured on the loop's
// clock (now), as RecordFailure sets it; the lease on the database's.
//
// The last successful deployment's facts are read for the claimed rows only,
// each an index probe on deployments (app_id, started_at DESC), so a claim's
// cost follows its limit and not the number of apps on the install.
func (l *Lease) Claim(ctx context.Context, now, cutoff time.Time, limit int, lease time.Duration) ([]Reconcilable, error) {
	rows, err := l.db.Query(ctx, `
		WITH claimed AS (
		    UPDATE apps
		    SET reconcile_lease_until = now() + $5::interval,
		        reconcile_lease_holder = $4
		    WHERE id IN (
		        SELECT id FROM apps
		        WHERE deleted_at IS NULL
		          AND state IN `+reconcilableIn+`
		          AND pinned_spec_id IS NOT NULL
		          AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
		          AND (reconcile_lease_until IS NULL OR reconcile_lease_until < $2)
		        ORDER BY reconcile_lease_until NULLS FIRST, id
		        LIMIT $3
		        FOR UPDATE SKIP LOCKED
		    )
		    RETURNING id, name, slug, owner_user_id, state, desired_state,
		              pinned_spec_id, source, created_at, updated_at,
		              consecutive_failures, last_failure_at,
		              next_attempt_at, unobservable_since,
		              coalesce(applied_env_fingerprint, '') AS applied_env_fingerprint,
		              now() AS claimed_at
		)
		SELECT a.id, a.name, a.slug, a.owner_user_id, a.state, a.desired_state,
		       a.pinned_spec_id, a.source, a.created_at, a.updated_at,
		       a.consecutive_failures, a.last_failure_at,
		       a.next_attempt_at, a.unobservable_since,
		       a.applied_env_fingerprint, a.claimed_at,
		       coalesce((
		           SELECT d.image_ref FROM deployments d
		           WHERE d.app_id = a.id AND d.status = 'succeeded' AND d.image_ref IS NOT NULL
		           ORDER BY d.started_at DESC LIMIT 1
		       ), ''),
		       coalesce((
		           SELECT d.image_digest FROM deployments d
		           WHERE d.app_id = a.id AND d.status = 'succeeded' AND d.image_digest IS NOT NULL
		           ORDER BY d.started_at DESC LIMIT 1
		       ), ''),
		       (
		           SELECT d.workload_images FROM deployments d
		           WHERE d.app_id = a.id AND d.status = 'succeeded' AND d.workload_images IS NOT NULL
		           ORDER BY d.started_at DESC LIMIT 1
		       ),
		       (
		           SELECT d.egress_rules FROM deployments d
		           WHERE d.app_id = a.id AND d.status = 'succeeded'
		           ORDER BY d.started_at DESC LIMIT 1
		       )
		FROM claimed a
		ORDER BY a.id
	`, now, cutoff, limit, l.holder, lease)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read which apps need attention.", err)
	}
	defer rows.Close()

	var out []Reconcilable
	for rows.Next() {
		var a Reconcilable
		var owner, pinned *string
		var source []byte

		var workloadImages, egressRules []byte
		if err := rows.Scan(&a.ID, &a.Name, &a.Slug, &owner, &a.State, &a.DesiredState,
			&pinned, &source, &a.CreatedAt, &a.UpdatedAt,
			&a.ConsecutiveFailures, &a.LastFailureAt,
			&a.NextAttemptAt, &a.UnobservableSince,
			&a.AppliedEnvHash, &a.ClaimedAt, &a.ImageRef, &a.ImageDigest, &workloadImages, &egressRules); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read which apps need attention.", err)
		}
		if len(workloadImages) > 0 {
			// A deployment written before this column, or by a version that
			// wrote something else into it, leaves the map empty: the
			// reconciler then knows nothing per workload, which is where it
			// started, rather than knowing something wrong.
			_ = json.Unmarshal(workloadImages, &a.WorkloadImages)
		}
		if len(egressRules) > 0 {
			// A deployment from before egress was enforced records none, and
			// ran unrestricted, which is what an empty value restores.
			_ = json.Unmarshal(egressRules, &a.EgressRules)
		}
		if owner != nil {
			a.OwnerUserID = *owner
		}
		if pinned != nil {
			a.PinnedSpecID = *pinned
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read which apps need attention.", err)
	}
	return out, nil
}

// Extend pushes every lease this pass still holds lease further out, and
// returns the apps it still holds.
//
// An app missing from the result is no longer this pass's — its lease ran out
// before it was extended, and another pass may have it — and its work must stop.
func (l *Lease) Extend(ctx context.Context, lease time.Duration) (map[string]bool, error) {
	rows, err := l.db.Query(ctx, `
		UPDATE apps SET reconcile_lease_until = now() + $2::interval
		WHERE reconcile_lease_holder = $1 AND reconcile_lease_until > now()
		RETURNING id`, l.holder, lease)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not extend the reconciliation lease.", err)
	}
	defer rows.Close()
	held := map[string]bool{}
	for rows.Next() {
		var appID string
		if err := rows.Scan(&appID); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not extend the reconciliation lease.", err)
		}
		held[appID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not extend the reconciliation lease.", err)
	}
	return held, nil
}

// Release lets go of one app.
//
// visited is when this pass's visit to it started, which orders the app behind
// every app visited less recently. Nil for an app the pass claimed and never
// started, which goes back to the front.
//
// Only if this pass still holds it: a lease that ran out belongs to whoever
// claimed it next.
func (l *Lease) Release(ctx context.Context, appID string, visited *time.Time) error {
	_, err := l.db.Exec(context.WithoutCancel(ctx), `
		UPDATE apps SET reconcile_lease_until = $3, reconcile_lease_holder = NULL
		WHERE id = $1 AND reconcile_lease_holder = $2`, appID, l.holder, visited)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not release the reconciliation lease.", err)
	}
	return nil
}

// MarkUnobservable records that the adapter could not be reached.
//
// It touches neither state nor the failure counter. An adapter being down is a
// platform problem, not app failure — otherwise restarting the Docker daemon
// marks every app on the host as failed (design 05 §2).
func (r *Reconciles) MarkUnobservable(ctx context.Context, appID, reason string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE apps
		SET unobservable_since = coalesce(unobservable_since, now()),
		    last_reconcile_error = $2,
		    updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
	`, appID, reason)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record that the app could not be observed.", err)
	}
	return nil
}

// ClearUnobservable is called on the first successful Observe.
func (r *Reconciles) ClearUnobservable(ctx context.Context, appID string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE apps SET unobservable_since = NULL, updated_at = now()
		WHERE id = $1 AND unobservable_since IS NOT NULL
	`, appID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not clear the unobservable marker.", err)
	}
	return nil
}

// RecordFailure increments the failure counter and returns the new count.
//
// The window is an idle timeout measured from the last failure, not a fixed
// window from the first. Measured from the first it is unreachable: R-149's
// backoff caps at five minutes, so ten attempts span 31.3 minutes against a
// 30-minute window that resets at 30 — the app retries forever and is never
// given up on, which is what R-150 exists to prevent.
//
// As an idle timeout it means what the requirement means. Consecutive failures
// always reach the threshold however long backoff stretches them out, and
// unrelated failures a day apart never accumulate.
func (r *Reconciles) RecordFailure(ctx context.Context, appID, reason string, window time.Duration, next time.Time) (int, error) {
	var count int
	err := r.db.QueryRow(ctx, `
		UPDATE apps
		SET consecutive_failures = CASE
		        WHEN last_failure_at IS NULL OR last_failure_at < now() - $2::interval
		        THEN 1
		        ELSE consecutive_failures + 1
		    END,
		    last_failure_at = now(),
		    next_attempt_at = $3,
		    last_reconcile_error = $4,
		    updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING consecutive_failures
	`, appID, window, next, reason).Scan(&count)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not record the failure.", err)
	}
	return count, nil
}

// ClearFailures resets the counter and backoff.
//
// Called only when an app is running with health passing. A flapping app that
// recovers between failures still accumulates toward the threshold, which is
// correct: flapping is a failure mode, not a recovery.
func (r *Reconciles) ClearFailures(ctx context.Context, appID string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE apps
		SET consecutive_failures = 0, last_failure_at = NULL,
		    next_attempt_at = NULL, last_reconcile_error = NULL, updated_at = now()
		WHERE id = $1 AND consecutive_failures > 0
	`, appID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not reset the failure count.", err)
	}
	return nil
}

// SetAppliedEnvFingerprint records the environment an app was last applied with.
//
// R-193's only mechanism. Observe returns no environment and deliberately
// should not — reading it back would require every runtime adapter to handle
// secret-bearing data, which is what the adapter interface works to avoid — so
// a rotated secret is invisible to observation and is detected by comparing
// this instead (design 02 §2.4).
func (r *Reconciles) SetAppliedEnvFingerprint(ctx context.Context, appID, fingerprint string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE apps SET applied_env_fingerprint = $2, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
	`, appID, fingerprint)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the applied environment.", err)
	}
	return nil
}
