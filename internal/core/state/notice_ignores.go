package state

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// NoticeIgnores is the notices on an app each person has chosen to ignore
// (R-412).
//
// Every query is keyed on the person: one person's ignores are invisible to
// everyone else, who still see the notice.
type NoticeIgnores struct{ db *DB }

// NewNoticeIgnores returns the store.
func NewNoticeIgnores(db *DB) *NoticeIgnores { return &NoticeIgnores{db: db} }

type noticeIgnoreRow struct{ key, fingerprint string }

// For returns a person's ignores on one app: each notice key, with the
// fingerprint the notice had when it was ignored.
func (n *NoticeIgnores) For(ctx context.Context, userID, appID string) (map[string]string, error) {
	rows, err := queryAll(ctx, n.db, "Could not read the notices you have ignored.",
		func(r pgx.CollectableRow) (out noticeIgnoreRow, err error) {
			return out, r.Scan(&out.key, &out.fingerprint)
		}, `
		SELECT notice_key, fingerprint FROM notice_ignores
		WHERE user_id = $1 AND app_id = $2`, userID, appID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.key] = r.fingerprint
	}
	return out, nil
}

// Ignore records that a person ignores a notice as it reads now. Ignoring it
// again replaces the fingerprint.
func (n *NoticeIgnores) Ignore(ctx context.Context, userID, appID, key, fingerprint string) error {
	_, err := n.db.Exec(ctx, `
		INSERT INTO notice_ignores (user_id, app_id, notice_key, fingerprint) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, app_id, notice_key)
		DO UPDATE SET fingerprint = EXCLUDED.fingerprint, updated_at = now()`,
		userID, appID, key, fingerprint)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not ignore the notice.", err)
	}
	return nil
}

// Restore removes a person's ignore of a notice. Removing one that is not
// there is not an error.
func (n *NoticeIgnores) Restore(ctx context.Context, userID, appID, key string) error {
	_, err := n.db.Exec(ctx,
		`DELETE FROM notice_ignores WHERE user_id = $1 AND app_id = $2 AND notice_key = $3`,
		userID, appID, key)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not restore the notice.", err)
	}
	return nil
}
