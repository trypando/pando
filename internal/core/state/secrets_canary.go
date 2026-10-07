package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// canaryRef is what the canary is sealed under. No app has an empty ID, so it
// cannot collide with, or be swapped for, any app's secret.
var canaryRef = api.SecretRef{AppID: "", Key: "pando:secrets-key-canary"}

// VerifyKey proves the secrets adapter holds the install's key, and refuses to
// go on if it does not (R-190, issue #72).
//
// The local adapter's key lives beside the process, not in the database — a
// key stored with the ciphertext it protects protects nothing. So several
// replicas must each be given the same key, and one given a different key, or
// one that generated its own because its volume was empty, used to start
// normally and then write secrets that no other replica could read. Nothing
// noticed until a deploy on another replica failed to decrypt them.
//
// The first replica seals a random value and records its digest; every replica
// after opens it at start. An adapter that seals nothing locally — one that
// keeps secrets in an external store and hands back a reference — has no local
// key to differ, and is not checked.
func (s *Secrets) VerifyKey(ctx context.Context) error {
	if s.adapter == nil {
		return nil
	}

	var (
		ref                string
		ciphertext, digest []byte
	)
	err := s.db.QueryRow(ctx,
		`SELECT adapter_ref, ciphertext, digest FROM secrets_canary WHERE id = 1`).Scan(&ref, &ciphertext, &digest)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && ref != s.adapterRef):
		// None yet, or one from a secrets adapter this install no longer uses.
		return s.seedCanary(ctx, err == nil)
	case err != nil:
		return errs.Wrap(errs.Internal, "Could not read the check for the secrets encryption key.", err)
	}

	value, err := s.adapter.Get(ctx, api.StoredRef{AppID: canaryRef.AppID, Key: canaryRef.Key, Ciphertext: ciphertext})
	if err == nil {
		sum := sha256.Sum256([]byte(value.Reveal()))
		if subtle.ConstantTimeCompare(sum[:], digest) == 1 {
			return nil
		}
	}
	return errs.New(errs.Internal,
		"This Pando process holds a different secrets encryption key from the rest of the install, so it could not read the secrets the others stored.").
		WithDetail("adapter_ref", s.adapterRef).
		WithRemedy("Give every Pando replica the same key file: mount one shared volume at /var/lib/pando, or mount the key from one secret at the path the local secrets adapter is configured with. Then start this replica again.")
}

// seedCanary seals and records a new canary. replace is set when one from
// another adapter is being replaced; otherwise a replica that lost the race to
// seed verifies against the winner's.
func (s *Secrets) seedCanary(ctx context.Context, replace bool) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return errs.Wrap(errs.Internal, "Could not set up the check for the secrets encryption key.", err)
	}
	value := secret.New(base64.RawURLEncoding.EncodeToString(raw))
	stored, err := s.adapter.Put(ctx, canaryRef, value)
	if err != nil {
		return err
	}
	if len(stored.Ciphertext) == 0 {
		// Kept elsewhere, under a reference: nothing local to check.
		_ = s.adapter.Delete(ctx, stored)
		return nil
	}
	sum := sha256.Sum256([]byte(value.Reveal()))

	stmt := `INSERT INTO secrets_canary (id, adapter_ref, ciphertext, digest) VALUES (1, $1, $2, $3)
		ON CONFLICT (id) DO NOTHING`
	if replace {
		stmt = `INSERT INTO secrets_canary (id, adapter_ref, ciphertext, digest) VALUES (1, $1, $2, $3)
			ON CONFLICT (id) DO UPDATE SET adapter_ref = EXCLUDED.adapter_ref,
				ciphertext = EXCLUDED.ciphertext, digest = EXCLUDED.digest, created_at = now()`
	}
	tag, err := s.db.Exec(ctx, stmt, s.adapterRef, stored.Ciphertext, sum[:])
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the check for the secrets encryption key.", err)
	}
	if tag.RowsAffected() == 0 {
		// Another replica seeded first. Its canary is the one to pass.
		return s.VerifyKey(ctx)
	}
	return nil
}
