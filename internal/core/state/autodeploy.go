package state

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// AutoDeployCheck is the last time auto-deploy looked at an app (R-141): what
// the watched branch or release pointed at, and the last commit it tried.
type AutoDeployCheck struct {
	AppID       string    `json:"app_id"`
	CheckedAt   time.Time `json:"checked_at"`
	FoundRef    string    `json:"found_ref,omitempty"`
	FoundCommit string    `json:"found_commit,omitempty"`

	// AttemptedCommit is tried once (O-56). Auto-deploy does not try it
	// again, whatever came of it.
	AttemptedCommit string     `json:"attempted_commit,omitempty"`
	AttemptedAt     *time.Time `json:"attempted_at,omitempty"`
	DeploymentID    string     `json:"deployment_id,omitempty"`

	// Error is why the last check or attempt went nowhere, or empty.
	Error string `json:"error,omitempty"`
}

// AutoDeployChecks stores one AutoDeployCheck per app.
type AutoDeployChecks struct{ db *DB }

func NewAutoDeployChecks(db *DB) *AutoDeployChecks { return &AutoDeployChecks{db: db} }

// ByApp is the app's last check, and whether there has been one.
func (c *AutoDeployChecks) ByApp(ctx context.Context, appID string) (AutoDeployCheck, bool, error) {
	var out AutoDeployCheck
	var ref, commit, attempted, dep, msg *string
	err := c.db.QueryRow(ctx, `
		SELECT app_id, checked_at, found_ref, found_commit, attempted_commit,
		       attempted_at, deployment_id, error
		FROM auto_deploy_checks WHERE app_id = $1`, appID).
		Scan(&out.AppID, &out.CheckedAt, &ref, &commit, &attempted, &out.AttemptedAt, &dep, &msg)
	if errors.Is(err, pgx.ErrNoRows) {
		return AutoDeployCheck{}, false, nil
	}
	if err != nil {
		return AutoDeployCheck{}, false, errs.Wrap(errs.Internal, "Could not read the app's last auto-deploy check.", err)
	}
	out.FoundRef, out.FoundCommit = deref(ref), deref(commit)
	out.AttemptedCommit, out.DeploymentID, out.Error = deref(attempted), deref(dep), deref(msg)
	return out, true, nil
}

// Checked records a check that found ref at commit, or an error, and tried
// nothing. The last attempt is kept: it is what stops a retry.
func (c *AutoDeployChecks) Checked(ctx context.Context, appID, ref, commit, message string) error {
	_, err := c.db.Exec(ctx, `
		INSERT INTO auto_deploy_checks (app_id, checked_at, found_ref, found_commit, error)
		VALUES ($1, now(), NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''))
		ON CONFLICT (app_id) DO UPDATE SET
		    checked_at = now(),
		    found_ref = coalesce(EXCLUDED.found_ref, auto_deploy_checks.found_ref),
		    found_commit = coalesce(EXCLUDED.found_commit, auto_deploy_checks.found_commit),
		    error = EXCLUDED.error,
		    updated_at = now()`,
		appID, ref, commit, message)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the auto-deploy check.", err)
	}
	return nil
}

// Attempted records that auto-deploy tried commit: the deployment it started,
// or why it could not start one.
func (c *AutoDeployChecks) Attempted(ctx context.Context, appID, ref, commit, deploymentID, message string) error {
	_, err := c.db.Exec(ctx, `
		INSERT INTO auto_deploy_checks (app_id, checked_at, found_ref, found_commit,
		    attempted_commit, attempted_at, deployment_id, error)
		VALUES ($1, now(), NULLIF($2, ''), $3, $3, now(), NULLIF($4, ''), NULLIF($5, ''))
		ON CONFLICT (app_id) DO UPDATE SET
		    checked_at = now(),
		    found_ref = EXCLUDED.found_ref,
		    found_commit = EXCLUDED.found_commit,
		    attempted_commit = EXCLUDED.attempted_commit,
		    attempted_at = now(),
		    deployment_id = EXCLUDED.deployment_id,
		    error = EXCLUDED.error,
		    updated_at = now()`,
		appID, ref, commit, deploymentID, message)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the auto-deploy attempt.", err)
	}
	return nil
}
