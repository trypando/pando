package subscription

import (
	"context"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/log"
)

// Preferences reads a person's notification choices.
type Preferences interface {
	List(ctx context.Context, userID string) ([]state.NotificationPreference, error)
}

// Router sends Pando's own notifications — a deploy waiting for approval, an
// app that failed — to the people they name, on every channel that reaches
// people, as each person's preferences allow (R-373).
//
// A channel adapter (Slack, Teams, Discord, ntfy) is skipped: a message meant
// for one person must not land in a room. Channels hear from subscriptions.
//
// Best effort, like what it replaced: an adapter that fails is logged and the
// rest still send, and telling people never blocks what it is about (R-159).
type Router struct {
	Registry    *api.Registry
	Preferences Preferences
	Users       Users
	Logger      *zap.Logger
}

func (r Router) logger() *zap.Logger {
	if r.Logger == nil {
		return zap.NewNop()
	}
	return r.Logger
}

// Notify sends msg.
func (r Router) Notify(ctx context.Context, msg api.Notification) error {
	if r.Registry == nil {
		return nil
	}
	recipients := r.withEmail(ctx, msg.Recipients)
	choices := r.choices(ctx, recipients)

	for _, ref := range r.Registry.ByCategory(api.CategoryNotify) {
		adapter, ok := r.Registry.Notify(ref)
		if !ok || adapter.Capabilities().Audience != api.AudiencePeople {
			continue
		}
		var to []api.Recipient
		for _, rcpt := range recipients {
			if wants(choices[rcpt.UserID], msg.Kind, ref) {
				to = append(to, rcpt)
			}
		}
		if len(to) == 0 && len(msg.Recipients) > 0 {
			continue
		}
		out := msg
		out.Recipients = to
		if err := adapter.Notify(ctx, out); err != nil {
			r.logger().Warn("a notification adapter could not send",
				zap.String("adapter", ref), zap.String("kind", string(msg.Kind)), zap.Error(err))
		}
	}
	return nil
}

// withEmail fills in each recipient's address, for an adapter that sends mail.
func (r Router) withEmail(ctx context.Context, in []api.Recipient) []api.Recipient {
	out := make([]api.Recipient, 0, len(in))
	for _, rcpt := range in {
		if rcpt.Email == "" && rcpt.UserID != "" && r.Users != nil {
			if u, ok, err := r.Users.ByID(ctx, rcpt.UserID); err == nil && ok {
				rcpt.Email = u.Email
			}
		}
		out = append(out, rcpt)
	}
	return out
}

type choice struct{ kind, channel string }

func (r Router) choices(ctx context.Context, recipients []api.Recipient) map[string]map[choice]bool {
	out := map[string]map[choice]bool{}
	if r.Preferences == nil {
		return out
	}
	for _, rcpt := range recipients {
		if rcpt.UserID == "" || out[rcpt.UserID] != nil {
			continue
		}
		prefs, err := r.Preferences.List(ctx, rcpt.UserID)
		if err != nil {
			// The ID can come from a request — the person an app was shared
			// with — so it is logged as untrusted, like a path.
			r.logger().Warn("could not read notification preferences; sending the defaults",
				log.Untrusted("user_id", rcpt.UserID), zap.Error(err))
		}
		m := map[choice]bool{}
		for _, p := range prefs {
			m[choice{p.Kind, p.Channel}] = p.Enabled
		}
		out[rcpt.UserID] = m
	}
	return out
}

// wants is one person's answer for one kind on one channel: what they chose,
// or the kind's default.
func wants(chosen map[choice]bool, kind api.NotificationKind, channel string) bool {
	if on, ok := chosen[choice{string(kind), channel}]; ok {
		return on
	}
	return defaultOn(kind)
}
