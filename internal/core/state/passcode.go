package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// Public with a passcode (R-075a): the anonymous grant carries an argon2id
// digest, and a visitor who enters the passcode holds an unlock — a random
// token in a cookie, stored here only as its SHA-256.

// UnlockLifetime is how long an entered passcode lets a browser in before it
// is asked again. [P] A day: long enough that nobody re-enters it through a
// working session, short enough that a borrowed laptop stops working.
const UnlockLifetime = 24 * time.Hour

// AnonymousAccess reports whether an app is shared with everyone, and whether
// that sharing asks for a passcode.
func (s *AuthzStore) AnonymousAccess(ctx context.Context, appID string) (granted, passcode bool, err error) {
	err = s.db.QueryRow(ctx, `
		SELECT true, passcode_hash IS NOT NULL
		FROM grants
		WHERE app_id = $1 AND plane = 'data' AND principal_kind = 'anonymous'`, appID).Scan(&granted, &passcode)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, errs.Wrap(errs.Internal, "Could not read access for this app.", err)
	}
	return granted, passcode, nil
}

// PasscodeUnlocked reports whether token is a live unlock of the app. Live
// means unexpired and made under the app's current anonymous grant: the
// grant's own row is joined, so an unlock outlives neither the grant nor —
// since SetPasscode clears them — the passcode it was made with.
func (s *AuthzStore) PasscodeUnlocked(ctx context.Context, appID, token string) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM passcode_unlocks u
			JOIN grants g ON g.id = u.grant_id
			WHERE u.token_hash = $1 AND u.app_id = $2 AND u.expires_at > now()
			  AND g.app_id = $2 AND g.principal_kind = 'anonymous' AND g.passcode_hash IS NOT NULL
		)`, tokenHash(token), appID).Scan(&ok)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not read access for this app.", err)
	}
	return ok, nil
}

// AnonymousGrant is an app's grant to everyone, with its passcode digest if it
// has one.
type AnonymousGrant struct {
	ID           string
	PasscodeHash string
}

// AnonymousGrantFor returns the app's anonymous grant, if it has one.
func (g *Grants) AnonymousGrantFor(ctx context.Context, appID string) (AnonymousGrant, bool, error) {
	var a AnonymousGrant
	var hash *string
	err := g.db.QueryRow(ctx, `
		SELECT id, passcode_hash FROM grants
		WHERE app_id = $1 AND plane = 'data' AND principal_kind = 'anonymous'`, appID).Scan(&a.ID, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return AnonymousGrant{}, false, nil
	}
	if err != nil {
		return AnonymousGrant{}, false, errs.Wrap(errs.Internal, "Could not read access for this app.", err)
	}
	if hash != nil {
		a.PasscodeHash = *hash
	}
	return a, true, nil
}

// SetPasscode sets, changes or (with an empty digest) removes the passcode on
// an app's anonymous grant, and ends every unlock made under the old one — in
// one transaction, so there is no moment when the new passcode is set and the
// old visitors are still let in.
func (g *Grants) SetPasscode(ctx context.Context, appID, grantID, passcodeHash string) error {
	tx, err := g.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not change the passcode.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE grants SET passcode_hash = $3
		WHERE id = $1 AND app_id = $2 AND plane = 'data' AND principal_kind = 'anonymous'`,
		grantID, appID, nullable(passcodeHash))
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not change the passcode.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "This app is not shared with everyone, so it has no passcode to change.")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM passcode_unlocks WHERE grant_id = $1`, grantID); err != nil {
		return errs.Wrap(errs.Internal, "Could not change the passcode.", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not change the passcode.", err)
	}
	return nil
}

// Unlock records that a visitor entered the app's passcode, and returns the
// token their browser keeps. Only its SHA-256 is stored: a copy of this table
// lets nobody in.
func (g *Grants) Unlock(ctx context.Context, appID, grantID string) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, errs.Wrap(errs.Internal, "Could not let you in.", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	expires := time.Now().Add(UnlockLifetime).UTC()
	if _, err := g.db.Exec(ctx, `
		INSERT INTO passcode_unlocks (token_hash, app_id, grant_id, expires_at) VALUES ($1, $2, $3, $4)`,
		tokenHash(token), appID, grantID, expires); err != nil {
		return "", time.Time{}, errs.Wrap(errs.Internal, "Could not let you in.", err)
	}
	// Expired unlocks go on the way past, rather than by a job that has to be
	// scheduled and remembered.
	_, _ = g.db.Exec(ctx, `DELETE FROM passcode_unlocks WHERE expires_at < now()`)
	return token, expires, nil
}

// tokenHash is what the unlock row stores, so a leaked table hands nobody a
// working cookie.
//
// SHA-256 and not argon2id, which the passcode itself gets: this hashes the
// 256 random bits Unlock just generated, not anything a person chose. There is
// nothing to guess, so there is nothing for a slow hash to slow down — and
// this runs on every request a visitor makes with the cookie, where argon2id's
// cost would be the app's latency. Unkeyed, unlike API tokens, which are
// HMAC-SHA-256 under the token key (tokens.go). Also the function that checks
// API token digests written unkeyed before that, so they can be rewritten.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token)) // codeql[go/weak-sensitive-data-hashing]
	return hex.EncodeToString(sum[:])
}
