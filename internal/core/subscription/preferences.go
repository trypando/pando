package subscription

import (
	"context"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// Channel is a notification adapter that reaches people, as a preferences
// screen offers it.
type Channel struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// PreferencesView is everything a person may choose and what they have chosen:
// every kind on every channel, defaults filled in, so a client shows the answer
// rather than working it out (R-261).
type PreferencesView struct {
	Kinds    []Kind                         `json:"kinds"`
	Channels []Channel                      `json:"channels"`
	Choices  []state.NotificationPreference `json:"choices"`
}

func (s *Service) channels() []Channel {
	out := []Channel{}
	if s.Registry == nil {
		return out
	}
	for _, ref := range s.Registry.ByCategory(api.CategoryNotify) {
		a, ok := s.Registry.Notify(ref)
		if ok && a.Capabilities().Audience == api.AudiencePeople {
			out = append(out, Channel{ID: ref, Kind: a.Kind()})
		}
	}
	return out
}

// Destination is a notification adapter a subscription may send through.
type Destination struct {
	ID       string             `json:"id"`
	Kind     string             `json:"kind"`
	Audience api.NotifyAudience `json:"audience"`
}

// Destinations lists every notification adapter a subscription may send
// through, for a form that offers them to somebody who may not read the
// adapters themselves. IDs and kinds only: an adapter's configuration is the
// adapters screen's.
func (s *Service) Destinations() []Destination {
	out := []Destination{}
	if s.Registry == nil {
		return out
	}
	for _, ref := range s.Registry.ByCategory(api.CategoryNotify) {
		if a, ok := s.Registry.Notify(ref); ok {
			out = append(out, Destination{ID: ref, Kind: a.Kind(), Audience: a.Capabilities().Audience})
		}
	}
	return out
}

func personOnly(p authz.Principal) error {
	if p.UserID == "" {
		return errs.New(errs.PermDenied,
			"Notification preferences belong to a person, and this request was made by an account token that has none.")
	}
	return nil
}

// Preferences returns the caller's notification preferences (R-373).
func (s *Service) Preferences(ctx context.Context, p authz.Principal) (PreferencesView, error) {
	if err := personOnly(p); err != nil {
		return PreferencesView{}, err
	}
	chosen, err := s.Prefs.List(ctx, p.UserID)
	if err != nil {
		return PreferencesView{}, err
	}
	set := map[choice]bool{}
	for _, c := range chosen {
		set[choice{c.Kind, c.Channel}] = c.Enabled
	}
	view := PreferencesView{Kinds: Kinds, Channels: s.channels(), Choices: []state.NotificationPreference{}}
	for _, k := range Kinds {
		for _, ch := range view.Channels {
			view.Choices = append(view.Choices, state.NotificationPreference{
				Kind: string(k.Kind), Channel: ch.ID, Enabled: wants(set, k.Kind, ch.ID),
			})
		}
	}
	return view, nil
}

// SetPreferences records the caller's choices. Each names a kind Pando sends
// and a channel that reaches people; anything else is refused rather than
// stored, so a typo does not look like a preference that took.
func (s *Service) SetPreferences(ctx context.Context, p authz.Principal, choices []state.NotificationPreference) (PreferencesView, error) {
	if err := personOnly(p); err != nil {
		return PreferencesView{}, err
	}
	channels := map[string]bool{}
	for _, ch := range s.channels() {
		channels[ch.ID] = true
	}
	for _, c := range choices {
		if !IsKind(c.Kind) {
			return PreferencesView{}, errs.Newf(errs.ValidInvalid,
				"%q is not a notification Pando sends. Valid answers are the kinds GET /api/v1/notification-preferences lists, such as app_failed.", c.Kind)
		}
		if !channels[c.Channel] {
			return PreferencesView{}, errs.Newf(errs.ValidInvalid,
				"%q is not a notification channel that reaches people on this installation. Valid answers are the channels GET /api/v1/notification-preferences lists.", c.Channel)
		}
	}
	if err := s.Prefs.Set(ctx, p.UserID, choices); err != nil {
		return PreferencesView{}, err
	}
	return s.Preferences(ctx, p)
}
