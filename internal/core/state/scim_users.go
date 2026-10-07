package state

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// SCIMUser is an account as one provider's SCIM client sees it (R-048).
type SCIMUser struct {
	ID          string // users.id, which is the SCIM resource id
	Key         string // the identity's external_id: what sign-in matches on
	UserName    string
	ExternalID  string
	Resource    []byte
	Email       string
	DisplayName string
	Status      string
	SuspendedBy string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Active is what SCIM's active attribute reports: false for a suspended or
// deleted account, whoever suspended it.
func (u SCIMUser) Active() bool { return u.Status == "active" }

// SCIMUsers is the SCIM view of one provider's accounts. A client sees the
// accounts its provider has an identity on, and nothing else.
type SCIMUsers struct{ db *DB }

func NewSCIMUsers(db *DB) *SCIMUsers { return &SCIMUsers{db: db} }

const scimUserSelect = `
	SELECT u.id, i.external_id, coalesce(i.scim_user_name, ''), coalesce(i.scim_external_id, ''),
	       coalesce(i.scim_resource, '{}'::jsonb)::text, coalesce(u.email, ''), coalesce(u.display_name, ''),
	       u.status, coalesce(u.suspended_by, ''), i.created_at, greatest(i.updated_at, u.updated_at)
	FROM user_identities i JOIN users u ON u.id = i.user_id`

func scanSCIMUser(row pgx.Row) (SCIMUser, error) {
	var u SCIMUser
	var resource string
	err := row.Scan(&u.ID, &u.Key, &u.UserName, &u.ExternalID, &resource, &u.Email, &u.DisplayName,
		&u.Status, &u.SuspendedBy, &u.CreatedAt, &u.UpdatedAt)
	u.Resource = []byte(resource)
	return u, err
}

// List returns the accounts a provider's SCIM client provisioned, optionally
// filtered by one attribute, with the total before paging. Accounts deleted in
// Pando, and ones the client deleted (Forget), are left out: to the client
// they no longer exist.
func (s *SCIMUsers) List(ctx context.Context, adapterID, attr, value string, offset, limit int) ([]SCIMUser, int, error) {
	where := ` WHERE i.adapter_id = $1 AND u.deleted_at IS NULL AND i.scim_resource IS NOT NULL`
	args := []any{adapterID}
	switch attr {
	case "":
	case "userName":
		where += ` AND lower(i.scim_user_name) = lower($2)`
		args = append(args, value)
	case "externalId":
		where += ` AND i.scim_external_id = $2`
		args = append(args, value)
	case "id":
		where += ` AND u.id = $2`
		args = append(args, value)
	case "emails", "emails.value":
		where += ` AND lower(u.email) = lower($2)`
		args = append(args, value)
	default:
		return nil, 0, errs.Newf(errs.ValidInvalid, "Pando cannot filter users by %q.", attr)
	}
	countArgs := args
	args = append(append([]any{}, args...), limit, offset)
	n := len(args)
	rows, err := s.db.Query(ctx, scimUserSelect+where+` ORDER BY i.created_at, u.id LIMIT $`+itoa(n-1)+` OFFSET $`+itoa(n), args...)
	if err != nil {
		return nil, 0, errs.Wrap(errs.Internal, "Could not read the users.", err)
	}
	defer rows.Close()
	out := []SCIMUser{}
	for rows.Next() {
		u, err := scanSCIMUser(rows)
		if err != nil {
			return nil, 0, errs.Wrap(errs.Internal, "Could not read the users.", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, errs.Wrap(errs.Internal, "Could not read the users.", err)
	}

	// totalResults is required in every list response (RFC 7644 §3.4.2), so
	// it is counted — unless the page already says it: a first page that is
	// not full holds every match. That is the common call by far, a client
	// looking one user up by userName before creating it, and it then costs
	// one indexed query instead of two. The count is index-served
	// (user_identities_scim_list_idx, migration 000047).
	if total, ok := totalFromPage(offset, limit, len(out)); ok {
		return out, total, nil
	}
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM user_identities i JOIN users u ON u.id = i.user_id`+where,
		countArgs...).Scan(&total); err != nil {
		return nil, 0, errs.Wrap(errs.Internal, "Could not read the users.", err)
	}
	return out, total, nil
}

// totalFromPage is a SCIM list's totalResults when one page shows it without
// counting: the first page, not full.
func totalFromPage(offset, limit, got int) (int, bool) {
	if offset == 0 && got < limit {
		return got, true
	}
	return 0, false
}

// ByID returns one of a provider's accounts.
func (s *SCIMUsers) ByID(ctx context.Context, adapterID, userID string) (SCIMUser, bool, error) {
	u, err := scanSCIMUser(s.db.QueryRow(ctx, scimUserSelect+`
		WHERE i.adapter_id = $1 AND u.id = $2 AND u.deleted_at IS NULL AND i.scim_resource IS NOT NULL
		ORDER BY i.created_at LIMIT 1`, adapterID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return SCIMUser{}, false, nil
	}
	if err != nil {
		return SCIMUser{}, false, errs.Wrap(errs.Internal, "Could not read the user.", err)
	}
	return u, true, nil
}

// ByKey returns the account an identity reaches, if the identity exists.
func (s *SCIMUsers) ByKey(ctx context.Context, adapterID, key string) (SCIMUser, bool, error) {
	u, err := scanSCIMUser(s.db.QueryRow(ctx, scimUserSelect+`
		WHERE i.adapter_id = $1 AND i.external_id = $2`, adapterID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return SCIMUser{}, false, nil
	}
	if err != nil {
		return SCIMUser{}, false, errs.Wrap(errs.Internal, "Could not read the user.", err)
	}
	return u, true, nil
}

// SCIMUserUpdate is a SCIM client's view of an account, written back.
type SCIMUserUpdate struct {
	Key         string
	UserName    string
	ExternalID  string
	Resource    []byte
	Email       string
	DisplayName string
}

// Write stores what a SCIM client said about an account: the identity's SCIM
// fields, its key if the client changed it, and — when the account came from
// this provider — its email and name. A linked local account's profile is its
// own (see RecordSignIn).
func (s *SCIMUsers) Write(ctx context.Context, adapterID, userID string, w SCIMUserUpdate) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the user.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var oldKey string
	err = tx.QueryRow(ctx, `
		SELECT external_id FROM user_identities WHERE adapter_id = $1 AND user_id = $2
		ORDER BY created_at LIMIT 1 FOR UPDATE`, adapterID, userID).Scan(&oldKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.New(errs.NotFound, "There is no such user for this provider.")
	}
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the user.", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE user_identities SET external_id = $3, scim_user_name = nullif($4, ''), scim_external_id = nullif($5, ''),
		       scim_resource = $6, updated_at = now()
		WHERE adapter_id = $1 AND external_id = $2`,
		adapterID, oldKey, w.Key, w.UserName, w.ExternalID, nullableBytes(w.Resource)); err != nil {
		if isUniqueViolation(err) {
			return ErrSCIMConflict
		}
		return errs.Wrap(errs.Internal, "Could not update the user.", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users SET external_id = $4, email = nullif($5, ''), display_name = nullif($6, ''), updated_at = now()
		WHERE id = $1 AND adapter_id = $2 AND external_id = $3`,
		userID, adapterID, oldKey, w.Key, w.Email, w.DisplayName); err != nil {
		if isUniqueViolation(err) {
			return ErrSCIMConflict
		}
		return errs.Wrap(errs.Internal, "Could not update the user.", err)
	}
	return commit(ctx, tx, "Could not update the user.")
}

// Reactivate lifts a suspension this provider's SCIM client made, and only
// that one. An administrator's suspension stands however often a client
// sends active=true — Entra resends it on every cycle (R-049).
func (s *SCIMUsers) Reactivate(ctx context.Context, adapterID, userID string) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE users SET status = 'active', suspended_by = NULL, updated_at = now()
		WHERE id = $1 AND status = 'suspended' AND suspended_by = $2 AND alias_of IS NULL AND deleted_at IS NULL`,
		userID, SuspendedBySCIM(adapterID))
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not update the user.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Forget is a SCIM DELETE: the client's record of the account goes, and so
// does its membership in the provider's groups. The account and its identity
// stay — suspended by the caller, not deleted (R-049) — so the identity
// cannot be reused by someone else, and a later POST for the same person
// picks the account up again.
func (s *SCIMUsers) Forget(ctx context.Context, adapterID, userID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the user.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE user_identities SET scim_user_name = NULL, scim_external_id = NULL, scim_resource = NULL, updated_at = now()
		WHERE adapter_id = $1 AND user_id = $2`, adapterID, userID); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the user.", err)
	}
	before, err := peopleWhoManage(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the user.", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM group_members m USING groups g
		WHERE m.group_id = g.id AND g.adapter_id = $1 AND m.user_id = $2`, adapterID, userID); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the user.", err)
	}
	if err := refuseLockout(ctx, tx, before); err != nil {
		return err
	}
	return commit(ctx, tx, "Could not remove the user.")
}
