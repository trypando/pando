package state

import (
	"context"
	"crypto/subtle"

	"github.com/trypando/pando/internal/core/tokenkey"
	"github.com/trypando/pando/internal/errs"
)

// tokenKeyLabel is what the token key check is an HMAC of. Not a token: no
// token secret has this shape, so the check's digest matches no token's.
const tokenKeyLabel = "pando:token-key-check"

// VerifyKey proves this process holds the install's API token key, and refuses
// to go on if it does not (R-063, issue #72).
//
// The key lives beside the process, not in the database — a key stored with
// the digests it protects protects nothing. So several replicas must each be
// given the same key file, and one given a different key, or one that
// generated its own because its volume was empty, would issue tokens no other
// replica accepts and reject every token the others issued.
//
// The first replica records HMAC(key, tokenKeyLabel); every replica after
// compares its own. The same shape as the secrets key check (Secrets.VerifyKey).
func (t *Tokens) VerifyKey(ctx context.Context) error {
	mine := t.mac(tokenKeyLabel)

	// Seeded by whichever replica gets here first; the rest read the winner's.
	if _, err := t.db.Exec(ctx,
		`INSERT INTO token_key_check (id, digest) VALUES (1, $1) ON CONFLICT (id) DO NOTHING`, mine); err != nil {
		return errs.Wrap(errs.Internal, "Pando could not record the check for the API token key.", err)
	}
	var recorded []byte
	if err := t.db.QueryRow(ctx, `SELECT digest FROM token_key_check WHERE id = 1`).Scan(&recorded); err != nil {
		return errs.Wrap(errs.Internal, "Pando could not read the check for the API token key.", err)
	}
	if subtle.ConstantTimeCompare(mine, recorded) == 1 {
		return nil
	}
	return errs.New(errs.Internal,
		"This Pando process holds a different API token key from the rest of the install, so it would reject every API token the others issued.").
		WithRemedy("Give every Pando replica the same key file: mount one shared volume at /var/lib/pando, or mount the key from one secret at the path set by PANDO_SERVER_TOKEN_KEY_PATH (default " + tokenkey.DefaultPath + "). After restoring a backup, the key file comes from the same backup as the database. Then start this replica again. If no copy of the original key exists, run DELETE FROM token_key_check in Pando's database, start Pando, and issue every API token again.")
}
