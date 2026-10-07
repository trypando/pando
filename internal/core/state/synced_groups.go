package state

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Groups an identity provider owns (R-078, R-048).
//
// A provider says who is in its groups — through the claims of each sign-in,
// or through SCIM — and each group it names is a group here, keyed by where it
// came from. Pando says what each can do: an administrator gives it roles and
// app grants like any other group, or links it to a Pando-made group
// (group_links), and membership is read live either way (R-079).

// SyncMemberships sets which of a provider's groups a person is in, from the
// group names a sign-in carried: groups not seen before are made, and the
// person leaves any of the provider's groups the sign-in did not name.
// Groups made in Pando, and other providers' groups, are untouched.
//
// Refused, and nothing changed, when it would leave nobody who can manage
// accounts (R-088) — the same rule as removing someone from a group by hand.
func (g *Groups) SyncMemberships(ctx context.Context, adapterID, userID string, names []string) error {
	tx, err := g.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the account's groups.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := peopleWhoManage(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the account's groups.", err)
	}

	keep := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		groupID, err := ensureSynced(ctx, tx, adapterID, name, name)
		if err != nil {
			return err
		}
		keep = append(keep, groupID)
		if _, err := tx.Exec(ctx,
			`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			groupID, userID); err != nil {
			return errs.Wrap(errs.Internal, "Could not update the account's groups.", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM group_members m USING groups g
		WHERE m.group_id = g.id AND g.adapter_id = $1 AND m.user_id = $2
		  AND NOT (m.group_id = ANY ($3))`, adapterID, userID, keep); err != nil {
		return errs.Wrap(errs.Internal, "Could not update the account's groups.", err)
	}
	if err := refuseLockout(ctx, tx, before); err != nil {
		return err
	}
	return commit(ctx, tx, "Could not update the account's groups.")
}

// ensureSynced returns the ID of a provider's group, making it if needed.
func ensureSynced(ctx context.Context, tx pgx.Tx, adapterID, externalID, name string) (string, error) {
	var groupID string
	err := tx.QueryRow(ctx, `
		INSERT INTO groups (id, adapter_id, external_id, name) VALUES ($1, $2, $3, $4)
		ON CONFLICT (adapter_id, external_id) WHERE adapter_id IS NOT NULL
		DO UPDATE SET name = groups.name
		RETURNING id`, id.New(id.Group), adapterID, externalID, name).Scan(&groupID)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not record the provider's group.", err)
	}
	return groupID, nil
}

// Link makes everyone in a provider's group count as a member of a Pando-made
// group, live (R-079).
func (g *Groups) Link(ctx context.Context, syncedGroupID, groupID, by string) error {
	// Checked first so a missing group is called missing, rather than
	// reported by the direction trigger as a link that runs the wrong way.
	var n int
	if err := g.db.QueryRow(ctx, `SELECT count(*) FROM groups WHERE id = ANY ($1)`,
		[]string{syncedGroupID, groupID}).Scan(&n); err != nil {
		return errs.Wrap(errs.Internal, "Could not link the groups.", err)
	}
	if n < 2 && syncedGroupID != groupID {
		return errs.New(errs.NotFound, "There is no group with that ID.")
	}
	_, err := g.db.Exec(ctx, `
		INSERT INTO group_links (synced_group_id, group_id, created_by) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, syncedGroupID, groupID, by)
	if err != nil {
		var pg interface{ SQLState() string }
		if errors.As(err, &pg) && pg.SQLState() == "23514" {
			return errs.New(errs.ValidInvalid,
				"A link runs from a group an identity provider syncs to a group made in Pando.").
				WithRemedy("Choose a synced group to link, and a Pando group to link it to.")
		}
		if isForeignKeyViolation(err) {
			return errs.New(errs.NotFound, "There is no group with that ID.")
		}
		return errs.Wrap(errs.Internal, "Could not link the groups.", err)
	}
	return nil
}

// Unlink removes a link. Refused when it would leave nobody who can manage
// accounts (R-088).
func (g *Groups) Unlink(ctx context.Context, syncedGroupID, groupID string) error {
	tx, err := g.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not unlink the groups.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, err := peopleWhoManage(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not unlink the groups.", err)
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM group_links WHERE synced_group_id = $1 AND group_id = $2`, syncedGroupID, groupID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not unlink the groups.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "Those groups are not linked.")
	}
	if err := refuseLockout(ctx, tx, before); err != nil {
		return err
	}
	return commit(ctx, tx, "Could not unlink the groups.")
}

// SCIMGroup is a provider's group as a SCIM client sees it.
type SCIMGroup struct {
	ID          string
	DisplayName string
	ExternalID  string
	Resource    []byte
	Members     []SCIMMember
	CreatedAt   string
	UpdatedAt   string
}

// SCIMMember is one member of a SCIM group.
type SCIMMember struct {
	UserID  string
	Display string
}

// SCIMGroups is the SCIM view of one provider's groups.
type SCIMGroups struct{ db *DB }

func NewSCIMGroups(db *DB) *SCIMGroups { return &SCIMGroups{db: db} }

// List returns a provider's groups, optionally filtered by display name or
// external ID, with the total before paging.
func (s *SCIMGroups) List(ctx context.Context, adapterID, attr, value string, offset, limit int, members bool) ([]SCIMGroup, int, error) {
	where := `adapter_id = $1`
	args := []any{adapterID}
	switch attr {
	case "":
	case "displayName":
		where += ` AND lower(name) = lower($2)`
		args = append(args, value)
	case "externalId":
		where += ` AND scim_resource->>'externalId' = $2`
		args = append(args, value)
	case "id":
		where += ` AND id = $2`
		args = append(args, value)
	default:
		return nil, 0, errs.Newf(errs.ValidInvalid, "Pando cannot filter groups by %q.", attr)
	}
	countArgs := args
	args = append(append([]any{}, args...), limit, offset)
	n := len(args)
	rows, err := s.db.Query(ctx, `
		SELECT id FROM groups WHERE `+where+`
		ORDER BY created_at, id LIMIT $`+itoa(n-1)+` OFFSET $`+itoa(n), args...)
	if err != nil {
		return nil, 0, errs.Wrap(errs.Internal, "Could not read the groups.", err)
	}
	var ids []string
	for rows.Next() {
		var groupID string
		if err := rows.Scan(&groupID); err != nil {
			rows.Close()
			return nil, 0, errs.Wrap(errs.Internal, "Could not read the groups.", err)
		}
		ids = append(ids, groupID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, errs.Wrap(errs.Internal, "Could not read the groups.", err)
	}
	// totalResults is required (RFC 7644 §3.4.2); counted only when the page
	// does not already show it, as for users. Index-served
	// (groups_scim_list_idx, migration 000049).
	total, ok := totalFromPage(offset, limit, len(ids))
	if !ok {
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM groups WHERE `+where, countArgs...).Scan(&total); err != nil {
			return nil, 0, errs.Wrap(errs.Internal, "Could not read the groups.", err)
		}
	}
	out := make([]SCIMGroup, 0, len(ids))
	for _, groupID := range ids {
		grp, _, err := s.ByID(ctx, adapterID, groupID, members)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, grp)
	}
	return out, total, nil
}

// ByID returns one of a provider's groups.
func (s *SCIMGroups) ByID(ctx context.Context, adapterID, groupID string, members bool) (SCIMGroup, bool, error) {
	var grp SCIMGroup
	err := s.db.QueryRow(ctx, `
		SELECT id, name, coalesce(scim_resource->>'externalId', ''), coalesce(scim_resource, '{}'::jsonb)::text,
		       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM groups WHERE id = $1 AND adapter_id = $2`, groupID, adapterID).
		Scan(&grp.ID, &grp.DisplayName, &grp.ExternalID, &grp.Resource, &grp.CreatedAt, &grp.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SCIMGroup{}, false, nil
	}
	if err != nil {
		return SCIMGroup{}, false, errs.Wrap(errs.Internal, "Could not read the group.", err)
	}
	if members {
		rows, err := s.db.Query(ctx, `
			SELECT u.id, coalesce(u.display_name, u.external_id)
			FROM group_members m JOIN users u ON u.id = m.user_id
			WHERE m.group_id = $1 ORDER BY u.id`, groupID)
		if err != nil {
			return SCIMGroup{}, false, errs.Wrap(errs.Internal, "Could not read the group.", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m SCIMMember
			if err := rows.Scan(&m.UserID, &m.Display); err != nil {
				return SCIMGroup{}, false, errs.Wrap(errs.Internal, "Could not read the group.", err)
			}
			grp.Members = append(grp.Members, m)
		}
	}
	return grp, true, nil
}

// Create makes a group for a SCIM client. The external ID it is keyed by is
// the client's externalId when it sends one; otherwise the display name,
// which is also what a sign-in's groups claim would carry.
func (s *SCIMGroups) Create(ctx context.Context, adapterID, displayName, externalID string, resource []byte, members []string) (string, error) {
	key := externalID
	if key == "" {
		key = displayName
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not create the group.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM groups WHERE adapter_id = $1 AND external_id = $2)`,
		adapterID, key).Scan(&exists); err != nil {
		return "", errs.Wrap(errs.Internal, "Could not create the group.", err)
	}
	if exists {
		return "", ErrSCIMConflict
	}
	groupID := id.New(id.Group)
	if _, err := tx.Exec(ctx, `
		INSERT INTO groups (id, adapter_id, external_id, name, scim_resource) VALUES ($1, $2, $3, $4, $5)`,
		groupID, adapterID, key, displayName, nullableBytes(resource)); err != nil {
		return "", errs.Wrap(errs.Internal, "Could not create the group.", err)
	}
	if err := setMembersTx(ctx, tx, adapterID, groupID, members, true); err != nil {
		return "", err
	}
	return groupID, commit(ctx, tx, "Could not create the group.")
}

// ErrSCIMConflict is a SCIM uniqueness conflict (RFC 7644 §3.3).
var ErrSCIMConflict = errs.New(errs.ValidInvalid, "A resource with that name already exists for this provider.")

// Update renames a group, replaces what the client said about it, and — when
// members is non-nil — replaces its membership.
func (s *SCIMGroups) Update(ctx context.Context, adapterID, groupID string, displayName *string, resource []byte, members []string, replaceMembers bool) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the group.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE groups SET name = coalesce($3, name), scim_resource = coalesce($4, scim_resource), updated_at = now()
		WHERE id = $1 AND adapter_id = $2`, groupID, adapterID, displayName, nullableBytes(resource))
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the group.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "There is no such group for this provider.")
	}
	if replaceMembers {
		if err := setMembersTx(ctx, tx, adapterID, groupID, members, true); err != nil {
			return err
		}
	}
	return commit(ctx, tx, "Could not update the group.")
}

// ChangeMembers adds or removes members, leaving the rest.
func (s *SCIMGroups) ChangeMembers(ctx context.Context, adapterID, groupID string, add, remove []string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the group.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM groups WHERE id = $1 AND adapter_id = $2)`,
		groupID, adapterID).Scan(&found); err != nil {
		return errs.Wrap(errs.Internal, "Could not update the group.", err)
	}
	if !found {
		return errs.New(errs.NotFound, "There is no such group for this provider.")
	}
	before, err := peopleWhoManage(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the group.", err)
	}
	if err := insertMembers(ctx, tx, adapterID, groupID, add); err != nil {
		return err
	}
	if len(remove) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM group_members WHERE group_id = $1 AND user_id = ANY ($2)`,
			groupID, remove); err != nil {
			return errs.Wrap(errs.Internal, "Could not update the group.", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE groups SET updated_at = now() WHERE id = $1`, groupID); err != nil {
		return errs.Wrap(errs.Internal, "Could not update the group.", err)
	}
	if err := refuseLockout(ctx, tx, before); err != nil {
		return err
	}
	return commit(ctx, tx, "Could not update the group.")
}

func setMembersTx(ctx context.Context, tx pgx.Tx, adapterID, groupID string, members []string, replace bool) error {
	before, err := peopleWhoManage(ctx, tx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the group.", err)
	}
	if replace {
		if _, err := tx.Exec(ctx, `DELETE FROM group_members WHERE group_id = $1`, groupID); err != nil {
			return errs.Wrap(errs.Internal, "Could not update the group.", err)
		}
	}
	if err := insertMembers(ctx, tx, adapterID, groupID, members); err != nil {
		return err
	}
	return refuseLockout(ctx, tx, before)
}

// insertMembers adds members, refusing an ID this provider did not provision
// or sign in: a SCIM client manages its own people and nobody else.
func insertMembers(ctx context.Context, tx pgx.Tx, adapterID, groupID string, userIDs []string) error {
	for _, userID := range userIDs {
		var ok bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM user_identities WHERE adapter_id = $1 AND user_id = $2)`,
			adapterID, userID).Scan(&ok); err != nil {
			return errs.Wrap(errs.Internal, "Could not update the group.", err)
		}
		if !ok {
			return errs.Newf(errs.NotFound, "%q is not a user this provider manages.", userID)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			groupID, userID); err != nil {
			return errs.Wrap(errs.Internal, "Could not update the group.", err)
		}
	}
	return nil
}

// UserGroups lists the provider's groups a user is in, for the user resource.
func (s *SCIMGroups) UserGroups(ctx context.Context, adapterID, userID string) ([]SCIMMember, error) {
	rows, err := s.db.Query(ctx, `
		SELECT g.id, g.name FROM group_members m JOIN groups g ON g.id = m.group_id
		WHERE g.adapter_id = $1 AND m.user_id = $2 ORDER BY g.name`, adapterID, userID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the account's groups.", err)
	}
	defer rows.Close()
	var out []SCIMMember
	for rows.Next() {
		var m SCIMMember
		if err := rows.Scan(&m.UserID, &m.Display); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the account's groups.", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func itoa(n int) string {
	const digits = "0123456789"
	if n < 10 {
		return digits[n : n+1]
	}
	return itoa(n/10) + digits[n%10:n%10+1]
}
