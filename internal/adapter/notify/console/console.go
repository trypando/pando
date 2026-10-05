// Package console is the v1 notification adapter (R-230, R-231).
//
// Console-only: a notification is recorded and shown to the recipient the next
// time they look at Pando. Nothing is sent anywhere, and that is the whole of
// R-231 rather than a placeholder for it.
//
// The honest consequence, which the product has to state rather than imply: a
// console-only notification reaches nobody who is not already looking. That is
// why R-266 says sharing an app sends no message — the tile appearing in the
// recipient's launcher is the notification, and it is strictly better than a
// console message, because it is waiting there for someone who has never signed
// in. An adapter that can reach a person elsewhere is R-232, and LATER.
//
// This adapter exists now so the category is exercised: the interface has an
// implementation, main registers it, and R-232's SMTP adapter is a new file
// rather than a new seam.
package console

import (
	"context"
	"encoding/json"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Kind is the adapter's kind string.
const Kind = "console"

// Adapter records notifications for display in the console.
type Adapter struct {
	sink   Sink
	config Config
}

// Config is the adapter's configuration.
type Config struct {
	// Retain is how long a delivered notification is kept. A console
	// notification that is never read is not a failure worth keeping forever.
	RetainDays int `json:"retain_days,omitempty"`
}

// Sink is where a notification is recorded.
//
// An interface so this adapter stores nothing itself: R-027 forbids an adapter
// touching state, and this is that boundary — main supplies a writer that core
// owns.
//
// The signature uses only api types on purpose. A struct declared here would
// have to be imported by whatever implements this, which would make core depend
// on one specific adapter — the dependency running backwards, past the boundary
// the lint is guarding, in a direction the lint does not check.
type Sink interface {
	Record(ctx context.Context, n api.Notification, retain time.Duration) error
}

// New returns an adapter writing to sink.
func New(sink Sink) *Adapter { return &Adapter{sink: sink} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryNotify }

// Capabilities: the console reaches the people a notification names.
func (a *Adapter) Capabilities() api.NotifyCapabilities {
	return api.NotifyCapabilities{Audience: api.AudiencePeople}
}

func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	cfg := Config{RetainDays: 30}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The console notification configuration could not be read.", err)
		}
	}
	a.config = cfg
	return nil
}

// HealthCheck succeeds when there is somewhere to record to.
func (a *Adapter) HealthCheck(context.Context) error {
	if a.sink == nil {
		return errs.New(errs.Internal, "Console notifications have nowhere to be recorded.")
	}
	return nil
}

// Notify records the message.
//
// Recipients with no user ID are dropped rather than guessed at. This adapter
// reaches people inside Pando, and a recipient Pando has no account for is
// someone it cannot reach — saying so by dropping them is better than recording
// a notification addressed to nobody, which would look delivered.
func (a *Adapter) Notify(ctx context.Context, n api.Notification) error {
	if a.sink == nil {
		return errs.New(errs.Internal, "Console notifications have nowhere to be recorded.")
	}

	reachable := make([]api.Recipient, 0, len(n.Recipients))
	for _, r := range n.Recipients {
		if r.UserID != "" {
			reachable = append(reachable, r)
		}
	}
	if len(reachable) == 0 {
		return nil
	}
	n.Recipients = reachable

	return a.sink.Record(ctx, n, time.Duration(a.config.RetainDays)*24*time.Hour)
}

var _ api.NotifyAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryNotify,
		Kind:        Kind,
		Name:        "Console",
		Description: "Shows notifications in the console.",
		IDPrefix:    "ntf_",
		Fields: []api.Field{
			{Key: "retain_days", Label: "Keep for", Type: "int", Help: "Days a notification is kept.", Default: "30", Advanced: true},
		},
	}
}
