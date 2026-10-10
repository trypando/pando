package state

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// The setup token (R-046, issue #130).
//
// A fresh installation's setup form is public, and before this whoever reached
// it first became the administrator. Now the form also asks for a token Pando
// printed to its own log, so claiming the installation needs whatever it takes
// to read that log — the host, or the deployment's log access — and not merely
// being first to the URL.
//
// One row at most, holding only a digest. The token is made once, when a
// starting replica finds no account and no token, and is gone the moment a
// claim uses it. `pando admin setup-token` replaces it for an operator who no
// longer has the log line.

// ErrSetupTokenWrong is ClaimFirst's refusal of a missing or mistaken token.
var ErrSetupTokenWrong = errs.New(errs.AuthInvalid, "That setup token is not the one this installation is waiting for.").
	WithRemedy("Pando printed the setup token to its log when it first started, as setup_token on the line " +
		"\"this installation is not set up yet\". With Docker Compose: docker compose logs pando | grep setup_token. " +
		"If the log no longer has it, run pando admin setup-token where Pando runs for a new one " +
		"(with Docker Compose: docker compose exec pando pando admin setup-token).")

// newSetupToken is 256 random bits, as text that survives a copy and paste.
func newSetupToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", errs.Wrap(errs.Internal, "Could not make a setup token.", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// EnsureSetupToken makes the setup token if there is none, and returns it.
// When one already exists it returns "" and when that one was made: its value
// cannot be recovered, only replaced.
//
// The caller holds FirstAccountLock and has found no account (bootstrap.Run),
// so replicas starting together make one token between them, and a token is
// never made for an installation that is already set up.
func (u *Users) EnsureSetupToken(ctx context.Context) (string, time.Time, error) {
	token, err := newSetupToken()
	if err != nil {
		return "", time.Time{}, err
	}
	var made time.Time
	err = u.db.QueryRow(ctx, `
		INSERT INTO setup_token (token_hash) VALUES ($1)
		ON CONFLICT (id) DO NOTHING
		RETURNING created_at`, tokenHash(token)).Scan(&made)
	if err == nil {
		return token, made, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, errs.Wrap(errs.Internal, "Could not make a setup token.", err)
	}
	if err := u.db.QueryRow(ctx, `SELECT created_at FROM setup_token WHERE id = 1`).Scan(&made); err != nil {
		return "", time.Time{}, errs.Wrap(errs.Internal, "Could not read the setup token.", err)
	}
	return "", made, nil
}

// ClearSetupToken removes the setup token. The caller holds FirstAccountLock
// and has found or made an account, so the installation is set up and the
// token would only be a way in that nothing needs.
func (u *Users) ClearSetupToken(ctx context.Context) error {
	if _, err := u.db.Exec(ctx, `DELETE FROM setup_token`); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the setup token.", err)
	}
	return nil
}

// ReplaceSetupToken makes a new setup token in place of any there was, for an
// operator who no longer has the one Pando printed. Refused with
// ErrAlreadySetUp once any account exists.
func (u *Users) ReplaceSetupToken(ctx context.Context) (string, error) {
	token, err := newSetupToken()
	if err != nil {
		return "", err
	}
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not make a setup token.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The same lock as ClaimFirst, so a claim cannot land between the check
	// for accounts and the write, and leave a token behind a set-up install.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, FirstAccountLock); err != nil {
		return "", errs.Wrap(errs.Internal, "Could not make a setup token.", err)
	}
	var any bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE deleted_at IS NULL)`).Scan(&any); err != nil {
		return "", errs.Wrap(errs.Internal, "Could not check for existing accounts.", err)
	}
	if any {
		return "", ErrAlreadySetUp
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO setup_token (token_hash) VALUES ($1)
		ON CONFLICT (id) DO UPDATE SET token_hash = EXCLUDED.token_hash, created_at = now()`,
		tokenHash(token)); err != nil {
		return "", errs.Wrap(errs.Internal, "Could not make a setup token.", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", errs.Wrap(errs.Internal, "Could not make a setup token.", err)
	}
	return token, nil
}

// useSetupToken checks token against the installation's and removes it, in
// ClaimFirst's transaction: a claim that fails after this rolls the removal
// back with everything else, and one that succeeds leaves no token.
func useSetupToken(ctx context.Context, tx pgx.Tx, token secret.Value) error {
	var want string
	err := tx.QueryRow(ctx, `SELECT token_hash FROM setup_token WHERE id = 1`).Scan(&want)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSetupTokenWrong
	}
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not check the setup token.", err)
	}
	if subtle.ConstantTimeCompare([]byte(tokenHash(token.Reveal())), []byte(want)) != 1 {
		return ErrSetupTokenWrong
	}
	if _, err := tx.Exec(ctx, `DELETE FROM setup_token`); err != nil {
		return errs.Wrap(errs.Internal, "Could not set up the installation.", err)
	}
	return nil
}
