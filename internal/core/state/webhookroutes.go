package state

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// WebhookRouteSecrets stores each webhook route's secret, sealed by the secrets
// adapter (R-427, R-190): ciphertext or an external reference, never the value.
// The same arrangement as AutoDeploySecrets, per route rather than per app.
type WebhookRouteSecrets struct {
	db         *DB
	adapter    api.SecretsAdapter
	adapterRef string
}

func NewWebhookRouteSecrets(db *DB, adapter api.SecretsAdapter, adapterRef string) *WebhookRouteSecrets {
	return &WebhookRouteSecrets{db: db, adapter: adapter, adapterRef: adapterRef}
}

// webhookRouteSecretRef scopes the secret to its use. "webhook-route:" cannot
// collide with an app's own secrets, keyed by the bare app ID, so the sealed
// value cannot be replayed as one of them.
func webhookRouteSecretRef(appID, route string) api.SecretRef {
	return api.SecretRef{AppID: "webhook-route:" + appID, Key: route}
}

// Put stores or replaces a route's secret.
func (k *WebhookRouteSecrets) Put(ctx context.Context, appID, route string, v secret.Value) error {
	if k == nil || k.adapter == nil {
		return errs.New(errs.StateInvalid,
			"Pando has no secrets adapter configured, so it cannot keep a webhook route's secret.").
			WithRemedy("Configure a secrets adapter, restart Pando, and try again.")
	}
	stored, err := k.adapter.Put(ctx, webhookRouteSecretRef(appID, route), v)
	if err != nil {
		return err
	}
	_, err = k.db.Exec(ctx, `
		INSERT INTO webhook_route_secrets AS t (app_id, route_name, adapter_ref, ciphertext, external_ref)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (app_id, route_name) DO UPDATE SET
			adapter_ref = EXCLUDED.adapter_ref, ciphertext = EXCLUDED.ciphertext,
			external_ref = EXCLUDED.external_ref, version = t.version + 1, updated_at = now()`,
		appID, route, k.adapterRef, stored.Ciphertext, nullable(stored.Handle))
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not store the webhook route's secret.", err)
	}
	return nil
}

// Get opens a route's secret, and says whether it has one.
func (k *WebhookRouteSecrets) Get(ctx context.Context, appID, route string) (secret.Value, bool, error) {
	var ciphertext []byte
	var handle *string
	err := k.db.QueryRow(ctx, `
		SELECT ciphertext, external_ref FROM webhook_route_secrets
		WHERE app_id = $1 AND route_name = $2`, appID, route).Scan(&ciphertext, &handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return secret.Value{}, false, nil
	}
	if err != nil {
		return secret.Value{}, false, errs.Wrap(errs.Internal, "Could not read the webhook route's secret.", err)
	}
	if k.adapter == nil {
		return secret.Value{}, false, errs.New(errs.StateInvalid,
			"A webhook route's secret is stored and no secrets adapter is configured to open it.")
	}
	ref := webhookRouteSecretRef(appID, route)
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

// Names lists which of an app's routes have a secret, without opening any.
func (k *WebhookRouteSecrets) Names(ctx context.Context, appID string) (map[string]bool, error) {
	rows, err := k.db.Query(ctx, `SELECT route_name FROM webhook_route_secrets WHERE app_id = $1`, appID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the webhook routes' secrets.", err)
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the webhook routes' secrets.", err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the webhook routes' secrets.", err)
	}
	return names, nil
}

// Delete removes a route's secret, after which the route refuses every delivery
// (R-430).
func (k *WebhookRouteSecrets) Delete(ctx context.Context, appID, route string) error {
	if _, err := k.db.Exec(ctx,
		`DELETE FROM webhook_route_secrets WHERE app_id = $1 AND route_name = $2`, appID, route); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the webhook route's secret.", err)
	}
	return nil
}

// dropStaleRouteSecrets removes the secrets of routes neither the previous
// revision nor the running one has, as a revision is written (R-427). A route
// taken out by a spec save keeps its secret while it is still running, and
// otherwise would keep it for good: a later revision declaring that name would
// take it up with nobody holding app.secrets.write having given it one.
func dropStaleRouteSecrets(ctx context.Context, tx pgx.Tx, appID string, prev *spec.AppSpec) error {
	live := []string{}
	for _, r := range prev.WebhookRoutes {
		live = append(live, r.Name)
	}
	var body []byte
	err := tx.QueryRow(ctx, `
		SELECT r.body FROM apps a JOIN spec_revisions r ON r.id = a.pinned_spec_id
		WHERE a.id = $1`, appID).Scan(&body)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return errs.Wrap(errs.Internal, "Could not read the running spec.", err)
	}
	var pinned spec.AppSpec
	if err == nil && json.Unmarshal(body, &pinned) == nil {
		for _, r := range pinned.WebhookRoutes {
			live = append(live, r.Name)
		}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM webhook_route_secrets WHERE app_id = $1 AND NOT route_name = ANY($2)`, appID, live); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove a stale webhook route secret.", err)
	}
	return nil
}

// WebhookClaim is where a signature stands after asking to claim it (R-433).
type WebhookClaim int

const (
	// WebhookClaimed means this delivery holds the signature and goes ahead.
	WebhookClaimed WebhookClaim = iota
	// WebhookInFlight means another copy holds it and has no answer yet.
	WebhookInFlight
	// WebhookAccepted means the app has already accepted this signature.
	WebhookAccepted
)

// WebhookDeliveries records the signatures seen on hmac_sha256 routes (R-433),
// in the database because a replay may reach any replica.
type WebhookDeliveries struct{ db *DB }

func NewWebhookDeliveries(db *DB) *WebhookDeliveries { return &WebhookDeliveries{db: db} }

// Claim takes a signature for one delivery until expires, and returns the token
// that names this claim. A row past its expiry counts as absent, so a claim left
// by a replica that died ends on its own.
func (d *WebhookDeliveries) Claim(ctx context.Context, appID, route string, digest []byte, now, expires time.Time) (WebhookClaim, string, error) {
	var token string
	err := d.db.QueryRow(ctx, `
		INSERT INTO webhook_deliveries AS t (app_id, route_name, signature_digest, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (app_id, route_name, signature_digest) DO UPDATE
			SET accepted = false, expires_at = EXCLUDED.expires_at, claim_token = gen_random_uuid()
			WHERE t.expires_at <= $5
		RETURNING claim_token::text`, appID, route, digest, expires, now).Scan(&token)
	if err == nil {
		return WebhookClaimed, token, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, "", errs.Wrap(errs.Internal, "Could not record a webhook delivery.", err)
	}
	var accepted bool
	err = d.db.QueryRow(ctx, `
		SELECT accepted FROM webhook_deliveries
		WHERE app_id = $1 AND route_name = $2 AND signature_digest = $3`, appID, route, digest).Scan(&accepted)
	if errors.Is(err, pgx.ErrNoRows) {
		// The holder released it between the two statements: it failed, and
		// the sender will retry, which finds nothing in the way.
		return WebhookInFlight, "", nil
	}
	if err != nil {
		return 0, "", errs.Wrap(errs.Internal, "Could not read a webhook delivery.", err)
	}
	if accepted {
		return WebhookAccepted, "", nil
	}
	return WebhookInFlight, "", nil
}

// Accept keeps a signature until expires: the app answered 2xx. It takes no
// token. A delivery the app accepted stands even if its claim expired while the
// app was answering and another copy took the signature, or gave it up.
func (d *WebhookDeliveries) Accept(ctx context.Context, appID, route string, digest []byte, expires time.Time) error {
	if _, err := d.db.Exec(ctx, `
		INSERT INTO webhook_deliveries (app_id, route_name, signature_digest, accepted, expires_at)
		VALUES ($1, $2, $3, true, $4)
		ON CONFLICT (app_id, route_name, signature_digest) DO UPDATE
			SET accepted = true, expires_at = EXCLUDED.expires_at`, appID, route, digest, expires); err != nil {
		return errs.Wrap(errs.Internal, "Could not record a webhook delivery.", err)
	}
	return nil
}

// Release gives up the claim token names, so the sender's retry is forwarded. It
// leaves a newer claim and an accepted signature alone.
func (d *WebhookDeliveries) Release(ctx context.Context, appID, route string, digest []byte, token string) error {
	if _, err := d.db.Exec(ctx, `
		DELETE FROM webhook_deliveries
		WHERE app_id = $1 AND route_name = $2 AND signature_digest = $3
			AND claim_token::text = $4 AND NOT accepted`, appID, route, digest, token); err != nil {
		return errs.Wrap(errs.Internal, "Could not record a webhook delivery.", err)
	}
	return nil
}
