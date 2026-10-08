package state

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// AutoDeployCheck is the last time auto-deploy looked at an app (R-141): what
// the watched branch or release pointed at, and the last commit it tried.
type AutoDeployCheck struct {
	AppID       string    `json:"app_id"`
	CheckedAt   time.Time `json:"checked_at"`
	FoundRef    string    `json:"found_ref,omitempty"`
	FoundCommit string    `json:"found_commit,omitempty"`

	// AttemptedCommit is tried once (O-58). Auto-deploy does not try it
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

// --- webhook secrets -------------------------------------------------------

// AutoDeploySecrets stores each app's auto-deploy webhook secret, sealed by
// the secrets adapter (R-142, R-190, O-58): ciphertext or an external
// reference, never the value. The same arrangement as SubscriptionSecrets.
type AutoDeploySecrets struct {
	db         *DB
	adapter    api.SecretsAdapter
	adapterRef string
}

func NewAutoDeploySecrets(db *DB, adapter api.SecretsAdapter, adapterRef string) *AutoDeploySecrets {
	return &AutoDeploySecrets{db: db, adapter: adapter, adapterRef: adapterRef}
}

// autoDeploySecretRef scopes the secret to its use. "auto-deploy:" cannot
// collide with an app's own secrets, which are keyed by the bare app ID, so
// the sealed value cannot be replayed as one of them or as this.
func autoDeploySecretRef(appID string) api.SecretRef {
	return api.SecretRef{AppID: "auto-deploy:" + appID, Key: "webhook_secret"}
}

// Put stores or replaces an app's webhook secret.
func (k *AutoDeploySecrets) Put(ctx context.Context, appID string, v secret.Value) error {
	if k == nil || k.adapter == nil {
		return errs.New(errs.StateInvalid,
			"Pando has no secrets adapter configured, so it cannot keep a webhook secret.").
			WithRemedy("Configure a secrets adapter, restart Pando, and try again.")
	}
	stored, err := k.adapter.Put(ctx, autoDeploySecretRef(appID), v)
	if err != nil {
		return err
	}
	_, err = k.db.Exec(ctx, `
		INSERT INTO auto_deploy_webhook_secrets AS t (app_id, adapter_ref, ciphertext, external_ref)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (app_id) DO UPDATE SET
			adapter_ref = EXCLUDED.adapter_ref, ciphertext = EXCLUDED.ciphertext,
			external_ref = EXCLUDED.external_ref, version = t.version + 1, updated_at = now()`,
		appID, k.adapterRef, stored.Ciphertext, nullable(stored.Handle))
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not store the webhook secret.", err)
	}
	return nil
}

// Get opens an app's webhook secret, and says whether it has one.
func (k *AutoDeploySecrets) Get(ctx context.Context, appID string) (secret.Value, bool, error) {
	var ciphertext []byte
	var handle *string
	err := k.db.QueryRow(ctx, `
		SELECT ciphertext, external_ref FROM auto_deploy_webhook_secrets WHERE app_id = $1`, appID).
		Scan(&ciphertext, &handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return secret.Value{}, false, nil
	}
	if err != nil {
		return secret.Value{}, false, errs.Wrap(errs.Internal, "Could not read the webhook secret.", err)
	}
	if k == nil || k.adapter == nil {
		return secret.Value{}, false, errs.New(errs.StateInvalid,
			"A webhook secret is stored and no secrets adapter is configured to open it.")
	}
	ref := autoDeploySecretRef(appID)
	stored := api.StoredRef{AppID: ref.AppID, Key: ref.Key, Ciphertext: ciphertext}
	if handle != nil {
		stored.Handle = *handle
	}
	v, err := k.adapter.Get(ctx, stored)
	if err != nil {
		return secret.Value{}, false, err
	}
	return v, true, nil
}

// Has says whether an app has a webhook secret, without opening it.
func (k *AutoDeploySecrets) Has(ctx context.Context, appID string) (bool, error) {
	var exists bool
	err := k.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM auto_deploy_webhook_secrets WHERE app_id = $1)`, appID).Scan(&exists)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not read the webhook secret.", err)
	}
	return exists, nil
}

// Delete removes an app's webhook secret, which turns its webhook off.
func (k *AutoDeploySecrets) Delete(ctx context.Context, appID string) error {
	if _, err := k.db.Exec(ctx, `DELETE FROM auto_deploy_webhook_secrets WHERE app_id = $1`, appID); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the webhook secret.", err)
	}
	return nil
}
