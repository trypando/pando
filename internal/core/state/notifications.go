package state

import (
	"context"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Notifications stores console notifications (R-231).
//
// Implements the console notify adapter's Sink. The adapter holds no storage of
// its own, which is R-027: an adapter never touches state, so core hands it a
// writer rather than a database.
type Notifications struct{ db *DB }

func NewNotifications(db *DB) *Notifications { return &Notifications{db: db} }

// Notification is one stored message.
type Notification struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	AppID     string     `json:"app_id,omitempty"`
	AppName   string     `json:"app_name,omitempty"`
	Link      string     `json:"link,omitempty"`
	EventID   string     `json:"event_id,omitempty"`
	Kind      string     `json:"kind"`
	Subject   string     `json:"subject"`
	Body      string     `json:"body,omitempty"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// Record writes one row per recipient.
//
// Per recipient rather than one row with a list, so "mark as read" is a write
// to one row and an unread count is a count. A shared row would make both of
// those a join against a second table that does not exist yet.
func (n *Notifications) Record(ctx context.Context, msg api.Notification, retainFor time.Duration) error {
	var retain *time.Time
	if retainFor > 0 {
		at := time.Now().UTC().Add(retainFor)
		retain = &at
	}

	for _, r := range msg.Recipients {
		if r.UserID == "" {
			continue
		}
		_, err := n.db.Exec(ctx, `
			INSERT INTO notifications (id, user_id, app_id, kind, subject, body, retain_until, link, event_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			id.New(id.Notification), r.UserID, nullable(msg.AppID),
			string(msg.Kind), msg.Subject, msg.Body, retain, nullable(msg.Link), nullable(msg.EventID))
		if err != nil {
			return errs.Wrap(errs.Internal, "Could not record the notification.", err)
		}
	}
	return nil
}

// ListForUser returns a user's notifications, newest first: up to limit,
// older than the notification ID before when one is given (a cursor).
func (n *Notifications) ListForUser(ctx context.Context, userID string, unreadOnly bool, before string, limit int) ([]Notification, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := n.db.Query(ctx, `
		SELECT nt.id, nt.user_id, coalesce(nt.app_id, ''), coalesce(a.name, ''), nt.kind, nt.subject, nt.body,
		       coalesce(nt.link, ''), coalesce(nt.event_id, ''), nt.read_at, nt.created_at
		FROM notifications nt LEFT JOIN apps a ON a.id = nt.app_id
		WHERE nt.user_id = $1 AND (NOT $2 OR nt.read_at IS NULL) AND ($3 = '' OR nt.id < $3)
		  AND (nt.retain_until IS NULL OR nt.retain_until > now())
		ORDER BY nt.id DESC
		LIMIT $4`, userID, unreadOnly, before, limit)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read your notifications.", err)
	}
	defer rows.Close()

	out := make([]Notification, 0)
	for rows.Next() {
		var rec Notification
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.AppID, &rec.AppName, &rec.Kind,
			&rec.Subject, &rec.Body, &rec.Link, &rec.EventID, &rec.ReadAt, &rec.CreatedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read your notifications.", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Unread counts a user's unread notifications, for the inbox's badge.
func (n *Notifications) Unread(ctx context.Context, userID string) (int, error) {
	var count int
	err := n.db.QueryRow(ctx, `
		SELECT count(*) FROM notifications
		WHERE user_id = $1 AND read_at IS NULL AND (retain_until IS NULL OR retain_until > now())`, userID).Scan(&count)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not count your notifications.", err)
	}
	return count, nil
}

// MarkAllRead marks every one of a user's notifications read.
func (n *Notifications) MarkAllRead(ctx context.Context, userID string) error {
	if _, err := n.db.Exec(ctx,
		`UPDATE notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL`, userID); err != nil {
		return errs.Wrap(errs.Internal, "Could not mark your notifications as read.", err)
	}
	return nil
}

// MarkRead marks one notification read, for its owner only.
//
// The user ID is in the WHERE clause rather than checked first: a check and
// then an update is two statements with a gap between them, and the gap is
// where someone marks a notification that is not theirs.
func (n *Notifications) MarkRead(ctx context.Context, userID, notificationID string) error {
	_, err := n.db.Exec(ctx,
		`UPDATE notifications SET read_at = now() WHERE id = $1 AND user_id = $2 AND read_at IS NULL`,
		notificationID, userID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not mark the notification as read.", err)
	}
	return nil
}

// No compile-time assertion that this satisfies the adapter's Sink: that would
// mean importing the adapter here, which is the dependency this signature
// exists to avoid. main wires the two together, and a mismatch fails there.
