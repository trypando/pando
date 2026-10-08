package state

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// Secrets stores an app's secret values through a secrets adapter.
//
// The row holds ciphertext or an external reference; there is no plaintext
// column anywhere (R-190, R-191). Core never encrypts — the adapter does, and
// core stores what it is handed.
type Secrets struct {
	db         *DB
	adapter    api.SecretsAdapter
	adapterRef string
}

func NewSecrets(db *DB, adapter api.SecretsAdapter, adapterRef string) *Secrets {
	return &Secrets{db: db, adapter: adapter, adapterRef: adapterRef}
}

// Put stores or rotates a secret.
//
// Rotation increments version, which is how the reconciler learns a restart is
// required (R-193). The value itself never appears in a log or an error,
// because it is a secret.Value the whole way through.
func (s *Secrets) Put(ctx context.Context, appID, key string, v secret.Value) error {
	stored, err := s.adapter.Put(ctx, api.SecretRef{AppID: appID, Key: key}, v)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(ctx, `
		INSERT INTO secrets (id, app_id, key, adapter_ref, ciphertext, external_ref)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (app_id, key) DO UPDATE SET
			ciphertext = EXCLUDED.ciphertext,
			external_ref = EXCLUDED.external_ref,
			version = secrets.version + 1,
			updated_at = now()`,
		id.New(id.Secret), appID, key, s.adapterRef, stored.Ciphertext, nullable(stored.Handle))
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not store the secret.", err)
	}
	s.changedApp(ctx, appID)
	return nil
}

// changedApp tells the reconciler an app's secrets changed: a rotated value
// reaches the app by the reconciler seeing the environment fingerprint move
// (R-193), and a settled app waiting for the slow sweep (O-52) must not wait
// minutes for that. updated_at too, so a pass looking at the app right now
// does not release it to the sweep (ReleaseSettled). Best effort: the secret
// is stored, and the sweep still comes.
func (s *Secrets) changedApp(ctx context.Context, appID string) {
	_, _ = s.db.Exec(ctx, `UPDATE apps SET updated_at = now(), `+dueAgain+` WHERE id = $1`, appID)
}

// Keys lists an app's secret keys and versions. Never values — reading a value
// is a separate, audited endpoint (R-083).
func (s *Secrets) Keys(ctx context.Context, appID string) ([]map[string]any, error) {
	rows, err := s.db.Query(ctx,
		`SELECT key, version, updated_at FROM secrets WHERE app_id = $1 ORDER BY key`, appID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list the app's secrets.", err)
	}
	defer rows.Close()

	out := make([]map[string]any, 0)
	for rows.Next() {
		var key string
		var version int
		var updated any
		if err := rows.Scan(&key, &version, &updated); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list the app's secrets.", err)
		}
		out = append(out, map[string]any{"key": key, "version": version, "updated_at": updated})
	}
	return out, rows.Err()
}

// Resolve decrypts an app's secrets for a deploy.
//
// Called once, at step 11 of the pipeline, to materialize WorkloadPlan.Env. It
// is the only place values leave storage on a deploy path.
func (s *Secrets) Resolve(ctx context.Context, appID string) (map[secretKey]secret.Value, error) {
	rows, err := s.db.Query(ctx,
		`SELECT key, ciphertext, external_ref FROM secrets WHERE app_id = $1`, appID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the app's secrets.", err)
	}
	defer rows.Close()

	out := map[secretKey]secret.Value{}
	for rows.Next() {
		var key string
		var ciphertext []byte
		var externalRef *string
		if err := rows.Scan(&key, &ciphertext, &externalRef); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the app's secrets.", err)
		}

		ref := api.StoredRef{AppID: appID, Key: key, Ciphertext: ciphertext}
		if externalRef != nil {
			ref.Handle = *externalRef
		}

		value, err := s.adapter.Get(ctx, ref)
		if err != nil {
			return nil, err
		}
		out[secretKey(key)] = value
	}
	return out, rows.Err()
}

// Get returns one secret's value, for R-083's separable read.
//
// One key rather than reusing Resolve, which decrypts every secret an app has:
// reading one value should not require decrypting the rest, and the audit event
// this backs names a single key.
func (s *Secrets) Get(ctx context.Context, appID, key string) (secret.Value, error) {
	var ciphertext []byte
	var externalRef *string
	err := s.db.QueryRow(ctx,
		`SELECT ciphertext, external_ref FROM secrets WHERE app_id = $1 AND key = $2`,
		appID, key).Scan(&ciphertext, &externalRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return secret.Value{}, errs.Newf(errs.NotFound, "This app has no secret called %q.", key)
	}
	if err != nil {
		return secret.Value{}, errs.Wrap(errs.Internal, "Could not read the secret.", err)
	}

	ref := api.StoredRef{AppID: appID, Key: key, Ciphertext: ciphertext}
	if externalRef != nil {
		ref.Handle = *externalRef
	}
	return s.adapter.Get(ctx, ref)
}

// Delete removes a secret.
func (s *Secrets) Delete(ctx context.Context, appID, key string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM secrets WHERE app_id = $1 AND key = $2`, appID, key)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the secret.", err)
	}
	s.changedApp(ctx, appID)
	return nil
}

type secretKey = string

// Versions returns each secret's version, without decrypting anything.
//
// This is what the environment fingerprint is built from (R-193). Drift
// detection runs on every tick for every app, and it must never be a reason to
// decrypt a secret — the version answers "did this change", which is the only
// question being asked.
func (s *Secrets) Versions(ctx context.Context, appID string) (map[string]int, error) {
	rows, err := s.db.Query(ctx,
		`SELECT key, version FROM secrets WHERE app_id = $1`, appID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the app's secret versions.", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var key string
		var version int
		if err := rows.Scan(&key, &version); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the app's secret versions.", err)
		}
		out[key] = version
	}
	return out, rows.Err()
}
