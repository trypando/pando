package state

import (
	"context"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// AuthzStore implements authz.Store against Postgres.
type AuthzStore struct{ db *DB }

func NewAuthzStore(db *DB) *AuthzStore { return &AuthzStore{db: db} }

// UserStatus returns a user's status, or "deleted" if no such user.
//
// A missing user resolving to "deleted" rather than an error matters for R-059:
// a delegated token whose owner was hard-removed must be orphaned, not produce a
// lookup failure that some caller might treat as transient and retry past.
func (s *AuthzStore) UserStatus(ctx context.Context, userID string) (string, error) {
	var status string
	err := s.db.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "deleted", nil
	}
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not read the account's status.", err)
	}
	return status, nil
}

// ControlGrantsFor returns control-plane grants matching the principal.
//
// Group membership is joined live (R-079) rather than read from the principal,
// so a grant made to a group the caller joined a moment ago is honored without
// waiting for a cache.
func (s *AuthzStore) ControlGrantsFor(ctx context.Context, appID string, p authz.Principal) ([]authz.Grant, error) {
	rows, err := s.db.Query(ctx, `
		SELECT g.id, g.app_id, g.plane, g.principal_kind, coalesce(g.principal_id, ''), coalesce(g.role_id, ''),
		       `+grantRoleColumns+`
		FROM grants g
		LEFT JOIN roles r ON r.id = g.role_id
		WHERE g.app_id = $1
		  AND g.plane = 'control'
		  AND (
		        (g.principal_kind = 'user'  AND g.principal_id = $2)
		     OR (g.principal_kind = 'token' AND g.principal_id = $3)
		     OR (g.principal_kind = 'group' AND g.principal_id IN (
		            SELECT group_id FROM effective_group_members WHERE user_id = $2))
		  )`,
		appID, nullable(p.UserID), nullable(accountTokenID(p)))
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read permissions for this app.", err)
	}
	defer rows.Close()

	var out []authz.Grant
	for rows.Next() {
		g, err := scanGrantWithRole(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read permissions for this app.", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ControlGrantsForApps is ControlGrantsFor for several apps in one query
// (authz.BatchStore), with an entry for every app asked about, so a list that
// shows what the caller may do on each row asks once rather than per row
// (issue #72).
func (s *AuthzStore) ControlGrantsForApps(ctx context.Context, appIDs []string, p authz.Principal) (map[string][]authz.Grant, error) {
	out := make(map[string][]authz.Grant, len(appIDs))
	for _, appID := range appIDs {
		out[appID] = nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT g.id, g.app_id, g.plane, g.principal_kind, coalesce(g.principal_id, ''), coalesce(g.role_id, ''),
		       `+grantRoleColumns+`
		FROM grants g
		LEFT JOIN roles r ON r.id = g.role_id
		WHERE g.app_id = ANY ($1)
		  AND g.plane = 'control'
		  AND (
		        (g.principal_kind = 'user'  AND g.principal_id = $2)
		     OR (g.principal_kind = 'token' AND g.principal_id = $3)
		     OR (g.principal_kind = 'group' AND g.principal_id IN (
		            SELECT group_id FROM effective_group_members WHERE user_id = $2))
		  )`,
		appIDs, nullable(p.UserID), nullable(accountTokenID(p)))
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read permissions for these apps.", err)
	}
	defer rows.Close()
	for rows.Next() {
		g, err := scanGrantWithRole(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read permissions for these apps.", err)
		}
		out[g.AppID] = append(out[g.AppID], g)
	}
	return out, rows.Err()
}

// InstallGrantsFor returns the principal's installation-wide grants (O-17).
//
// `app_id IS NULL` is the whole definition of install scope, and the schema
// guarantees such a row carries an install-scoped role: grants.role_scope is
// tied to roles.scope by a composite foreign key, and a CHECK ties role_scope
// to whether app_id is null. So this cannot return a grant carrying app verbs
// however the row was written.
//
// Group membership counts, as it does per app. Token principals do too — an
// account token acts for its user (design 06 §2) — which means an install-wide
// administrator's token is administrative, and that is the point of R-262's
// "an agent holds what its user holds" rather than an exception to it.
func (s *AuthzStore) InstallGrantsFor(ctx context.Context, p authz.Principal) ([]authz.Grant, error) {
	rows, err := s.db.Query(ctx, `
		SELECT g.id, coalesce(g.app_id, ''), g.plane, g.principal_kind,
		       coalesce(g.principal_id, ''), coalesce(g.role_id, ''),
		       `+grantRoleColumns+`
		FROM grants g
		LEFT JOIN roles r ON r.id = g.role_id
		WHERE g.app_id IS NULL
		  AND g.plane = 'control'
		  AND (
		        (g.principal_kind = 'user'  AND g.principal_id = $1)
		     OR (g.principal_kind = 'token' AND g.principal_id = $2)
		     OR (g.principal_kind = 'group' AND g.principal_id IN (
		            SELECT group_id FROM effective_group_members WHERE user_id = $1))
		  )`,
		nullable(p.UserID), nullable(accountTokenID(p)))
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read installation permissions.", err)
	}
	defer rows.Close()

	var out []authz.Grant
	for rows.Next() {
		g, err := scanGrantWithRole(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read installation permissions.", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// grantRoleColumns are the role columns read with a grant, from a LEFT JOIN
// of roles as r, so the authorizer does not ask for each grant's role in a
// query of its own. A grant whose role is missing reads as a role with no
// verbs, which is what Role answers for one.
const grantRoleColumns = `coalesce(r.name, ''), coalesce(r.builtin, false), coalesce(r.verbs, '{}')`

// scanGrantWithRole reads a grant and its joined role (grantRoleColumns).
func scanGrantWithRole(rows pgx.Rows) (authz.Grant, error) {
	var g authz.Grant
	var role authz.Role
	var verbs []string
	if err := rows.Scan(&g.ID, &g.AppID, &g.Plane, &g.PrincipalKind, &g.PrincipalID, &g.RoleID,
		&role.Name, &role.Builtin, &verbs); err != nil {
		return authz.Grant{}, err
	}
	role.ID = g.RoleID
	for _, v := range verbs {
		role.Verbs = append(role.Verbs, authz.Verb(v))
	}
	g.Role = &role
	return g, nil
}

// InstallVerbsFor returns every installation-wide verb the principal holds.
//
// For GET /me, so the console can decide whether to show the Admin entry and
// what to put in it (R-265) without guessing. The list is the server's answer,
// which is what keeps the console a client of the API rather than a second
// opinion about authorization (R-261).
func (s *AuthzStore) InstallVerbsFor(ctx context.Context, p authz.Principal) ([]string, error) {
	grants, err := s.InstallGrantsFor(ctx, p)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var verbs []string
	for _, g := range grants {
		// Read with the grant (grantRoleColumns).
		role := g.Role
		if role == nil {
			r, err := s.Role(ctx, g.RoleID)
			if err != nil {
				return nil, err
			}
			role = &r
		}
		for _, verb := range role.Verbs {
			if !seen[string(verb)] {
				seen[string(verb)] = true
				verbs = append(verbs, string(verb))
			}
		}
	}
	sort.Strings(verbs)
	return verbs, nil
}

// DeployApprovers returns the active people who may approve a deploy of the
// app: holders of install.deploys.approve, and holders of app.deploy.approve
// on this app, directly or through a group (R-155). For telling them a
// request is waiting (R-159), not for deciding whether they may: that is
// the authorizer's, which also applies host policy.
func (s *AuthzStore) DeployApprovers(ctx context.Context, appID string) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		WITH holding AS (
		    SELECT g.principal_kind, g.principal_id
		    FROM grants g JOIN roles r ON r.id = g.role_id
		    WHERE g.plane = 'control'
		      AND ((g.app_id IS NULL AND $2 = ANY (r.verbs))
		        OR (g.app_id = $1 AND $3 = ANY (r.verbs)))
		)
		SELECT u.id FROM users u
		WHERE u.status = 'active' AND u.deleted_at IS NULL
		  AND (u.id IN (SELECT principal_id FROM holding WHERE principal_kind = 'user')
		    OR u.id IN (SELECT m.user_id FROM effective_group_members m
		                JOIN holding h ON h.principal_kind = 'group' AND h.principal_id = m.group_id))
		ORDER BY u.id`,
		appID, string(authz.InstallDeploysApprove), string(authz.AppDeployApprove))
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read who may approve this app's deploys.", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read who may approve this app's deploys.", err)
		}
		out = append(out, userID)
	}
	return out, rows.Err()
}

// InstallVerbHolders returns the active people holding an install verb,
// directly or through a group. For telling them something — an update is
// available (R-362) — never for deciding what they may do: that is the
// authorizer's, which also applies host policy.
func (s *AuthzStore) InstallVerbHolders(ctx context.Context, verb authz.Verb) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		WITH holding AS (
		    SELECT g.principal_kind, g.principal_id
		    FROM grants g JOIN roles r ON r.id = g.role_id
		    WHERE g.plane = 'control' AND g.app_id IS NULL AND $1 = ANY (r.verbs)
		)
		SELECT u.id FROM users u
		WHERE u.status = 'active' AND u.deleted_at IS NULL
		  AND (u.id IN (SELECT principal_id FROM holding WHERE principal_kind = 'user')
		    OR u.id IN (SELECT m.user_id FROM effective_group_members m
		                JOIN holding h ON h.principal_kind = 'group' AND h.principal_id = m.group_id))
		ORDER BY u.id`, string(verb))
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read who holds "+string(verb)+".", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read who holds "+string(verb)+".", err)
		}
		out = append(out, userID)
	}
	return out, rows.Err()
}

// IsOwner reports whether userID is the app's owner of record (R-031).
func (s *AuthzStore) IsOwner(ctx context.Context, appID, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	var owner bool
	err := s.db.QueryRow(ctx,
		`SELECT owner_user_id = $2 FROM apps WHERE id = $1 AND deleted_at IS NULL`,
		appID, userID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not read the app's owner.", err)
	}
	return owner, nil
}

// HasDataGrant reports whether the principal may use the app.
func (s *AuthzStore) HasDataGrant(ctx context.Context, appID string, p authz.Principal) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM grants g
			WHERE g.app_id = $1
			  AND g.plane = 'data'
			  AND (
			        (g.principal_kind = 'user'  AND g.principal_id = $2)
			     OR (g.principal_kind = 'token' AND g.principal_id = $3)
			     OR (g.principal_kind = 'group' AND g.principal_id IN (
			            SELECT group_id FROM effective_group_members WHERE user_id = $2))
			  )
		)`, appID, nullable(p.UserID), nullable(accountTokenID(p))).Scan(&exists)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not read access for this app.", err)
	}
	return exists, nil
}

// Role returns a role by ID.
func (s *AuthzStore) Role(ctx context.Context, roleID string) (authz.Role, error) {
	var r authz.Role
	var verbs []string
	err := s.db.QueryRow(ctx,
		`SELECT id, name, builtin, verbs FROM roles WHERE id = $1`, roleID).
		Scan(&r.ID, &r.Name, &r.Builtin, &verbs)
	if errors.Is(err, pgx.ErrNoRows) {
		// A grant referencing a missing role grants nothing. The FK makes this
		// unreachable; returning an empty role rather than an error keeps a
		// data problem from becoming an outage on the authorization path.
		return authz.Role{ID: roleID}, nil
	}
	if err != nil {
		return authz.Role{}, errs.Wrap(errs.Internal, "Could not read the role.", err)
	}
	for _, v := range verbs {
		r.Verbs = append(r.Verbs, authz.Verb(v))
	}
	return r, nil
}

// GroupsForUser resolves group membership live (R-079).
func (s *AuthzStore) GroupsForUser(ctx context.Context, userID string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `SELECT group_id FROM effective_group_members WHERE user_id = $1`, userID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read group membership.", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read group membership.", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// accountTokenID returns the principal's own ID when it is an account token.
//
// A delegated token must never match a grant under its own ID — it holds no
// grants and resolves entirely through its owner (R-058, R-059). Returning empty
// here is what keeps that true at the query level rather than by convention.
func accountTokenID(p authz.Principal) string {
	if p.Kind == authz.KindToken && p.UserID == "" {
		return p.ID
	}
	return ""
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var _ authz.Store = (*AuthzStore)(nil)

// Grants records who may do what with an app.
type Grants struct{ db *DB }

func NewGrants(db *DB) *Grants { return &Grants{db: db} }

// Grant is a stored grant.
type GrantRow struct {
	ID            string `json:"id"`
	AppID         string `json:"app_id"`
	Plane         string `json:"plane"`
	PrincipalKind string `json:"principal_kind"`
	PrincipalID   string `json:"principal_id,omitempty"`
	RoleID        string `json:"role_id,omitempty"`

	// PrincipalName is who the principal is, for a screen a person reads.
	//
	// Carried here rather than joined by the caller because the caller cannot.
	// This list is read with app.view, and resolving a name from /users needs
	// install.view — so an app's owner, who is the person this screen exists
	// for, would be refused the second request and left looking at identifiers.
	// The API is the product (R-261); a surface that cannot render a name
	// without a permission it has no business holding is an API gap.
	//
	// Empty for an anonymous grant, which is not a person and is rendered as a
	// sentence rather than a name (R-077).
	PrincipalName string `json:"principal_name,omitempty"`

	// RoleName is what the role is called, for the same reason as above:
	// resolving one from /roles needs install.view, which the app owner reading
	// this screen does not hold. Empty on a data-plane grant, which carries no
	// role at all (R-070).
	RoleName string `json:"role_name,omitempty"`

	// Passcode is whether the anonymous grant asks for one (R-075a). Never the
	// passcode or its digest: nobody reads a passcode back, they set a new one.
	Passcode bool `json:"passcode,omitempty"`
}

// Create adds a grant.
//
// The shapes are enforced by the database — a data-plane grant carries no role
// (R-070), only an anonymous grant has a null principal (R-074), and the
// anonymous grant cannot be inserted twice. This method translates those
// refusals into something a person can act on rather than surfacing a
// constraint name.
func (g *Grants) Create(ctx context.Context, appID, plane, kind, principalID, roleID, createdBy string) (GrantRow, error) {
	switch plane {
	case "control", "data":
	default:
		return GrantRow{}, errs.New(errs.ValidInvalid, "A grant is either for managing an app or for using it.")
	}

	if plane == "control" && roleID == "" {
		roleID = authz.RoleViewer
	}
	if plane == "data" {
		roleID = ""
	}
	if kind == "anonymous" {
		principalID = ""
	} else if principalID == "" {
		return GrantRow{}, errs.New(errs.ValidInvalid, "This grant needs someone to grant it to.")
	}

	row := GrantRow{
		ID: id.New(id.Grant), AppID: appID, Plane: plane,
		PrincipalKind: kind, PrincipalID: principalID, RoleID: roleID,
	}
	_, err := g.db.Exec(ctx, `
		INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		row.ID, appID, plane, kind, nullable(principalID), nullable(roleID), createdBy)
	if err != nil {
		if isUniqueViolation(err) {
			return GrantRow{}, errs.New(errs.ValidInvalid, "This app is already shared that way.")
		}
		return GrantRow{}, errs.Wrap(errs.Internal, "Could not share the app.", err)
	}
	return row, nil
}

// GrantInstall gives a principal an installation-wide control-plane grant (O-17).
//
// Install grants are deliberately not reachable through Create: an app-scoped
// grant and an install-scoped one are different powers, and a caller that can
// pass an empty app ID into the sharing path would be able to make an
// administrator out of a sharing request. They are separate methods so the
// callers are separate too — this one is reached from bootstrap, and will be
// reached by the accounts screen's promote/demote route when that exists; never
// from `POST /apps/{id}/grants`.
//
// There is no anonymous install grant, and the schema is what says so: an
// anonymous grant has a NULL principal (R-074) and this refuses an empty one.
//
// A principal already holding an install grant has its role replaced rather
// than a second row added — the schema allows exactly one control grant per
// principal install-wide, so "grant administrator to someone who is already a
// viewer" has only one sensible meaning. That also makes it idempotent, which
// is what the bootstrap path needs: it runs on every start.
func (g *Grants) GrantInstall(ctx context.Context, kind, principalID, roleID, createdBy string) (GrantRow, error) {
	if principalID == "" {
		return GrantRow{}, errs.New(errs.ValidInvalid, "This grant needs someone to grant it to.")
	}
	if roleID == "" {
		return GrantRow{}, errs.New(errs.ValidInvalid, "An installation-wide grant needs a role.")
	}

	row := GrantRow{
		ID: id.New(id.Grant), Plane: "control",
		PrincipalKind: kind, PrincipalID: principalID, RoleID: roleID,
	}

	// ON CONFLICT infers grants_unique_principal, whose NULLS NOT DISTINCT is
	// doing the work: with app_id null it reads "one control grant per
	// principal, install-wide".
	err := g.db.QueryRow(ctx, `
		INSERT INTO grants (id, app_id, plane, role_scope, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, NULL, 'control', 'install', $2, $3, $4, $5)
		ON CONFLICT (app_id, plane, principal_kind, principal_id) DO UPDATE
		  SET role_id = excluded.role_id, role_scope = 'install'
		RETURNING id, coalesce(role_id, '')`,
		row.ID, kind, principalID, roleID, createdBy).Scan(&row.ID, &row.RoleID)
	if err != nil {
		// The composite foreign key refuses an app-scoped role here. That is a
		// caller mistake rather than a database failure, and it deserves a
		// sentence saying which of the two scopes the role belongs to.
		if isForeignKeyViolation(err) {
			return GrantRow{}, errs.New(errs.ValidInvalid,
				"That role applies to a single app, so it cannot be granted across the installation.").
				WithRemedy("Grant it on an app instead, or choose an installation role.")
		}
		return GrantRow{}, errs.Wrap(errs.Internal, "Could not grant installation-wide access.", err)
	}
	return row, nil
}

// RevokeInstall removes a principal's installation-wide grant.
//
// Refuses to remove the last account that can manage accounts (R-088). Without that
// check the install becomes unadministrable in one click and the only way back
// is a psql prompt — which is the same lockout O-17 allowed by accident, now
// reachable on purpose. The check and the delete share a transaction, so two
// administrators removing each other at the same moment cannot both succeed.
//
// The rule is stated in terms of `install.users.manage` rather than "the
// administrator role", because a custom role (R-082) holding that verb is just
// as much an administrator and a rule naming the built-in role would not see it.
func (g *Grants) RevokeInstall(ctx context.Context, kind, principalID string) error {
	tx, err := g.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not remove installation-wide access.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		DELETE FROM grants
		WHERE app_id IS NULL AND plane = 'control'
		  AND principal_kind = $1 AND principal_id = $2`, kind, principalID); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove installation-wide access.", err)
	}

	var remaining int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM grants g
		JOIN roles r ON r.id = g.role_id
		WHERE g.app_id IS NULL
		  AND g.plane = 'control'
		  AND $1 = ANY (r.verbs)`, string(authz.InstallUsersManage)).Scan(&remaining); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove installation-wide access.", err)
	}
	if remaining == 0 {
		return errs.New(errs.ValidInvalid,
			"This is the only account that can manage accounts, so Pando cannot remove its access.").
			WithRemedy("Make someone else an administrator first, then remove this one.")
	}

	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove installation-wide access.", err)
	}
	return nil
}

// InstallRolesByPrincipal maps each principal holding an install grant to its
// role ID, for the accounts screen.
//
// One query rather than one per account: the list is small (R-015 — one
// install, one organization) but a per-row lookup is the shape that stops being
// small quietly.
func (g *Grants) InstallRolesByPrincipal(ctx context.Context) (map[string]string, error) {
	rows, err := g.db.Query(ctx, `
		SELECT principal_id, role_id
		FROM grants
		WHERE app_id IS NULL AND plane = 'control' AND principal_id IS NOT NULL`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read installation-wide access.", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var principal, role string
		if err := rows.Scan(&principal, &role); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read installation-wide access.", err)
		}
		out[principal] = role
	}
	return out, rows.Err()
}

// InstallRoles returns the roles that may be granted installation-wide (R-082).
//
// Read from the roles table rather than hardcoded, so a custom install-scoped
// role appears in the console the moment it exists without a second change here.
func (g *Grants) InstallRoles(ctx context.Context) ([]authz.Role, error) {
	rows, err := g.db.Query(ctx,
		`SELECT id, name, builtin, verbs FROM roles WHERE scope = 'install' ORDER BY name`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the roles.", err)
	}
	defer rows.Close()

	out := make([]authz.Role, 0)
	for rows.Next() {
		var r authz.Role
		var verbs []string
		if err := rows.Scan(&r.ID, &r.Name, &r.Builtin, &verbs); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the roles.", err)
		}
		for _, v := range verbs {
			r.Verbs = append(r.Verbs, authz.Verb(v))
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListForApp returns one page of an app's grants, with the cursor for the
// next page (empty after the last).
//
// Keyset-paged by principal (grants_app_list_idx), so one principal's grants
// on both planes arrive together, and the grant to everyone — principal kind
// "anonymous", which sorts first — is on the first page. An app shared with
// every person by name is as many grants as people (issue #72).
func (g *Grants) ListForApp(ctx context.Context, appID string, page Page) ([]GrantRow, string, error) {
	var afterKind, afterPrincipal, afterPlane, afterID string
	have, err := decodeCursor(page.Cursor, &afterKind, &afterPrincipal, &afterPlane, &afterID)
	if err != nil {
		return nil, "", err
	}
	after := ""
	args := []any{appID, page.Size() + 1}
	if have {
		args = append(args, afterKind, afterPrincipal, afterPlane, afterID)
		after = `
		  AND (g.principal_kind, coalesce(g.principal_id, ''), g.plane, g.id) > ($3, $4, $5, $6)`
	}
	// The name comes from the same query, for both principal kinds a grant can
	// name. A user's display name falls back to the username it signs in with,
	// because a local account created without one would otherwise render blank
	// — worse than an identifier, since there is nothing to recognize at all.
	rows, err := g.db.Query(ctx, `
		SELECT g.id, g.app_id, g.plane, g.principal_kind,
		       coalesce(g.principal_id, ''), coalesce(g.role_id, ''),
		       coalesce(nullif(u.display_name, ''), u.external_id, gr.name, ''),
		       coalesce(r.name, ''), g.passcode_hash IS NOT NULL
		FROM grants g
		LEFT JOIN users u
		       ON g.principal_kind = 'user' AND u.id = g.principal_id
		LEFT JOIN groups gr
		       ON g.principal_kind = 'group' AND gr.id = g.principal_id
		LEFT JOIN roles r ON r.id = g.role_id
		WHERE g.app_id = $1`+after+`
		ORDER BY g.principal_kind, coalesce(g.principal_id, ''), g.plane, g.id
		LIMIT $2`, args...)
	if err != nil {
		return nil, "", errs.Wrap(errs.Internal, "Could not read who this app is shared with.", err)
	}
	defer rows.Close()

	out := make([]GrantRow, 0)
	for rows.Next() {
		var row GrantRow
		if err := rows.Scan(&row.ID, &row.AppID, &row.Plane, &row.PrincipalKind,
			&row.PrincipalID, &row.RoleID, &row.PrincipalName, &row.RoleName, &row.Passcode); err != nil {
			return nil, "", errs.Wrap(errs.Internal, "Could not read who this app is shared with.", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", errs.Wrap(errs.Internal, "Could not read who this app is shared with.", err)
	}
	var next string
	if len(out) > page.Size() {
		out = out[:page.Size()]
		last := out[len(out)-1]
		next = encodeCursor(last.PrincipalKind, last.PrincipalID, last.Plane, last.ID)
	}
	return out, next, nil
}

// Delete revokes a grant.
// SetRole changes the role a control-plane grant carries, in one statement —
// not a revoke and a new grant, which would leave the person with nothing if
// the second half failed. A data-plane grant carries no role (R-070) and is
// refused, and so is an installation role: the composite foreign key on
// (role_id, role_scope) refuses it, and that becomes a sentence here.
func (g *Grants) SetRole(ctx context.Context, appID, grantID, roleID string) error {
	if roleID == "" {
		return errs.New(errs.ValidInvalid, "A role is needed to change what this grant allows.")
	}
	tag, err := g.db.Exec(ctx, `
		UPDATE grants SET role_id = $3
		WHERE id = $1 AND app_id = $2 AND plane = 'control'`, grantID, appID, roleID)
	if err != nil {
		if isForeignKeyViolation(err) {
			return errs.New(errs.ValidInvalid,
				"That is not a role that can be granted on an app.").
				WithRemedy("Choose Viewer, Operator, Owner, or a custom app role.")
		}
		return errs.Wrap(errs.Internal, "Could not change the role.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "There is no grant for managing this app with that ID.")
	}
	return nil
}

// UserAppGrant is one grant that gives a person something on an app, directly
// or through a group they are in.
type UserAppGrant struct {
	AppID     string `json:"app_id"`
	AppName   string `json:"app_name"`
	AppOwner  string `json:"-"`
	GrantID   string `json:"grant_id"`
	Plane     string `json:"plane"`
	RoleID    string `json:"role_id,omitempty"`
	RoleName  string `json:"role_name,omitempty"`
	Via       string `json:"via"` // user or group
	GroupID   string `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`
}

// ForGroup returns a group's own app grants, what everyone in it gets, for
// one page of apps by name (the cursor is the last app's), with the cursor
// for the next page.
func (g *Grants) ForGroup(ctx context.Context, groupID string, page Page) ([]UserAppGrant, string, error) {
	return g.appGrants(ctx, page, `
		    SELECT app_id FROM grants WHERE principal_kind = 'group' AND principal_id = $1 AND app_id IS NOT NULL`,
		`g.principal_kind = 'group' AND g.principal_id = $1`, groupID)
}

// ForUser returns every app grant that reaches a person — their own, and
// their groups' (resolved live, R-079) — for one page of apps by name, with
// the cursor for the next page. Install grants are not app grants and are not
// here.
func (g *Grants) ForUser(ctx context.Context, userID string, page Page) ([]UserAppGrant, string, error) {
	return g.appGrants(ctx, page, `
		    SELECT app_id FROM grants
		    WHERE principal_kind = 'user' AND principal_id = $1 AND app_id IS NOT NULL
		    UNION
		    SELECT g.app_id FROM grants g
		    JOIN effective_group_members m ON m.group_id = g.principal_id
		    WHERE g.principal_kind = 'group' AND m.user_id = $1 AND g.app_id IS NOT NULL`,
		`((g.principal_kind = 'user' AND g.principal_id = $1)
		     OR (g.principal_kind = 'group' AND g.principal_id IN (
		            SELECT group_id FROM effective_group_members WHERE user_id = $1)))`, userID)
}

// appGrants is one page of apps — those `reach` ($1 the principal) selects,
// live, by name — and every grant `which` matches on each, in one query: the
// apps are chosen first and the grants read for those apps alone, so a page
// costs a page of apps however many the principal reaches (issue #72).
func (g *Grants) appGrants(ctx context.Context, page Page, reach, which, principal string) ([]UserAppGrant, string, error) {
	var afterName, afterID string
	have, err := decodeCursor(page.Cursor, &afterName, &afterID)
	if err != nil {
		return nil, "", err
	}
	args := []any{principal, page.Size() + 1}
	after := ""
	if have {
		args = append(args, afterName, afterID)
		after = ` AND (a.name, a.id) > ($3, $4)`
	}
	rows, err := g.db.Query(ctx, `
		WITH reach AS (`+reach+`
		), page AS (
		    SELECT a.id, a.name, coalesce(a.owner_user_id, '') AS owner
		    FROM reach JOIN apps a ON a.id = reach.app_id
		    WHERE a.deleted_at IS NULL`+after+`
		    ORDER BY a.name, a.id
		    LIMIT $2
		)
		SELECT p.id, p.name, p.owner, g.id, g.plane,
		       coalesce(g.role_id, ''), coalesce(r.name, ''), g.principal_kind,
		       coalesce(gr.id, ''), coalesce(gr.name, '')
		FROM page p
		JOIN grants g ON g.app_id = p.id
		LEFT JOIN roles r ON r.id = g.role_id
		LEFT JOIN groups gr ON g.principal_kind = 'group' AND gr.id = g.principal_id
		WHERE `+which+`
		ORDER BY p.name, p.id, g.plane, g.principal_kind DESC, g.id`, args...)
	if err != nil {
		return nil, "", errs.Wrap(errs.Internal, "Could not read the apps.", err)
	}
	defer rows.Close()

	out := []UserAppGrant{}
	apps := 0
	for rows.Next() {
		var u UserAppGrant
		if err := rows.Scan(&u.AppID, &u.AppName, &u.AppOwner, &u.GrantID, &u.Plane,
			&u.RoleID, &u.RoleName, &u.Via, &u.GroupID, &u.GroupName); err != nil {
			return nil, "", errs.Wrap(errs.Internal, "Could not read the apps.", err)
		}
		if len(out) == 0 || out[len(out)-1].AppID != u.AppID {
			apps++
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, "", errs.Wrap(errs.Internal, "Could not read the apps.", err)
	}
	// One app more than the page was read, to learn whether there is a next.
	var next string
	if apps > page.Size() {
		extra := out[len(out)-1].AppID
		for len(out) > 0 && out[len(out)-1].AppID == extra {
			out = out[:len(out)-1]
		}
		last := out[len(out)-1]
		next = encodeCursor(last.AppName, last.AppID)
	}
	return out, next, nil
}

func (g *Grants) Delete(ctx context.Context, appID, grantID string) error {
	_, err := g.db.Exec(ctx, `DELETE FROM grants WHERE id = $1 AND app_id = $2`, grantID, appID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not stop sharing the app.", err)
	}
	return nil
}
