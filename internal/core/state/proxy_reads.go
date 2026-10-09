package state

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
)

// The reads a proxied request makes, folded (issue #93). Each answers what
// several queries answered before, from the same rows and in the same
// transaction-free way: one round trip where there were three, and nothing
// read from anywhere it was not read before.

// ActiveWithUser is Sessions.Active, Users.ByID and the user's groups in one
// query: a session unrevoked and unexpired, the account it belongs to, and the
// groups that account is in through the effective_group_members view, read
// live as R-079 requires. found is false where Active would have found no
// session or ByID no user.
func (s *Sessions) ActiveWithUser(ctx context.Context, sessionID string) (Session, User, []string, bool, error) {
	var (
		sess           Session
		user           User
		email, display *string
		groups         []string
	)
	err := s.db.QueryRow(ctx, `
		SELECT s.id, s.user_id, s.adapter_id, s.expires_at,
		       u.id, u.adapter_id, u.external_id, u.email, u.display_name, u.status,
		       u.must_change_password, u.created_at, coalesce(u.alias_of, ''),
		       coalesce(array(SELECT group_id FROM effective_group_members WHERE user_id = u.id), '{}')
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.id = $1 AND s.revoked_at IS NULL AND s.expires_at > now()`, sessionID).
		Scan(&sess.ID, &sess.UserID, &sess.AdapterID, &sess.ExpiresAt,
			&user.ID, &user.AdapterID, &user.ExternalID, &email, &display, &user.Status,
			&user.MustChangePassword, &user.CreatedAt, &user.AliasOf, &groups)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, User{}, nil, false, nil
	}
	if err != nil {
		return Session{}, User{}, nil, false, errs.Wrap(errs.Internal, "Could not read the session.", err)
	}
	if email != nil {
		user.Email = *email
	}
	if display != nil {
		user.DisplayName = *display
	}
	return sess, user, groups, true, nil
}

// DataFacts reads every fact CheckData decides on, in one query: what IsOwner,
// HasDataGrant, AnonymousAccess and PasscodeUnlocked answer, by the same
// conditions. passcodeToken is the unlock this request brought for the app,
// or "" for none. It decides nothing; authz.CheckData does, in its fixed
// order, and R-029 holds there: nothing about the control plane is read here
// beyond the owner of record, R-072's one implication.
func (s *AuthzStore) DataFacts(ctx context.Context, appID string, p authz.Principal, passcodeToken string) (authz.DataFacts, error) {
	var (
		f    authz.DataFacts
		hash *string
	)
	if passcodeToken != "" {
		h := tokenHash(passcodeToken)
		hash = &h
	}
	err := s.db.QueryRow(ctx, `
		SELECT
		  EXISTS (SELECT 1 FROM apps WHERE id = $1 AND deleted_at IS NULL AND owner_user_id = $2),
		  EXISTS (SELECT 1 FROM grants g
		          WHERE g.app_id = $1 AND g.plane = 'data'
		            AND ((g.principal_kind = 'user'  AND g.principal_id = $2)
		              OR (g.principal_kind = 'token' AND g.principal_id = $3)
		              OR (g.principal_kind = 'group' AND g.principal_id IN (
		                     SELECT group_id FROM effective_group_members WHERE user_id = $2)))),
		  EXISTS (SELECT 1 FROM grants WHERE app_id = $1 AND plane = 'data' AND principal_kind = 'anonymous'),
		  EXISTS (SELECT 1 FROM grants WHERE app_id = $1 AND plane = 'data' AND principal_kind = 'anonymous'
		            AND passcode_hash IS NOT NULL),
		  EXISTS (SELECT 1 FROM passcode_unlocks u
		          JOIN grants g ON g.id = u.grant_id
		          WHERE u.token_hash = $4 AND u.app_id = $1 AND u.expires_at > now()
		            AND g.app_id = $1 AND g.principal_kind = 'anonymous' AND g.passcode_hash IS NOT NULL)`,
		appID, nullable(p.UserID), nullable(accountTokenID(p)), hash).
		Scan(&f.Owner, &f.Grant, &f.Anonymous, &f.Passcode, &f.Unlocked)
	if err != nil {
		return authz.DataFacts{}, errs.Wrap(errs.Internal, "Could not read access for this app.", err)
	}
	return f, nil
}
