package state

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Identity is one way into an account: an external provider's subject, bound
// to a users.id (O-1, design 02 §2.1).
type Identity struct {
	AdapterID   string     `json:"adapter_id"`
	AdapterName string     `json:"adapter_name"`
	AdapterKind string     `json:"adapter_kind"`
	ExternalID  string     `json:"external_id"`
	UserID      string     `json:"user_id"`
	Origin      bool       `json:"origin"`
	SCIM        bool       `json:"scim"`
	LastSignIn  *time.Time `json:"last_sign_in_at,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Identities reads and writes user_identities.
type Identities struct{ db *DB }

func NewIdentities(db *DB) *Identities { return &Identities{db: db} }

// Resolve finds the account an external identity signs in to. Deleted and
// suspended accounts are returned as they are, for the caller to refuse:
// treating a deleted account's identity as unknown would let a first sign-in
// make a new account for someone an administrator removed.
func (s *Identities) Resolve(ctx context.Context, adapterID, externalID string) (User, bool, error) {
	var userID string
	err := s.db.QueryRow(ctx,
		`SELECT user_id FROM user_identities WHERE adapter_id = $1 AND external_id = $2`,
		adapterID, externalID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, errs.Wrap(errs.Internal, "Could not look up the account.", err)
	}
	return NewUsers(s.db).ByID(ctx, userID)
}

// ForUser lists the identities that reach an account.
func (s *Identities) ForUser(ctx context.Context, userID string) ([]Identity, error) {
	rows, err := s.db.Query(ctx, `
		SELECT i.adapter_id, a.name, a.kind, i.external_id, i.user_id,
		       (u.adapter_id = i.adapter_id AND u.external_id = i.external_id),
		       i.scim_resource IS NOT NULL, i.last_sign_in_at, i.created_by, i.created_at
		FROM user_identities i
		JOIN identity_adapters a ON a.id = i.adapter_id
		JOIN users u ON u.id = i.user_id
		WHERE i.user_id = $1
		ORDER BY i.created_at`, userID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the account's sign-in identities.", err)
	}
	defer rows.Close()
	out := []Identity{}
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.AdapterID, &i.AdapterName, &i.AdapterKind, &i.ExternalID, &i.UserID,
			&i.Origin, &i.SCIM, &i.LastSignIn, &i.CreatedBy, &i.CreatedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the account's sign-in identities.", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// NewExternal describes an account made for an external identity.
type NewExternal struct {
	AdapterID   string
	ExternalID  string
	Email       string
	DisplayName string
	CreatedBy   string

	// SCIM fields, when a SCIM client made it.
	SCIMUserName   string
	SCIMExternalID string
	SCIMResource   []byte
	Suspended      bool
}

// CreateExternal makes an account for an external identity, in one
// transaction with the identity that reaches it. Used by a first sign-in
// (just-in-time) and by a SCIM push.
func (s *Identities) CreateExternal(ctx context.Context, n NewExternal) (User, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return User{}, errs.Wrap(errs.Internal, "Could not create the account.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	status, suspendedBy := "active", ""
	if n.Suspended {
		status, suspendedBy = "suspended", SuspendedBySCIM(n.AdapterID)
	}
	user := User{
		ID: id.New(id.User), AdapterID: n.AdapterID, ExternalID: n.ExternalID,
		Email: n.Email, DisplayName: n.DisplayName, Status: status,
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (id, adapter_id, external_id, email, display_name, status, suspended_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at`,
		user.ID, n.AdapterID, n.ExternalID, nullable(n.Email), nullable(n.DisplayName), status,
		nullable(suspendedBy)).Scan(&user.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return User{}, errs.New(errs.ValidInvalid, "There is already an account for this identity.")
		}
		return User{}, errs.Wrap(errs.Internal, "Could not create the account.", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_identities (adapter_id, external_id, user_id, scim_user_name, scim_external_id, scim_resource, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		n.AdapterID, n.ExternalID, user.ID, nullable(n.SCIMUserName), nullable(n.SCIMExternalID),
		nullableBytes(n.SCIMResource), n.CreatedBy); err != nil {
		if isUniqueViolation(err) {
			return User{}, errs.New(errs.ValidInvalid, "There is already an account for this identity.")
		}
		return User{}, errs.Wrap(errs.Internal, "Could not create the account.", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, errs.Wrap(errs.Internal, "Could not create the account.", err)
	}
	return user, nil
}

// ErrIdentityTaken is Link's refusal when the identity already reaches a
// different account and the caller did not ask to move it.
var ErrIdentityTaken = errs.New(errs.ValidInvalid,
	"That identity already signs in to a different account.").
	WithRemedy("To move it here, link it again with replace_account set. The other account is kept, " +
		"suspended, as an alias of this one — never deleted, because apps may hold data under its ID.")

// Link attaches an external identity to an account (O-1). An alias is added;
// nothing merges and no users.id changes.
//
// When the identity already reaches another account, it moves only if
// replace is set. The account it leaves becomes an alias of this one if that
// identity was its only way in — suspended, its sessions ended, and never
// deleted, because its ID may be an assertion subject an app holds data under
// (R-054).
//
// Returns the account that became an alias, if any.
func (s *Identities) Link(ctx context.Context, userID, adapterID, externalID, by string, replace bool) (string, error) {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return "", errs.New(errs.ValidInvalid, "An identity needs the provider's ID for the person.").
			WithRemedy("A test sign-in shows it, as the subject.")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var kind string
	err = tx.QueryRow(ctx, `SELECT kind FROM identity_adapters WHERE id = $1`, adapterID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errs.New(errs.NotFound, "There is no identity provider with that ID.")
	}
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
	}
	if kind == "local" {
		return "", errs.New(errs.ValidInvalid, "A local account is linked by giving it a password, not an identity.")
	}

	var target string
	var alias *string
	err = tx.QueryRow(ctx, `SELECT id, alias_of FROM users WHERE id = $1 AND deleted_at IS NULL`, userID).Scan(&target, &alias)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errs.New(errs.NotFound, "There is no account with that ID.")
	}
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
	}
	if alias != nil {
		return "", errs.New(errs.ValidInvalid, "That account is an alias of another. Link the identity to that one.")
	}

	var current string
	err = tx.QueryRow(ctx, `SELECT user_id FROM user_identities WHERE adapter_id = $1 AND external_id = $2 FOR UPDATE`,
		adapterID, externalID).Scan(&current)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_identities (adapter_id, external_id, user_id, created_by)
			VALUES ($1, $2, $3, $4)`, adapterID, externalID, userID, by); err != nil {
			return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
		}
		return "", commit(ctx, tx, "Could not link the identity.")
	case err != nil:
		return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
	case current == userID:
		return "", nil
	case !replace:
		return "", ErrIdentityTaken
	}

	if _, err := tx.Exec(ctx, `
		UPDATE user_identities SET user_id = $3, updated_at = now()
		WHERE adapter_id = $1 AND external_id = $2`, adapterID, externalID, userID); err != nil {
		return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
	}

	// The account it left: an alias if it has no way in left. A local
	// account keeps its password, so it is never one.
	var remaining int
	var origin string
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM user_identities WHERE user_id = $1), a.kind
		FROM users u JOIN identity_adapters a ON a.id = u.adapter_id WHERE u.id = $1`,
		current).Scan(&remaining, &origin); err != nil {
		return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
	}
	aliased := ""
	if remaining == 0 && origin != "local" {
		// The R-088 check applies: aliasing suspends, and an alias that was
		// the last administrator is the lockout reached another way. Moving
		// its grants would be a merge, which O-1 rules out; the administrator
		// grants the surviving account what it needs.
		before, err := lockoutForUsers(ctx, tx, current)
		if err != nil {
			return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users SET alias_of = $2, status = 'suspended', suspended_by = 'alias', updated_at = now()
			WHERE id = $1`, current, userID); err != nil {
			return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, current); err != nil {
			return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
		}
		lost, err := before.managersLost(ctx, tx)
		if err != nil {
			return "", errs.Wrap(errs.Internal, "Could not link the identity.", err)
		}
		if lost {
			return "", errs.New(errs.ValidInvalid,
				"The account this identity leaves is the only one that can manage accounts, and it would be suspended.").
				WithRemedy("Make this account an administrator first, then link the identity.")
		}
		aliased = current
	}
	return aliased, commit(ctx, tx, "Could not link the identity.")
}

// Unlink removes an identity from an account. An account left with no way in
// can still be reached by linking one again.
func (s *Identities) Unlink(ctx context.Context, userID, adapterID, externalID string) error {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM user_identities WHERE user_id = $1 AND adapter_id = $2 AND external_id = $3`,
		userID, adapterID, externalID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not unlink the identity.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "That account has no such identity.")
	}
	return nil
}

// ByVerifiedEmail finds the one active account with this email, for linking
// by an email a provider vouches for. More than one match is no match: a
// guess between two people is how one signs in as the other.
func (s *Identities) ByVerifiedEmail(ctx context.Context, email string) (User, bool, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return User{}, false, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT id FROM users
		WHERE lower(email) = lower($1) AND deleted_at IS NULL AND alias_of IS NULL AND status = 'active'
		LIMIT 2`, email)
	if err != nil {
		return User{}, false, errs.Wrap(errs.Internal, "Could not look up the account.", err)
	}
	var ids []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			return User{}, false, errs.Wrap(errs.Internal, "Could not look up the account.", err)
		}
		ids = append(ids, userID)
	}
	rows.Close()
	if len(ids) != 1 {
		return User{}, false, rows.Err()
	}
	return NewUsers(s.db).ByID(ctx, ids[0])
}

// RecordSignIn notes a sign-in, and refreshes the account's email and name
// from the provider when the account came from it. A linked account's profile
// is its own and is not overwritten by whichever provider signed it in.
func (s *Identities) RecordSignIn(ctx context.Context, adapterID, externalID, userID, email, displayName string) error {
	if _, err := s.db.Exec(ctx, `
		UPDATE user_identities SET last_sign_in_at = now()
		WHERE adapter_id = $1 AND external_id = $2`, adapterID, externalID); err != nil {
		return errs.Wrap(errs.Internal, "Could not record the sign-in.", err)
	}
	_, err := s.db.Exec(ctx, `
		UPDATE users SET
			email = coalesce(nullif($4, ''), email),
			display_name = coalesce(nullif($5, ''), display_name),
			updated_at = now()
		WHERE id = $1 AND adapter_id = $2 AND external_id = $3
		  AND (coalesce(email, '') <> $4 OR coalesce(display_name, '') <> $5)`,
		userID, adapterID, externalID, email, displayName)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the sign-in.", err)
	}
	return nil
}

func commit(ctx context.Context, tx pgx.Tx, msg string) error {
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, msg, err)
	}
	return nil
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
