package subscription

import (
	"context"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
)

// Inbox is the console's notifications, read by the person they are for
// (R-377): what the console notify adapter records, which until issue #50
// nothing displayed.
type Inbox struct {
	Store *state.Notifications
}

// InboxPage is one page of a person's inbox, and how many are unread in all.
type InboxPage struct {
	Notifications []state.Notification `json:"notifications"`
	Unread        int                  `json:"unread"`
	NextBefore    string               `json:"next_before"`
}

// List returns the caller's notifications, newest first.
func (b *Inbox) List(ctx context.Context, p authz.Principal, unreadOnly bool, before string, limit int) (InboxPage, error) {
	if err := personOnly(p); err != nil {
		return InboxPage{}, err
	}
	list, err := b.Store.ListForUser(ctx, p.UserID, unreadOnly, before, limit)
	if err != nil {
		return InboxPage{}, err
	}
	unread, err := b.Store.Unread(ctx, p.UserID)
	if err != nil {
		return InboxPage{}, err
	}
	page := InboxPage{Notifications: list, Unread: unread}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if len(list) == limit {
		page.NextBefore = list[len(list)-1].ID
	}
	return page, nil
}

// MarkRead marks one of the caller's notifications read. Another person's is
// left alone, without saying it exists.
func (b *Inbox) MarkRead(ctx context.Context, p authz.Principal, notificationID string) error {
	if err := personOnly(p); err != nil {
		return err
	}
	return b.Store.MarkRead(ctx, p.UserID, notificationID)
}

// MarkAllRead marks every one of the caller's notifications read.
func (b *Inbox) MarkAllRead(ctx context.Context, p authz.Principal) error {
	if err := personOnly(p); err != nil {
		return err
	}
	return b.Store.MarkAllRead(ctx, p.UserID)
}
