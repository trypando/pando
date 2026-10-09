package state

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// App limits (R-244, issue #131): how many apps a person may own. Host
// policy's default is in the policy document; a user's and a group's own
// values are here. Nil is not set; zero is unlimited.

// AppLimits reads and writes users' and groups' own limits.
type AppLimits struct{ db *DB }

func NewAppLimits(db *DB) *AppLimits { return &AppLimits{db: db} }

// GroupLimit is a group's own limit, as one of a person's groups.
type GroupLimit struct {
	GroupID string
	Name    string
	MaxApps int
}

// LimitFacts is everything a person's limit is decided from, but host policy.
type LimitFacts struct {
	// UserMaxApps is the user's own value, nil when not set.
	UserMaxApps *int
	// Groups are the person's groups that set a value. Through
	// effective_group_members, so a provider's group linked to a Pando group
	// counts like a direct member.
	Groups []GroupLimit
	// Owned is how many apps they own now, in any state but deleted.
	Owned int
}

// Facts reads a person's limit facts.
func (l *AppLimits) Facts(ctx context.Context, userID string) (LimitFacts, error) {
	var f LimitFacts
	err := l.db.QueryRow(ctx, `
		SELECT u.max_apps,
		       (SELECT count(*) FROM apps WHERE owner_user_id = u.id AND deleted_at IS NULL)
		FROM users u WHERE u.id = $1`, userID).Scan(&f.UserMaxApps, &f.Owned)
	if errors.Is(err, pgx.ErrNoRows) {
		return LimitFacts{}, errs.New(errs.NotFound, "There is no such user.")
	}
	if err != nil {
		return LimitFacts{}, errs.Wrap(errs.Internal, "Could not read the user's app limit.", err)
	}

	rows, err := l.db.Query(ctx, `
		SELECT DISTINCT g.id, g.name, g.max_apps
		FROM effective_group_members m
		JOIN groups g ON g.id = m.group_id
		WHERE m.user_id = $1 AND g.max_apps IS NOT NULL
		ORDER BY g.id`, userID)
	if err != nil {
		return LimitFacts{}, errs.Wrap(errs.Internal, "Could not read the user's groups.", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g GroupLimit
		if err := rows.Scan(&g.GroupID, &g.Name, &g.MaxApps); err != nil {
			return LimitFacts{}, errs.Wrap(errs.Internal, "Could not read the user's groups.", err)
		}
		f.Groups = append(f.Groups, g)
	}
	if err := rows.Err(); err != nil {
		return LimitFacts{}, errs.Wrap(errs.Internal, "Could not read the user's groups.", err)
	}
	return f, nil
}

// SetUserMaxApps sets a user's own limit, or clears it with nil.
func (l *AppLimits) SetUserMaxApps(ctx context.Context, userID string, maxApps *int) error {
	tag, err := l.db.Exec(ctx, `UPDATE users SET max_apps = $2, updated_at = now() WHERE id = $1`, userID, maxApps)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not save the user's app limit.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "There is no such user.")
	}
	return nil
}

// GroupMaxApps reads a group's own limit, nil when not set.
func (l *AppLimits) GroupMaxApps(ctx context.Context, groupID string) (*int, error) {
	var maxApps *int
	err := l.db.QueryRow(ctx, `SELECT max_apps FROM groups WHERE id = $1`, groupID).Scan(&maxApps)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.NotFound, "There is no such group.")
	}
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the group's app limit.", err)
	}
	return maxApps, nil
}

// SetGroupMaxApps sets a group's own limit, or clears it with nil.
func (l *AppLimits) SetGroupMaxApps(ctx context.Context, groupID string, maxApps *int) error {
	tag, err := l.db.Exec(ctx, `UPDATE groups SET max_apps = $2 WHERE id = $1`, groupID, maxApps)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not save the group's app limit.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "There is no such group.")
	}
	return nil
}
