package idle

import (
	"context"
	"time"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// An app's own idle settings, and where its clock stands (R-397). The API's
// GET and PUT /apps/{id}/idle are this, so the console, the CLI and an agent
// all see the same dates.

// SettingsStore reads and writes an app's idle settings.
type SettingsStore interface {
	IdleStatus(ctx context.Context, appID string) (state.IdleStatus, error)
	SetIdleSettings(ctx context.Context, appID string, s state.IdleSettings) error
}

// Settings reads and changes an app's idle settings.
type Settings struct {
	Store  SettingsStore
	Policy Policy
	Clock  clock.Clock
}

// Report is an app's idle settings and what they mean for it now.
type Report struct {
	// StopDays and DeleteDays are the app's own: null is the install's, 0 is
	// off.
	StopDays   *int `json:"stop_days"`
	DeleteDays *int `json:"delete_days"`

	// InstallStopDays and InstallDeleteDays are host policy's.
	InstallStopDays   int `json:"install_stop_days"`
	InstallDeleteDays int `json:"install_delete_days"`

	// EffectiveStopDays and EffectiveDeleteDays are in force: the app's own
	// where set, the install's otherwise. 0 is off.
	EffectiveStopDays   int `json:"effective_stop_days"`
	EffectiveDeleteDays int `json:"effective_delete_days"`

	// LastActivityAt is the last request let through, deploy or start, or
	// the app's creation (R-394).
	LastActivityAt time.Time `json:"last_activity_at"`

	// StopsAt and DeletesAt are when Pando would act if nothing happens
	// before then; absent when it would not. Never sooner than a notice's
	// length after the owner is told (R-395).
	StopsAt   *time.Time `json:"stops_at,omitempty"`
	DeletesAt *time.Time `json:"deletes_at,omitempty"`

	// StoppedForIdle says Pando stopped it for being idle (R-396).
	StoppedForIdle bool `json:"stopped_for_idle"`
}

// Report reads an app's report.
func (s *Settings) Report(ctx context.Context, appID string) (Report, error) {
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return Report{}, err
	}
	st, err := s.Store.IdleStatus(ctx, appID)
	if err != nil {
		return Report{}, err
	}
	return report(doc, st, s.now()), nil
}

// Set replaces an app's own settings. Refused when a value is below zero, or
// when the delete in force would come no later than the stop in force.
func (s *Settings) Set(ctx context.Context, appID string, own state.IdleSettings) (Report, error) {
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return Report{}, err
	}

	stopName, deleteName := "stop_days", "delete_days"
	stop, del := doc.IdleStopDays, doc.IdleDeleteDays
	if own.StopDays != nil {
		stop = *own.StopDays
	} else {
		stopName = "the installation's idle_stop_days"
	}
	if own.DeleteDays != nil {
		del = *own.DeleteDays
	} else {
		deleteName = "the installation's idle_delete_days"
	}
	if err := policy.ValidateIdleDays(stopName, deleteName, stop, del); err != nil {
		// Begins with a field name, as host policy's own refusals do, so it
		// is left as written rather than capitalized into a different word.
		return Report{}, errs.New(errs.ValidInvalid, err.Error()+".")
	}

	if err := s.Store.SetIdleSettings(ctx, appID, own); err != nil {
		return Report{}, err
	}
	return s.Report(ctx, appID)
}

func report(doc policy.Document, st state.IdleStatus, now time.Time) Report {
	r := Report{
		StopDays: st.StopDays, DeleteDays: st.DeleteDays,
		InstallStopDays: doc.IdleStopDays, InstallDeleteDays: doc.IdleDeleteDays,
		EffectiveStopDays: doc.IdleStopDays, EffectiveDeleteDays: doc.IdleDeleteDays,
		LastActivityAt: st.LastActivity,
		StoppedForIdle: st.StoppedForIdle,
	}
	if st.StopDays != nil {
		r.EffectiveStopDays = *st.StopDays
	}
	if st.DeleteDays != nil {
		r.EffectiveDeleteDays = *st.DeleteDays
	}

	if r.EffectiveStopDays > 0 && st.DesiredState == state.StateRunning && st.State != state.StateFailed {
		at := due(st.LastActivity, st.StopNoticedAt, r.EffectiveStopDays, now)
		r.StopsAt = &at
	}
	if r.EffectiveDeleteDays > 0 {
		at := due(st.LastActivity, st.DeleteNoticedAt, r.EffectiveDeleteDays, now)
		r.DeletesAt = &at
	}
	return r
}

// due is when the pass would act on a setting of n days: from the notice
// already given, or from the one it would give, which is never in the past.
// A notice older than the last activity was answered and does not count.
func due(lastActivity time.Time, noticed *time.Time, n int, now time.Time) time.Time {
	if noticed != nil && !noticed.Before(lastActivity) {
		return ActionAt(lastActivity, *noticed, n)
	}
	noticeAt := lastActivity.Add(noticeAfter(n))
	if noticeAt.Before(now) {
		noticeAt = now
	}
	return ActionAt(lastActivity, noticeAt, n)
}

func (s *Settings) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now()
}
