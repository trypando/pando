package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/trypando/pando/internal/adapter/identity/local"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// LocalAdapterID is the identity adapter seeded on a fresh install (R-041).
const LocalAdapterID = "idp_local"

// Users reads and writes user records.
type Users struct{ db *DB }

func NewUsers(db *DB) *Users { return &Users{db: db} }

// ByUsername implements local.Users. Username is the local adapter's
// external_id.
func (u *Users) ByUsername(ctx context.Context, username string) (local.Record, bool, error) {
	var r local.Record
	var email, display *string
	err := u.db.QueryRow(ctx, `
		SELECT external_id, email, display_name, coalesce(password_hash, ''), status
		FROM users
		WHERE adapter_id = $1 AND external_id = $2 AND deleted_at IS NULL`,
		LocalAdapterID, username).
		Scan(&r.ExternalID, &email, &display, &r.PasswordHash, &r.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return local.Record{}, false, nil
	}
	if err != nil {
		return local.Record{}, false, errs.Wrap(errs.Internal, "Could not look up the account.", err)
	}
	if email != nil {
		r.Email = *email
	}
	if display != nil {
		r.DisplayName = *display
	}
	return r, true, nil
}

// User is a stored principal.
//
// Tagged because this type is serialized directly by the API, and every other
// type on the wire is lower_snake_case. An untagged struct would put Go field
// names in the API surface — where they would then be a compatibility promise.
type User struct {
	ID          string `json:"id"`
	AdapterID   string `json:"adapter_id"`
	ExternalID  string `json:"external_id"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Status      string `json:"status"`

	MustChangePassword bool `json:"must_change_password"`

	// AliasOf is set on an account whose identities an administrator moved to
	// another (O-1). It stays, suspended, because its ID may be an assertion
	// subject some app holds data under (R-054).
	AliasOf string `json:"alias_of,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// Create inserts a user and returns it.
func (u *Users) Create(ctx context.Context, adapterID, externalID, email, displayName, passwordHash string, mustChange bool) (User, error) {
	user := User{
		ID:                 id.New(id.User),
		AdapterID:          adapterID,
		ExternalID:         externalID,
		Email:              email,
		DisplayName:        displayName,
		Status:             "active",
		MustChangePassword: mustChange,
	}
	err := u.db.QueryRow(ctx, `
		INSERT INTO users (id, adapter_id, external_id, email, display_name, password_hash, must_change_password, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'active')
		RETURNING created_at`,
		user.ID, adapterID, externalID, nullable(email), nullable(displayName), nullable(passwordHash), mustChange).
		Scan(&user.CreatedAt)
	if err != nil {
		return User{}, errs.Wrap(errs.Internal, "Could not create the account.", err)
	}
	return user, nil
}

// ByExternalID resolves an adapter's subject to a Pando user.
func (u *Users) ByExternalID(ctx context.Context, adapterID, externalID string) (User, bool, error) {
	var user User
	var email, display *string
	err := u.db.QueryRow(ctx, `
		SELECT id, adapter_id, external_id, email, display_name, status, must_change_password, created_at, coalesce(alias_of, '')
		FROM users
		WHERE adapter_id = $1 AND external_id = $2 AND deleted_at IS NULL`,
		adapterID, externalID).
		Scan(&user.ID, &user.AdapterID, &user.ExternalID, &email, &display, &user.Status, &user.MustChangePassword, &user.CreatedAt, &user.AliasOf)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, errs.Wrap(errs.Internal, "Could not look up the account.", err)
	}
	if email != nil {
		user.Email = *email
	}
	if display != nil {
		user.DisplayName = *display
	}
	return user, true, nil
}

// ByID returns a user by Pando ID.
func (u *Users) ByID(ctx context.Context, userID string) (User, bool, error) {
	var user User
	var email, display *string
	err := u.db.QueryRow(ctx, `
		SELECT id, adapter_id, external_id, email, display_name, status, must_change_password, created_at, coalesce(alias_of, '')
		FROM users WHERE id = $1`, userID).
		Scan(&user.ID, &user.AdapterID, &user.ExternalID, &email, &display, &user.Status, &user.MustChangePassword, &user.CreatedAt, &user.AliasOf)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, errs.Wrap(errs.Internal, "Could not look up the account.", err)
	}
	if email != nil {
		user.Email = *email
	}
	if display != nil {
		user.DisplayName = *display
	}
	return user, true, nil
}

// List returns every account, newest first.
//
// Includes suspended accounts and excludes deleted ones: suspension is not
// deletion (R-049), and an administrator managing accounts needs to see the
// suspended ones — they are the ones most likely to need attention.
//
// Unpaginated, for the one reader that needs every account at once (the access
// assistant). The API pages with ListPage: an organization of 100,000 people
// is not a screenful (issue #72).
func (u *Users) List(ctx context.Context) ([]User, error) {
	list, _, _, err := u.list(ctx, Page{Limit: -1})
	return list, err
}

// ListPage is one page of List, newest first, with the cursor for the next
// page (empty after the last) and how many accounts match in all.
//
// Page.Query matches the username, display name or email, as Search does.
func (u *Users) ListPage(ctx context.Context, page Page) ([]User, string, int, error) {
	return u.list(ctx, page)
}

// searchUsers is what an account search matches: the username, display name
// and email, joined by a character nobody types. It is, character for
// character, the expression users_search_trgm_idx indexes (000055), which is
// what lets a substring search read the matches rather than every account.
const searchUsers = `(external_id || chr(31) || coalesce(display_name, '') || chr(31) || coalesce(email, ''))`

// searchApps is what an app search matches, the name and slug, as
// apps_search_trgm_idx indexes it (000055).
const searchApps = `(a.name || chr(31) || a.slug)`

// usersMatch is the accounts' search: the username, display name or email
// containing q. Left out entirely when there is no search, rather than written
// as an OR'd test for an empty value, which a cached generic plan cannot serve
// from an index.
func usersMatch(where []string, args []any, q string) ([]string, []any) {
	if q == "" {
		return where, args
	}
	args = append(args, likeEscape(q))
	return append(where, fmt.Sprintf(`%s ILIKE '%%' || $%d || '%%'`, searchUsers, len(args))), args
}

func (u *Users) list(ctx context.Context, page Page) ([]User, string, int, error) {
	var after string
	if _, err := decodeCursor(page.Cursor, &after); err != nil {
		return nil, "", 0, err
	}

	// The accounts list's filter: live, not an alias, matching the search,
	// and among the IDs asked for.
	where := []string{`deleted_at IS NULL`, `alias_of IS NULL`}
	var args []any
	where, args = usersMatch(where, args, page.Query)
	if len(page.IDs) > 0 {
		args = append(args, page.IDs)
		where = append(where, fmt.Sprintf(`id = ANY($%d::text[])`, len(args)))
	}
	from := ` FROM users WHERE ` + strings.Join(where, " AND ")

	// A negative limit is the unpaginated read, and LIMIT NULL is no limit.
	// One more row than the page is read to learn whether there is a next.
	var limit any
	if page.Limit >= 0 {
		limit = page.Size() + 1
	}
	// The cursor narrows the page, not the count.
	list, listArgs := from, append([]any{}, args...)
	if after != "" {
		listArgs = append(listArgs, after)
		list += fmt.Sprintf(` AND id < $%d`, len(listArgs))
	}
	listArgs = append(listArgs, limit)
	rows, err := u.db.Query(ctx, `
		SELECT id, adapter_id, external_id, email, display_name, status, must_change_password, created_at, coalesce(alias_of, '')`+
		list+fmt.Sprintf(`
		ORDER BY id DESC
		LIMIT $%d`, len(listArgs)), listArgs...)
	if err != nil {
		return nil, "", 0, errs.Wrap(errs.Internal, "Could not read the accounts.", err)
	}
	defer rows.Close()

	out := make([]User, 0)
	for rows.Next() {
		var user User
		var email, display *string
		if err := rows.Scan(&user.ID, &user.AdapterID, &user.ExternalID, &email, &display,
			&user.Status, &user.MustChangePassword, &user.CreatedAt, &user.AliasOf); err != nil {
			return nil, "", 0, errs.Wrap(errs.Internal, "Could not read the accounts.", err)
		}
		if email != nil {
			user.Email = *email
		}
		if display != nil {
			user.DisplayName = *display
		}
		out = append(out, user)
	}
	if err := rows.Err(); err != nil {
		return nil, "", 0, errs.Wrap(errs.Internal, "Could not read the accounts.", err)
	}
	if page.Limit < 0 {
		return out, "", len(out), nil
	}

	var next string
	if len(out) > page.Size() {
		out = out[:page.Size()]
		next = encodeCursor(out[len(out)-1].ID)
	}
	total, err := countCapped(ctx, u.db, from, args...)
	if err != nil {
		return nil, "", 0, errs.Wrap(errs.Internal, "Could not count the accounts.", err)
	}
	return out, next, total, nil
}

// SetStatus changes a user's status.
//
// Suspension is not deletion (R-049): this is the endpoint behind PATCH, and it
// must never trigger the destruction rules that DELETE does (R-282).
func (u *Users) SetStatus(ctx context.Context, userID, status string) error {
	return u.SetStatusBy(ctx, userID, status, SuspendedByAdmin)
}

// Who suspended an account (users.suspended_by).
const SuspendedByAdmin = "admin"

// SuspendedBySCIM is the suspended_by value for a SCIM client acting for a
// provider.
func SuspendedBySCIM(providerID string) string { return "scim:" + providerID }

// SetStatusBy is SetStatus on someone's behalf: an administrator, or a SCIM
// client. The R-088 lockout rule applies to both.
func (u *Users) SetStatusBy(ctx context.Context, userID, status, by string) error {
	switch status {
	case "active", "suspended":
	default:
		return errs.New(errs.ValidInvalid, "An account can be set to active or suspended.")
	}

	if status == "active" {
		// An alias stays suspended: its identities belong to another account
		// now, and reactivating it would give back a way in that nothing
		// should reach (O-1). The constraint says so too; this says it in
		// words.
		tag, err := u.db.Exec(ctx,
			`UPDATE users SET status = $2, suspended_by = NULL, updated_at = now()
			 WHERE id = $1 AND alias_of IS NULL`, userID, status)
		if err != nil {
			return errs.Wrap(errs.Internal, "Could not update the account.", err)
		}
		if tag.RowsAffected() == 0 {
			return errs.New(errs.ValidInvalid,
				"This account's sign-in identities were moved to another account, so it stays suspended.").
				WithRemedy("Sign in with the account they were moved to.")
		}
		return nil
	}

	// Suspending the last account that can manage accounts is R-088's lockout
	// reached by a different route.
	//
	// Grants.RevokeInstall refuses to take the last administrator's role away,
	// for the reason given there: the install becomes unadministrable in one
	// click and the only way back is a psql prompt. Suspending that same
	// account does exactly the same thing — it cannot sign in, and nobody else
	// can lift the suspension — and nothing was stopping it. An administrator
	// could do it to themselves in one request, which is how it was found.
	//
	// Checked here rather than in the handler so the console, the CLI and the
	// API all inherit it, which is where the sibling rule already lives.
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the account.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Only this account's own suspension can cause the lockout. An install that
	// had no administrator before is not made worse by suspending someone who
	// was never one — and refusing that would break every install whose access
	// is granted through groups alone.
	var administers bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM grants g
			JOIN roles r ON r.id = g.role_id
			WHERE g.app_id IS NULL AND g.plane = 'control'
			  AND g.principal_kind = 'user' AND g.principal_id = $1
			  AND $2 = ANY (r.verbs))`,
		userID, string(authz.InstallUsersManage)).Scan(&administers); err != nil {
		return errs.Wrap(errs.Internal, "Could not update the account.", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET status = $2, suspended_by = $3, updated_at = now() WHERE id = $1`,
		userID, status, by); err != nil {
		return errs.Wrap(errs.Internal, "Could not update the account.", err)
	}

	if administers {
		// Counted after the update, so the account being suspended is already
		// excluded. A grant held by a group still counts — Pando cannot tell
		// whether that group has reachable members — but a grant held by an
		// account that cannot sign in does not, which is the whole point.
		var remaining int
		if err := tx.QueryRow(ctx, `
			SELECT count(*)
			FROM grants g
			JOIN roles r ON r.id = g.role_id
			LEFT JOIN users u ON g.principal_kind = 'user' AND u.id = g.principal_id
			WHERE g.app_id IS NULL
			  AND g.plane = 'control'
			  AND $1 = ANY (r.verbs)
			  AND (g.principal_kind <> 'user'
			       OR (u.deleted_at IS NULL AND u.status = 'active'))`,
			string(authz.InstallUsersManage)).Scan(&remaining); err != nil {
			return errs.Wrap(errs.Internal, "Could not update the account.", err)
		}
		if remaining == 0 {
			return errs.New(errs.ValidInvalid,
				"This is the only account that can manage accounts, so Pando cannot suspend it.").
				WithRemedy("Make someone else an administrator first, then suspend this one.")
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not update the account.", err)
	}
	return nil
}

// SetPassword replaces a local account's password and clears the
// must-change-on-first-login flag (R-046).
//
// The flag is cleared here rather than by a separate call because there is no
// state in which a person has chosen their own password and must still change
// it. Two statements would make that state reachable by forgetting one.
//
// Local accounts only. An external identity provider owns its own credentials
// (R-044), and a password Pando could change would be a second copy of one.
func (u *Users) SetPassword(ctx context.Context, userID, passwordHash string) error {
	tag, err := u.db.Exec(ctx, `
		UPDATE users
		SET password_hash = $2, must_change_password = false, updated_at = now()
		WHERE id = $1 AND adapter_id = $3 AND deleted_at IS NULL`,
		userID, passwordHash, LocalAdapterID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not change the password.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.ValidInvalid,
			"This account signs in through an external identity provider, so Pando cannot change its password.").
			WithRemedy("Change it where that account lives.")
	}
	return nil
}

// ResetPassword sets a local account's password from outside any session, and
// requires the holder to change it again at the next sign-in.
//
// Separate from SetPassword, which clears must_change_password because the
// person changing it is the person who chose it. Here somebody else chose it:
// it was handed over out of band, so it is a way back in rather than a
// credential, and R-046's rule applies for the same reason it applies on first
// run.
//
// Returns the user ID so the caller can end that account's sessions and record
// what it did. A reset that leaves a stolen session alive has not reset
// anything.
func (u *Users) ResetPassword(ctx context.Context, username, passwordHash string) (string, error) {
	var userID string
	err := u.db.QueryRow(ctx, `
		UPDATE users
		SET password_hash = $3, must_change_password = true, updated_at = now()
		WHERE adapter_id = $1 AND external_id = $2 AND deleted_at IS NULL
		RETURNING id`, LocalAdapterID, username, passwordHash).Scan(&userID)

	if errors.Is(err, pgx.ErrNoRows) {
		return "", errs.Newf(errs.NotFound,
			"This installation has no local account called %q.", username).
			WithRemedy("Accounts from an external identity provider are changed where they live, not here.")
	}
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not reset the password.", err)
	}
	return userID, nil
}

// SetPasswordFor sets a local account's password on an administrator's behalf,
// and whether its holder must change it at the next sign-in.
//
// Returns false when there is no such local account: an external identity
// provider owns its own credentials (R-044).
func (u *Users) SetPasswordFor(ctx context.Context, userID, passwordHash string, mustChange bool) (bool, error) {
	tag, err := u.db.Exec(ctx, `
		UPDATE users
		SET password_hash = $2, must_change_password = $3, updated_at = now()
		WHERE id = $1 AND adapter_id = $4 AND deleted_at IS NULL`,
		userID, passwordHash, mustChange, LocalAdapterID)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not reset the password.", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Profile is the part of an account a person or an administrator edits. A nil
// field is left as it is.
type Profile struct {
	Username    *string
	DisplayName *string
	Email       *string
}

// UpdateProfile changes an account's username, name or email.
//
// Username and email only on a local account: an external identity provider
// owns those, and a change here would be overwritten at the next sign-in —
// or worse, would make the account stop matching its subject (R-054 is about
// users.id, which never changes, but the provider matches on external_id).
func (u *Users) UpdateProfile(ctx context.Context, userID string, p Profile) error {
	user, found, err := u.ByID(ctx, userID)
	if err != nil {
		return err
	}
	if !found {
		return errs.New(errs.NotFound, "There is no account with that ID.")
	}
	if user.AdapterID != LocalAdapterID && (p.Username != nil || p.Email != nil) {
		return errs.New(errs.ValidInvalid,
			"This account comes from an external identity provider, so its username and email are changed there.")
	}
	if p.Username != nil && *p.Username == "" {
		return errs.New(errs.ValidInvalid, "An account needs a username.")
	}

	_, err = u.db.Exec(ctx, `
		UPDATE users SET
		  external_id  = coalesce($2, external_id),
		  display_name = CASE WHEN $3::text IS NULL THEN display_name ELSE nullif($3, '') END,
		  email        = CASE WHEN $4::text IS NULL THEN email ELSE nullif($4, '') END,
		  updated_at   = now()
		WHERE id = $1 AND deleted_at IS NULL`, userID, p.Username, p.DisplayName, p.Email)
	if err != nil {
		if isUniqueViolation(err) {
			return errs.Newf(errs.ValidInvalid, "There is already an account called %q.", *p.Username)
		}
		return errs.Wrap(errs.Internal, "Could not update the account.", err)
	}
	return nil
}

// Search finds active and suspended accounts by username, name or email,
// case-insensitively, at most limit of them, ordered by username. An empty
// query lists the first few.
func (u *Users) Search(ctx context.Context, q string, limit int) ([]User, error) {
	where, args := usersMatch([]string{`deleted_at IS NULL`, `alias_of IS NULL`}, []any{limit}, q)
	rows, err := u.db.Query(ctx, `
		SELECT id, adapter_id, external_id, email, display_name, status, must_change_password, created_at, coalesce(alias_of, '')
		FROM users
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY external_id
		LIMIT $1`, args...)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not search the accounts.", err)
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var user User
		var email, display *string
		if err := rows.Scan(&user.ID, &user.AdapterID, &user.ExternalID, &email, &display,
			&user.Status, &user.MustChangePassword, &user.CreatedAt, &user.AliasOf); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not search the accounts.", err)
		}
		if email != nil {
			user.Email = *email
		}
		if display != nil {
			user.DisplayName = *display
		}
		out = append(out, user)
	}
	return out, rows.Err()
}

// likeEscape neutralizes ILIKE's wildcards in a search typed by a person.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ErrAlreadySetUp is ClaimFirst's refusal once an installation has an account.
var ErrAlreadySetUp = errs.New(errs.ValidInvalid, "This installation is already set up.").
	WithRemedy("Sign in with an existing account. An administrator can create one for you.")

// ClaimFirst creates the installation's first account and makes it an
// administrator, if and only if there is no account yet (R-046).
//
// One transaction, serialized by an advisory lock, so two people submitting the
// setup form at the same moment cannot both become the first administrator:
// the second waits, then finds an account and is refused.
func (u *Users) ClaimFirst(ctx context.Context, username, displayName, passwordHash, roleID string) (User, string, error) {
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return User{}, "", errs.Wrap(errs.Internal, "Could not set up the installation.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Any fixed key; it only has to be the same key for every claimant.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, FirstAccountLock); err != nil {
		return User{}, "", errs.Wrap(errs.Internal, "Could not set up the installation.", err)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE deleted_at IS NULL`).Scan(&count); err != nil {
		return User{}, "", errs.Wrap(errs.Internal, "Could not set up the installation.", err)
	}
	if count > 0 {
		return User{}, "", ErrAlreadySetUp
	}

	user := User{
		ID: id.New(id.User), AdapterID: LocalAdapterID, ExternalID: username,
		DisplayName: displayName, Status: "active",
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (id, adapter_id, external_id, display_name, password_hash, must_change_password, status)
		VALUES ($1, $2, $3, $4, $5, false, 'active')
		RETURNING created_at`,
		user.ID, LocalAdapterID, username, nullable(displayName), passwordHash).Scan(&user.CreatedAt); err != nil {
		return User{}, "", errs.Wrap(errs.Internal, "Could not create the account.", err)
	}

	grantID := id.New(id.Grant)
	if _, err := tx.Exec(ctx, `
		INSERT INTO grants (id, app_id, plane, role_scope, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, NULL, 'control', 'install', 'user', $2, $3, 'system')`,
		grantID, user.ID, roleID); err != nil {
		return User{}, "", errs.Wrap(errs.Internal, "Could not make the account an administrator.", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, "", errs.Wrap(errs.Internal, "Could not set up the installation.", err)
	}
	return user, grantID, nil
}

// NeedsSetup reports whether the installation has no account yet.
func (u *Users) NeedsSetup(ctx context.Context) (bool, error) {
	var any bool
	if err := u.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE deleted_at IS NULL)`).Scan(&any); err != nil {
		return false, errs.Wrap(errs.Internal, "Could not check for existing accounts.", err)
	}
	return !any, nil
}

// Delete soft-deletes a user, which is what fires R-282's destruction rules.
//
// Soft, because the audit log references this ID and R-054 makes users.id the
// assertion subject apps key their own data on. A hard delete would orphan both
// — the audit trail would name an ID nothing explains, and an app would be
// holding rows for a person Pando can no longer describe.
func (u *Users) Delete(ctx context.Context, userID string) error {
	tag, err := u.db.Exec(ctx,
		`UPDATE users SET deleted_at = now(), status = 'deleted', updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`, userID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the account.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "There is no account with that ID.")
	}
	return nil
}

// EnsureLocalAdapter seeds the local identity adapter on a fresh install.
func (u *Users) EnsureLocalAdapter(ctx context.Context) error {
	_, err := u.db.Exec(ctx, `
		INSERT INTO identity_adapters (id, kind, name)
		VALUES ($1, $2, 'Local users')
		ON CONFLICT (id) DO NOTHING`, LocalAdapterID, local.Kind)
	// Two first claims at once both insert it. ON CONFLICT (id) arbitrates
	// only the primary key, so the second can trip the name's unique index
	// before the key's and fail where it should have found the row. Not a
	// target-less DO NOTHING: that would also pass over another adapter
	// holding the name, leaving no local adapter at all.
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		var exists bool
		if qerr := u.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity_adapters WHERE id = $1)`,
			LocalAdapterID).Scan(&exists); qerr == nil && exists {
			return nil
		}
	}
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not set up local accounts.", err)
	}
	return nil
}

// Sessions are server-side rows; the cookie carries only ses_… (design 02 §2.7).
type Sessions struct{ db *DB }

func NewSessions(db *DB) *Sessions { return &Sessions{db: db} }

// Session is an active login.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	AdapterID string    `json:"adapter_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Create issues a session.
func (s *Sessions) Create(ctx context.Context, userID, adapterID string, lifetime time.Duration, userAgent, ip string) (Session, error) {
	sess := Session{
		ID:        id.New(id.Session),
		UserID:    userID,
		AdapterID: adapterID,
		ExpiresAt: time.Now().UTC().Add(lifetime),
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO sessions (id, user_id, adapter_id, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		sess.ID, userID, adapterID, sess.ExpiresAt, nullable(userAgent), nullableInet(ip))
	if err != nil {
		return Session{}, errs.Wrap(errs.Internal, "Could not start a session.", err)
	}
	return sess, nil
}

// Active returns a session if it exists, is unrevoked, and has not expired.
//
// Checked on every request rather than cached. At single-host scale this is one
// indexed lookup; caching it would add a delay to the revocation window (design
// 06 §3.1), and that window is a stated number rather than an accident.
func (s *Sessions) Active(ctx context.Context, sessionID string) (Session, bool, error) {
	var sess Session
	err := s.db.QueryRow(ctx, `
		SELECT id, user_id, adapter_id, expires_at
		FROM sessions
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > now()`,
		sessionID).Scan(&sess.ID, &sess.UserID, &sess.AdapterID, &sess.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, errs.Wrap(errs.Internal, "Could not read the session.", err)
	}
	return sess, true, nil
}

// Revoke ends one session.
func (s *Sessions) Revoke(ctx context.Context, sessionID string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, sessionID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not end the session.", err)
	}
	return nil
}

// RevokeAllForUser ends every session a user holds. Used when an adapter pushes
// a revocation (R-048), which is what makes immediate revocation possible.
func (s *Sessions) RevokeAllForUser(ctx context.Context, userID string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not end the account's sessions.", err)
	}
	return nil
}

// RevokeOthersForUser ends every session a user holds except one.
//
// For a password change: the sessions someone did not know about should end,
// and the one they are typing in should not — being signed out by your own
// password change teaches people that changing it is a risky thing to do.
func (s *Sessions) RevokeOthersForUser(ctx context.Context, userID, keepSessionID string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`, userID, keepSessionID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not end the account's other sessions.", err)
	}
	return nil
}

func nullableInet(s string) any {
	if s == "" {
		return nil
	}
	return s
}
