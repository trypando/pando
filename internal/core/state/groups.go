package state

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Groups are collections of users (R-078).
//
// Pando-native here. An IdP may also push groups (R-048), and when it does the
// membership arrives the same way — which is why membership is a table rather
// than a field, and why authorization reads it live (R-079) instead of
// denormalizing it into grants.
type Groups struct{ db *DB }

func NewGroups(db *DB) *Groups { return &Groups{db: db} }

// Group is a stored group.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Source is the identity provider that owns the group's membership, and
	// SourceName its name; both empty for a group made in Pando. A synced
	// group's members change at the provider, never here (R-079).
	Source     string `json:"source,omitempty"`
	SourceName string `json:"source_name,omitempty"`

	Members []string `json:"members,omitempty"`

	// MemberCount is how many people are in the group directly. Set by
	// ListPage, which leaves Members out: a page of groups carrying every
	// member of each is a page that grows with the organization (issue #72).
	// GET /groups/{id} has the members.
	MemberCount *int `json:"member_count,omitempty"`

	// LinkedFrom, on a Pando-made group, are the provider groups whose members
	// count as its members (group_links). LinksTo, on a synced group, are the
	// Pando groups it feeds.
	LinkedFrom []string `json:"linked_from,omitempty"`
	LinksTo    []string `json:"links_to,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

const groupSelect = `
		SELECT g.id, g.name, coalesce(g.adapter_id, ''), coalesce(a.name, ''), g.created_at,
		       coalesce(array_agg(DISTINCT m.user_id) FILTER (WHERE m.user_id IS NOT NULL), '{}'),
		       coalesce((SELECT array_agg(l.synced_group_id ORDER BY l.synced_group_id) FROM group_links l WHERE l.group_id = g.id), '{}'),
		       coalesce((SELECT array_agg(l.group_id ORDER BY l.group_id) FROM group_links l WHERE l.synced_group_id = g.id), '{}')
		FROM groups g
		LEFT JOIN identity_adapters a ON a.id = g.adapter_id
		LEFT JOIN group_members m ON m.group_id = g.id`

func scanGroup(row pgx.Row) (Group, error) {
	var group Group
	err := row.Scan(&group.ID, &group.Name, &group.Source, &group.SourceName, &group.CreatedAt,
		&group.Members, &group.LinkedFrom, &group.LinksTo)
	return group, err
}

// Create adds a Pando-native group.
func (g *Groups) Create(ctx context.Context, name string) (Group, error) {
	if name == "" {
		return Group{}, errs.New(errs.ValidInvalid, "A group needs a name.")
	}

	group := Group{ID: id.New(id.Group), Name: name}
	err := g.db.QueryRow(ctx,
		`INSERT INTO groups (id, name) VALUES ($1, $2) RETURNING created_at`,
		group.ID, name).Scan(&group.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Group{}, errs.Newf(errs.ValidInvalid, "There is already a group called %q.", name)
		}
		return Group{}, errs.Wrap(errs.Internal, "Could not create the group.", err)
	}
	return group, nil
}

// List returns every group with its members.
func (g *Groups) List(ctx context.Context) ([]Group, error) {
	rows, err := g.db.Query(ctx, groupSelect+`
		GROUP BY g.id, g.name, g.adapter_id, a.name, g.created_at
		ORDER BY g.adapter_id IS NOT NULL, lower(g.name)`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the groups.", err)
	}
	defer rows.Close()

	out := make([]Group, 0)
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the groups.", err)
		}
		out = append(out, group)
	}
	return out, rows.Err()
}

// GroupFilter narrows ListPage.
type GroupFilter struct {
	// Member keeps only the groups this user is directly in.
	Member string
}

// ListPage is one page of groups — Pando's own first, then providers', each
// by name — with the cursor for the next page and how many match in all.
// Each carries MemberCount rather than Members, and its links.
//
// Page.Query matches the name.
func (g *Groups) ListPage(ctx context.Context, page Page, f GroupFilter) ([]Group, string, int, error) {
	var (
		synced bool
		name   string
		after  string
	)
	have, err := decodeCursor(page.Cursor, &synced, &name, &after)
	if err != nil {
		return nil, "", 0, err
	}
	const where = `
		WHERE ($1 = '' OR g.name ILIKE '%' || $1 || '%')
		  AND ($2 = '' OR EXISTS (SELECT 1 FROM group_members gm WHERE gm.group_id = g.id AND gm.user_id = $2))`
	q := likeEscape(page.Query)

	rows, err := g.db.Query(ctx, `
		SELECT g.id, g.name, coalesce(g.adapter_id, ''), coalesce(a.name, ''), g.created_at,
		       (SELECT count(*) FROM group_members m WHERE m.group_id = g.id),
		       coalesce((SELECT array_agg(l.synced_group_id ORDER BY l.synced_group_id) FROM group_links l WHERE l.group_id = g.id), '{}'),
		       coalesce((SELECT array_agg(l.group_id ORDER BY l.group_id) FROM group_links l WHERE l.synced_group_id = g.id), '{}'),
		       g.adapter_id IS NOT NULL, lower(g.name)
		FROM groups g
		LEFT JOIN identity_adapters a ON a.id = g.adapter_id`+where+`
		  AND (NOT $3 OR (g.adapter_id IS NOT NULL, lower(g.name), g.id) > ($4::boolean, $5::text, $6::text))
		ORDER BY g.adapter_id IS NOT NULL, lower(g.name), g.id
		LIMIT $7`, q, f.Member, have, synced, name, after, page.Size()+1)
	if err != nil {
		return nil, "", 0, errs.Wrap(errs.Internal, "Could not read the groups.", err)
	}
	defer rows.Close()

	type keyed struct {
		Group
		synced bool
		lower  string
	}
	var got []keyed
	for rows.Next() {
		var k keyed
		var count int
		if err := rows.Scan(&k.ID, &k.Name, &k.Source, &k.SourceName, &k.CreatedAt,
			&count, &k.LinkedFrom, &k.LinksTo, &k.synced, &k.lower); err != nil {
			return nil, "", 0, errs.Wrap(errs.Internal, "Could not read the groups.", err)
		}
		k.MemberCount = &count
		got = append(got, k)
	}
	if err := rows.Err(); err != nil {
		return nil, "", 0, errs.Wrap(errs.Internal, "Could not read the groups.", err)
	}

	var next string
	if len(got) > page.Size() {
		got = got[:page.Size()]
		last := got[len(got)-1]
		next = encodeCursor(last.synced, last.lower, last.ID)
	}
	out := make([]Group, 0, len(got))
	for _, k := range got {
		out = append(out, k.Group)
	}
	var total int
	if err := g.db.QueryRow(ctx, `SELECT count(*) FROM groups g`+where, q, f.Member).Scan(&total); err != nil {
		return nil, "", 0, errs.Wrap(errs.Internal, "Could not count the groups.", err)
	}
	return out, next, total, nil
}

// Search finds groups by name, case-insensitively, at most limit of them.
func (g *Groups) Search(ctx context.Context, q string, limit int) ([]Group, error) {
	rows, err := g.db.Query(ctx, `
		SELECT g.id, g.name, coalesce(g.adapter_id, ''), coalesce(a.name, ''), g.created_at
		FROM groups g LEFT JOIN identity_adapters a ON a.id = g.adapter_id
		WHERE $1 = '' OR g.name ILIKE '%' || $1 || '%'
		ORDER BY g.name LIMIT $2`, likeEscape(q), limit)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not search the groups.", err)
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.Name, &group.Source, &group.SourceName, &group.CreatedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not search the groups.", err)
		}
		out = append(out, group)
	}
	return out, rows.Err()
}

// ByID returns one group.
func (g *Groups) ByID(ctx context.Context, groupID string) (Group, bool, error) {
	group, err := scanGroup(g.db.QueryRow(ctx, groupSelect+`
		WHERE g.id = $1
		GROUP BY g.id, g.name, g.adapter_id, a.name, g.created_at`, groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, false, nil
	}
	if err != nil {
		return Group{}, false, errs.Wrap(errs.Internal, "Could not read the group.", err)
	}
	return group, true, nil
}

// SetMembers replaces a group's membership.
//
// Replaces rather than merges, in one transaction. A merge cannot express
// "remove this person", and removing someone from a group is the operation that
// has to work — it is how access is revoked (R-079).
func (g *Groups) SetMembers(ctx context.Context, groupID string, userIDs []string) error {
	if err := g.native(ctx, groupID); err != nil {
		return err
	}
	tx, err := g.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not change the group's members.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := peopleWhoManage(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not change the group's members.", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM group_members WHERE group_id = $1`, groupID); err != nil {
		return errs.Wrap(errs.Internal, "Could not change the group's members.", err)
	}
	for _, userID := range userIDs {
		if userID == "" {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)
			 ON CONFLICT DO NOTHING`, groupID, userID); err != nil {
			return errs.Wrap(errs.Internal, "Could not change the group's members.", err)
		}
	}
	if err := refuseLockout(ctx, tx, before); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not change the group's members.", err)
	}
	return nil
}

// AddMember puts one person in a group. Adding someone already in it is not
// an error: the outcome holds. Refused for a group synced from an identity
// provider, whose membership belongs to the provider (R-079).
func (g *Groups) AddMember(ctx context.Context, groupID, userID string) error {
	if err := g.native(ctx, groupID); err != nil {
		return err
	}
	_, err := g.db.Exec(ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		groupID, userID)
	if err != nil {
		if isForeignKeyViolation(err) {
			return errs.New(errs.NotFound, "There is no account with that ID.")
		}
		return errs.Wrap(errs.Internal, "Could not add the account to the group.", err)
	}
	return nil
}

// RemoveMember takes one person out of a group, and with them whatever the
// group gave them. Refused when it would leave nobody who can manage accounts
// (R-088) — the last member of a group holding the administrator role is as
// much the last administrator as a direct grant is.
func (g *Groups) RemoveMember(ctx context.Context, groupID, userID string) error {
	if err := g.native(ctx, groupID); err != nil {
		return err
	}
	tx, err := g.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the account from the group.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := peopleWhoManage(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the account from the group.", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the account from the group.", err)
	}
	if err := refuseLockout(ctx, tx, before); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the account from the group.", err)
	}
	return nil
}

// native refuses a change to a group that does not exist, or whose
// membership an identity provider owns.
func (g *Groups) native(ctx context.Context, groupID string) error {
	var adapter *string
	err := g.db.QueryRow(ctx, `SELECT adapter_id FROM groups WHERE id = $1`, groupID).Scan(&adapter)
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.New(errs.NotFound, "There is no group with that ID.")
	}
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not read the group.", err)
	}
	if adapter != nil {
		return errs.New(errs.ValidInvalid, "This group comes from an identity provider, so its members are changed there.").
			WithRemedy("Change the membership in the identity provider; Pando picks it up at the next sign-in, " +
				"or at once if the provider pushes changes through SCIM. To give its members a Pando group's " +
				"access as well, link it to that group.")
	}
	return nil
}

// peopleWhoManage counts the active accounts that can manage accounts, directly
// or through a group — the R-088 quantity when membership changes. Counting
// grants, as accountManagers does, would count a group holding the
// administrator role as a manager even with nobody left in it.
func peopleWhoManage(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(DISTINCT u.id)
		FROM users u
		JOIN grants g ON g.app_id IS NULL AND g.plane = 'control'
		JOIN roles r ON r.id = g.role_id
		WHERE u.deleted_at IS NULL AND u.status = 'active'
		  AND $1 = ANY (r.verbs)
		  AND (
		        (g.principal_kind = 'user'  AND g.principal_id = u.id)
		     OR (g.principal_kind = 'group' AND g.principal_id IN (
		            SELECT group_id FROM effective_group_members WHERE user_id = u.id))
		  )`, string(authz.InstallUsersManage)).Scan(&n)
	return n, err
}

func refuseLockout(ctx context.Context, tx pgx.Tx, before int) error {
	after, err := peopleWhoManage(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not change the group's members.", err)
	}
	if before > 0 && after == 0 {
		return errs.New(errs.ValidInvalid,
			"This would leave nobody who can manage accounts, so Pando cannot make this change.").
			WithRemedy("Make someone else an administrator first, then change this group.")
	}
	return nil
}

// Delete removes a group. Grants made to it go with it — the people in it lose
// whatever the group gave them, and keep anything given to them directly.
//
// Refused when it would leave nobody who can manage accounts (R-088): a group
// can hold an administrator role like anyone else, and deleting it is the same
// lockout as revoking that grant directly.
func (g *Groups) Delete(ctx context.Context, groupID string) error {
	// Grants to the group are removed first and explicitly. The schema does not
	// cascade from groups to grants — grants reference a principal_id that is
	// not a foreign key, because a principal may be a user, a group or a token.
	// So this is the one place that link is maintained, and forgetting it would
	// leave a grant naming a group that no longer exists: invisible, and
	// matching nobody.
	tx, err := g.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the group.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := accountManagers(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the group.", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM grants WHERE principal_kind = 'group' AND principal_id = $1`, groupID); err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the group.", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the group.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "There is no group with that ID.")
	}
	after, err := accountManagers(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the group.", err)
	}
	if before > 0 && after == 0 {
		return errs.New(errs.ValidInvalid,
			"This group is how the installation's only administrators manage accounts, so Pando cannot delete it.").
			WithRemedy("Give someone an administrator role directly, or through another group, then delete this one.")
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the group.", err)
	}
	return nil
}

// Roles reads and writes custom roles (R-082).
type Roles struct{ db *DB }

func NewRoles(db *DB) *Roles { return &Roles{db: db} }

// CreateCustom adds a custom role composed from the verb catalog.
//
// Custom roles are arbitrary subsets of the catalog, and validated against it:
// a role naming a verb Pando does not have would grant nothing and look like it
// granted something.
func (r *Roles) CreateCustom(ctx context.Context, name, scope string, verbs []authz.Verb) (authz.Role, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return authz.Role{}, errs.New(errs.ValidInvalid, "A role needs a name.")
	}

	// Names are unique ignoring case and spaces, built-ins included: the
	// built-ins are stored lowercase and shown capitalized, so "Administrator"
	// would otherwise sit in every picker beside the real one. The index
	// (roles_name_folded_unique) is what guarantees it; this is what says
	// which role is in the way.
	var taken string
	var builtin bool
	err := r.db.QueryRow(ctx,
		`SELECT name, builtin FROM roles WHERE lower(btrim(name)) = lower($1)`, name).Scan(&taken, &builtin)
	switch {
	case err == nil && builtin:
		return authz.Role{}, errs.Newf(errs.ValidInvalid, "Pando already has a built-in role called %q.", taken).
			WithRemedy("Choose a different name.")
	case err == nil:
		return authz.Role{}, errs.Newf(errs.ValidInvalid, "There is already a role called %q.", taken).
			WithRemedy("Choose a different name.")
	case !errors.Is(err, pgx.ErrNoRows):
		return authz.Role{}, errs.Wrap(errs.Internal, "Could not create the role.", err)
	}
	if len(verbs) == 0 {
		return authz.Role{}, errs.New(errs.ValidInvalid, "A role needs at least one permission.").
			WithRemedy("Choose from the list at GET /verbs.")
	}

	switch scope {
	case "app", "install":
	default:
		return authz.Role{}, errs.New(errs.ValidInvalid,
			"A role applies either to one app or across the installation.")
	}

	names := make([]string, 0, len(verbs))
	for _, v := range verbs {
		if !authz.IsVerb(v) {
			return authz.Role{}, errs.Newf(errs.ValidInvalid,
				"%q is not a permission Pando has.", v).
				WithRemedy("Choose from the list at GET /verbs.")
		}
		// A role must not mix scopes. The two are checked by different
		// functions against different grants, so a role holding both would be
		// half-usable wherever it was granted (design 06 §2.1).
		if authz.InstallScoped(v) != (scope == "install") {
			return authz.Role{}, errs.Newf(errs.ValidInvalid,
				"%q does not apply %s, so it cannot be part of a role that does.", v, scopeWord(scope)).
				WithRemedy("Make two roles, or choose the other scope.")
		}
		names = append(names, string(v))
	}

	role := authz.Role{ID: id.New(id.Role), Name: name, Builtin: false, Verbs: verbs}
	_, err = r.db.Exec(ctx,
		`INSERT INTO roles (id, name, builtin, scope, verbs) VALUES ($1, $2, false, $3, $4)`,
		role.ID, name, scope, names)
	if err != nil {
		if isUniqueViolation(err) {
			return authz.Role{}, errs.Newf(errs.ValidInvalid, "There is already a role called %q.", name)
		}
		return authz.Role{}, errs.Wrap(errs.Internal, "Could not create the role.", err)
	}
	return role, nil
}

func scopeWord(scope string) string {
	if scope == "install" {
		return "across the installation"
	}
	return "to a single app"
}

// List returns every role, of either scope: installation roles first, Pando's
// before the installation's own, and within those the broadest first — so the
// built-ins read administrator, creator, then owner, operator, viewer.
func (r *Roles) List(ctx context.Context) ([]RoleRow, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, name, builtin, scope, verbs FROM roles
		ORDER BY scope DESC, builtin DESC, cardinality(verbs) DESC, name`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the roles.", err)
	}
	defer rows.Close()

	out := make([]RoleRow, 0)
	for rows.Next() {
		var row RoleRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Builtin, &row.Scope, &row.Verbs); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the roles.", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// RoleRow is a role as stored.
type RoleRow struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Builtin bool     `json:"builtin"`
	Scope   string   `json:"scope"`
	Verbs   []string `json:"verbs"`
}

// DeleteCustom removes a custom role, and every grant of it with it: anyone
// who held it — on an app, or across the installation — loses what it allowed,
// and keeps anything they hold another way. Built-ins are protected by trigger
// (R-081), so this reports that rather than surfacing a constraint name.
//
// The grants have to go first: they reference the role by foreign key, so a
// role anyone held could not be deleted at all, and the error said only that
// something had gone wrong.
//
// Refused when it would leave nobody who can manage accounts (R-088) — a
// custom role holding install.users.manage is as much an administrator as the
// built-in one.
func (r *Roles) DeleteCustom(ctx context.Context, roleID string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the role.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var builtin bool
	err = tx.QueryRow(ctx, `SELECT builtin FROM roles WHERE id = $1`, roleID).Scan(&builtin)
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.New(errs.NotFound, "There is no role with that ID.")
	}
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not read the role.", err)
	}
	if builtin {
		return errs.New(errs.ValidInvalid, "Pando's built-in roles cannot be deleted.").
			WithRemedy("Make a custom role instead, and grant that.")
	}

	before, err := accountManagers(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the role.", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM grants WHERE role_id = $1`, roleID); err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the role.", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM roles WHERE id = $1`, roleID); err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the role.", err)
	}
	after, err := accountManagers(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the role.", err)
	}
	if before > 0 && after == 0 {
		return errs.New(errs.ValidInvalid,
			"This role is how the installation's only administrators manage accounts, so Pando cannot delete it.").
			WithRemedy("Give someone the built-in administrator role first, then delete this one.")
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the role.", err)
	}
	return nil
}

// accountManagers counts the installation-wide grants that can manage
// accounts — the R-088 quantity. Counted inside the caller's transaction so a
// check and the delete it guards cannot be interleaved with another.
func accountManagers(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM grants g
		JOIN roles r ON r.id = g.role_id
		WHERE g.app_id IS NULL
		  AND g.plane = 'control'
		  AND $1 = ANY (r.verbs)`, string(authz.InstallUsersManage)).Scan(&n)
	return n, err
}
