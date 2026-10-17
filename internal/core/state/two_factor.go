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

// The two credential fields a local account's second factor keeps (R-405).
const (
	errTurnOn  = "Could not turn on two-factor authentication."
	errTurnOff = "Could not turn off two-factor authentication."

	totpField        = "totp"
	totpPendingField = "totp_pending"
)

// TwoFactor stores local accounts' second factors (R-405): the authenticator
// secret, sealed by the install's secrets adapter like any credential (R-190),
// and recovery codes as digests.
type TwoFactor struct {
	db    *DB
	creds *AdapterCredentials
}

// NewTwoFactor stores second factors with the install's secrets adapter. Its
// own table and scope, so an authenticator's ciphertext cannot be replayed as
// any other credential's.
func NewTwoFactor(db *DB, adapter api.SecretsAdapter, adapterRef string) *TwoFactor {
	return &TwoFactor{db: db, creds: &AdapterCredentials{db: db, adapter: adapter, adapterRef: adapterRef,
		table: "user_two_factor_credentials", column: "user_id", scope: "totp:"}}
}

// TwoFactorStatus is what is known about an account's second factor, none of
// it secret.
type TwoFactorStatus struct {
	EnabledAt *time.Time
	LastStep  int64
	OfferedAt *time.Time

	// RecoveryCodesLeft is how many recovery codes are unused.
	RecoveryCodesLeft int
}

// Status reads an account's second factor. A missing account reads as none.
func (t *TwoFactor) Status(ctx context.Context, userID string) (TwoFactorStatus, error) {
	var st TwoFactorStatus
	var last *int64
	err := t.db.QueryRow(ctx, `
		SELECT totp_enabled_at, totp_last_step, two_factor_offered_at,
		       (SELECT count(*) FROM user_recovery_codes WHERE user_id = u.id AND used_at IS NULL)
		FROM users u WHERE id = $1`, userID).Scan(&st.EnabledAt, &last, &st.OfferedAt, &st.RecoveryCodesLeft)
	if errors.Is(err, pgx.ErrNoRows) {
		return TwoFactorStatus{}, nil
	}
	if err != nil {
		return TwoFactorStatus{}, errs.Wrap(errs.Internal, "Could not read the account's two-factor authentication.", err)
	}
	if last != nil {
		st.LastStep = *last
	}
	return st, nil
}

// PutPending keeps an authenticator secret that has been shown to its holder
// and not yet confirmed, replacing any other.
func (t *TwoFactor) PutPending(ctx context.Context, userID string, v secret.Value) error {
	return t.creds.Put(ctx, userID, totpPendingField, v)
}

// Secrets opens an account's authenticator secrets: the one in use and the
// one waiting to be confirmed, each zero when there is none.
func (t *TwoFactor) Secrets(ctx context.Context, userID string) (current, pending secret.Value, err error) {
	all, err := t.creds.Resolve(ctx, userID)
	if err != nil {
		return secret.Value{}, secret.Value{}, err
	}
	return all[totpField], all[totpPendingField], nil
}

// Enable puts a confirmed authenticator in use, with a fresh set of recovery
// codes, and forgets the pending one. lastStep is the step the confirming code
// was made for, so it cannot sign in as well.
func (t *TwoFactor) Enable(ctx context.Context, userID string, v secret.Value, lastStep int64, digests []string) error {
	// Sealed again under its own field rather than moved: the field is bound
	// into the ciphertext, so a pending secret's cannot be opened as the one
	// in use. Until the transaction below commits, totp_enabled_at says the
	// old state, which is what is in force.
	if err := t.creds.Put(ctx, userID, totpField, v); err != nil {
		return err
	}
	tx, err := t.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, errTurnOn, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE users
		SET totp_enabled_at = now(), totp_last_step = $2,
		    two_factor_offered_at = coalesce(two_factor_offered_at, now()), updated_at = now()
		WHERE id = $1 AND adapter_id = $3 AND deleted_at IS NULL`, userID, lastStep, LocalAdapterID)
	if err != nil {
		return errs.Wrap(errs.Internal, errTurnOn, err)
	}
	if tag.RowsAffected() == 0 {
		return errNotLocal()
	}
	if err := replaceCodes(ctx, tx, userID, digests); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_two_factor_credentials WHERE user_id = $1 AND field = $2`,
		userID, totpPendingField); err != nil {
		return errs.Wrap(errs.Internal, errTurnOn, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, errTurnOn, err)
	}
	return nil
}

// AcceptStep records that a code for step was used, and reports false when a
// code for it or a later step already was — in one statement, so two sign-ins
// racing with the same code cannot both win.
func (t *TwoFactor) AcceptStep(ctx context.Context, userID string, step int64) (bool, error) {
	tag, err := t.db.Exec(ctx, `
		UPDATE users SET totp_last_step = $2
		WHERE id = $1 AND totp_enabled_at IS NOT NULL AND (totp_last_step IS NULL OR totp_last_step < $2)`,
		userID, step)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not check the code.", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UseRecoveryCode spends the recovery code with this digest, and reports
// false when the account has no unused one like it.
func (t *TwoFactor) UseRecoveryCode(ctx context.Context, userID, digest string) (bool, error) {
	tag, err := t.db.Exec(ctx, `
		UPDATE user_recovery_codes SET used_at = now()
		WHERE user_id = $1 AND digest = $2 AND used_at IS NULL`, userID, digest)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not check the recovery code.", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ReplaceRecoveryCodes throws away an account's recovery codes, used or not,
// and keeps these instead.
func (t *TwoFactor) ReplaceRecoveryCodes(ctx context.Context, userID string, digests []string) error {
	tx, err := t.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not make new recovery codes.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := replaceCodes(ctx, tx, userID, digests); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not make new recovery codes.", err)
	}
	return nil
}

func replaceCodes(ctx context.Context, tx pgx.Tx, userID string, digests []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM user_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return errs.Wrap(errs.Internal, "Could not store the recovery codes.", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_recovery_codes (user_id, digest) SELECT $1, unnest($2::text[])`, userID, digests); err != nil {
		return errs.Wrap(errs.Internal, "Could not store the recovery codes.", err)
	}
	return nil
}

// Disable removes an account's second factor: the authenticator, a pending
// one, and the recovery codes. reoffer also forgets that its holder was asked
// to set one up, which an administrator's reset does so they are asked again.
// Reports false when the account had nothing to remove.
func (t *TwoFactor) Disable(ctx context.Context, userID string, reoffer bool) (bool, error) {
	tx, err := t.db.Begin(ctx)
	if err != nil {
		return false, errs.Wrap(errs.Internal, errTurnOff, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var had bool
	if err := tx.QueryRow(ctx, `
		UPDATE users u
		SET totp_enabled_at = NULL, totp_last_step = NULL,
		    two_factor_offered_at = CASE WHEN $2 THEN NULL ELSE two_factor_offered_at END,
		    updated_at = now()
		FROM (SELECT totp_enabled_at FROM users WHERE id = $1) old
		WHERE u.id = $1
		RETURNING old.totp_enabled_at IS NOT NULL`, userID, reoffer).Scan(&had); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, errs.New(errs.NotFound, "There is no such account.")
		}
		return false, errs.Wrap(errs.Internal, errTurnOff, err)
	}
	for _, q := range []string{
		`DELETE FROM user_two_factor_credentials WHERE user_id = $1`,
		`DELETE FROM user_recovery_codes WHERE user_id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, userID); err != nil {
			return false, errs.Wrap(errs.Internal, errTurnOff, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, errs.Wrap(errs.Internal, errTurnOff, err)
	}
	return had, nil
}

// MarkOffered records that the account's holder was asked to set up a second
// factor and chose not to yet, so they are not asked again (R-405).
func (t *TwoFactor) MarkOffered(ctx context.Context, userID string) error {
	if _, err := t.db.Exec(ctx, `
		UPDATE users SET two_factor_offered_at = coalesce(two_factor_offered_at, now())
		WHERE id = $1`, userID); err != nil {
		return errs.Wrap(errs.Internal, "Could not record the answer.", err)
	}
	return nil
}

func errNotLocal() error {
	return errs.New(errs.ValidInvalid,
		"This account signs in through an identity provider, which asks for its second factor. "+
			"Pando's two-factor authentication is for local accounts.").
		WithRemedy("Set up two-factor authentication at your organization's identity provider.")
}
