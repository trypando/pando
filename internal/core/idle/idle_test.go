package idle_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/appdelete"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/idle"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

const day = 24 * time.Hour

// fakeStore returns every app as a candidate: the pass must decide on its own
// what is owed, whatever the store's prefilter lets through.
type fakeStore struct {
	apps     map[string]*state.IdleApp
	defaults state.IdleDefaults
	own      map[string][2]*int
}

func newStore(apps ...state.IdleApp) *fakeStore {
	s := &fakeStore{apps: map[string]*state.IdleApp{}, own: map[string][2]*int{}}
	for i := range apps {
		a := apps[i]
		s.apps[a.AppID] = &a
	}
	return s
}

func (s *fakeStore) IdleCandidates(_ context.Context, d state.IdleDefaults, _ time.Time) ([]state.IdleApp, error) {
	s.defaults = d
	var out []state.IdleApp
	for _, a := range s.apps {
		app := *a
		own := s.own[a.AppID]
		app.StopDays, app.DeleteDays = d.StopDays, d.DeleteDays
		if own[0] != nil {
			app.StopDays = *own[0]
		}
		if own[1] != nil {
			app.DeleteDays = *own[1]
		}
		out = append(out, app)
	}
	return out, nil
}

func (s *fakeStore) SetIdleNotice(_ context.Context, appID string, which state.IdleNotice, at *time.Time) error {
	if which == state.IdleNoticeStop {
		s.apps[appID].StopNoticedAt = at
	} else {
		s.apps[appID].DeleteNoticedAt = at
	}
	return nil
}

func (s *fakeStore) StopForIdle(_ context.Context, appID string) (bool, error) {
	a := s.apps[appID]
	if a.DesiredState != state.StateRunning {
		return false, nil
	}
	a.DesiredState, a.StoppedForIdle, a.StopNoticedAt = state.StateStopped, true, nil
	return true, nil
}

func (s *fakeStore) ByID(_ context.Context, appID string) (state.App, bool, error) {
	a, ok := s.apps[appID]
	if !ok {
		return state.App{}, false, nil
	}
	return state.App{ID: a.AppID, Name: a.Name, OwnerUserID: a.OwnerUserID}, true, nil
}

type fakeDeleter struct {
	store *fakeStore
	err   error
	reqs  []appdelete.Request
}

func (d *fakeDeleter) Delete(_ context.Context, app state.App, req appdelete.Request) (appdelete.Result, error) {
	d.reqs = append(d.reqs, req)
	if d.err != nil {
		return appdelete.Result{}, d.err
	}
	delete(d.store.apps, app.ID)
	return appdelete.Result{BackupID: "bak_1", VolumesDiscarded: 1}, nil
}

type fakePolicy struct{ doc policy.Document }

func (p fakePolicy) Load(context.Context) (policy.Document, error) { return p.doc, nil }

type notices struct{ sent []api.Notification }

func (n *notices) Notify(_ context.Context, note api.Notification) error {
	n.sent = append(n.sent, note)
	return nil
}

type auditLog struct{ events []audit.Event }

func (a *auditLog) Write(_ context.Context, e audit.Event) error {
	a.events = append(a.events, e)
	return nil
}

func (a *auditLog) actions() []string {
	var out []string
	for _, e := range a.events {
		out = append(out, e.Action)
	}
	return out
}

type rig struct {
	clock   *clock.Fake
	store   *fakeStore
	deleter *fakeDeleter
	notes   *notices
	audit   *auditLog
	pass    *idle.Pass
}

func newRig(doc policy.Document, apps ...state.IdleApp) *rig {
	r := &rig{clock: clock.NewFake(time.Time{}), store: newStore(apps...), notes: &notices{}, audit: &auditLog{}}
	r.deleter = &fakeDeleter{store: r.store}
	r.pass = &idle.Pass{
		Store: r.store, Apps: r.store, Deleter: r.deleter, Policy: fakePolicy{doc},
		Notifier: r.notes, Auditor: r.audit, Clock: r.clock,
	}
	return r
}

// use gives the rig these apps.
func (r *rig) use(apps ...state.IdleApp) {
	r.store = newStore(apps...)
	r.deleter.store, r.pass.Store, r.pass.Apps = r.store, r.store, r.store
}

// runningApp is an app whose last activity was at the rig's start.
func (r *rig) runningApp(id string) state.IdleApp {
	return state.IdleApp{
		AppID: id, Name: "Expenses", OwnerUserID: "usr_owner",
		State: state.StateRunning, DesiredState: state.StateRunning,
		LastActivity: r.clock.Now(),
	}
}

// TestR393_AnIdleAppIsStoppedAfterItsNotice asserts R-393 and R-395: an app
// nobody uses is told about 7 days before the setting and stopped at it.
func TestR393_AnIdleAppIsStoppedAfterItsNotice(t *testing.T) {
	r := newRig(policy.Document{IdleStopDays: 30})
	r.use(r.runningApp("app_1"))
	ctx := context.Background()

	r.clock.Advance(22 * day)
	r.pass.Once(ctx)
	assert.Empty(t, r.notes.sent, "nothing is owed before 23 days")

	r.clock.Advance(day)
	r.pass.Once(ctx)
	require.Len(t, r.notes.sent, 1, "the owner is told 7 days before")
	assert.Equal(t, api.NotifyAppIdle, r.notes.sent[0].Kind)
	assert.Equal(t, "usr_owner", r.notes.sent[0].Recipients[0].UserID)
	assert.Contains(t, r.notes.sent[0].Subject, "will be stopped on January 31, 2026")

	r.clock.Advance(6 * day)
	r.pass.Once(ctx)
	assert.Equal(t, state.StateRunning, r.store.apps["app_1"].DesiredState, "not before the notice has run")

	r.clock.Advance(day)
	r.pass.Once(ctx)
	app := r.store.apps["app_1"]
	assert.Equal(t, state.StateStopped, app.DesiredState)
	assert.True(t, app.StoppedForIdle, "recorded as Pando's stop, not its owner's (R-396)")
	assert.Equal(t, []string{"app.idle.notice", "app.idle.stopped"}, r.audit.actions())
	require.Len(t, r.notes.sent, 2, "and told when it happens")
	assert.Contains(t, r.notes.sent[1].Body, "start it again")
}

// TestR395_NothingStopsWithoutAWeeksNotice asserts R-395: a setting shorter
// than the notice still waits it out, and the notice is not sent before a
// whole day without activity.
func TestR395_NothingStopsWithoutAWeeksNotice(t *testing.T) {
	r := newRig(policy.Document{IdleStopDays: 3})
	r.use(r.runningApp("app_1"))
	ctx := context.Background()

	r.clock.Advance(12 * time.Hour)
	r.pass.Once(ctx)
	assert.Empty(t, r.notes.sent, "an app used this morning is not warned")

	r.clock.Advance(12 * time.Hour)
	r.pass.Once(ctx)
	require.Len(t, r.notes.sent, 1)

	r.clock.Advance(7*day - time.Minute)
	r.pass.Once(ctx)
	assert.Equal(t, state.StateRunning, r.store.apps["app_1"].DesiredState, "three days are up, the notice is not")

	r.clock.Advance(time.Minute)
	r.pass.Once(ctx)
	assert.Equal(t, state.StateStopped, r.store.apps["app_1"].DesiredState)
}

// TestR395_UseAfterANoticeWithdrawsIt asserts R-395: activity after a
// notice answers it, and the next idle stretch gets a notice of its own.
func TestR395_UseAfterANoticeWithdrawsIt(t *testing.T) {
	r := newRig(policy.Document{IdleStopDays: 30})
	r.use(r.runningApp("app_1"))
	ctx := context.Background()

	r.clock.Advance(23 * day)
	r.pass.Once(ctx)
	require.NotNil(t, r.store.apps["app_1"].StopNoticedAt)

	// Somebody opens it.
	r.clock.Advance(day)
	r.store.apps["app_1"].LastActivity = r.clock.Now()
	r.clock.Advance(7 * day)
	r.pass.Once(ctx)
	assert.Nil(t, r.store.apps["app_1"].StopNoticedAt, "the notice was answered")
	assert.Equal(t, state.StateRunning, r.store.apps["app_1"].DesiredState)

	r.clock.Advance(16 * day)
	r.pass.Once(ctx)
	assert.NotNil(t, r.store.apps["app_1"].StopNoticedAt, "a new stretch, a new notice")
	assert.Len(t, r.notes.sent, 2)
}

// TestR396_OnlyARunningAppIsStopped asserts R-396 and R-151: a failed app and
// an app its owner stopped are not stopped for being idle, and are not sent a
// notice about it.
func TestR396_OnlyARunningAppIsStopped(t *testing.T) {
	r := newRig(policy.Document{IdleStopDays: 30})
	failed := r.runningApp("app_failed")
	failed.State = state.StateFailed
	stopped := r.runningApp("app_stopped")
	stopped.DesiredState = state.StateStopped
	r.use(failed, stopped)

	r.clock.Advance(60 * day)
	r.pass.Once(context.Background())
	r.clock.Advance(8 * day)
	r.pass.Once(context.Background())

	assert.Empty(t, r.notes.sent)
	assert.False(t, r.store.apps["app_failed"].StoppedForIdle)
	assert.False(t, r.store.apps["app_stopped"].StoppedForIdle)
}

// TestR293_AnAppsOwnSettingWins asserts R-293 and R-397: an app may turn the install's
// default off, or set its own number.
func TestR293_AnAppsOwnSettingWins(t *testing.T) {
	r := newRig(policy.Document{IdleStopDays: 30})
	r.use(r.runningApp("app_off"), r.runningApp("app_longer"))
	off, longer := 0, 60
	r.store.own["app_off"] = [2]*int{&off, nil}
	r.store.own["app_longer"] = [2]*int{&longer, nil}

	r.clock.Advance(40 * day)
	r.pass.Once(context.Background())
	assert.Empty(t, r.notes.sent, "neither is owed anything at 40 days")

	r.clock.Advance(20 * day)
	r.pass.Once(context.Background())
	require.Len(t, r.notes.sent, 1)
	assert.Equal(t, "app_longer", r.notes.sent[0].AppID)
}

// TestR398_AnIdleAppIsDeletedAsHostPolicySays asserts R-393 and R-398: the
// deletion counts from the last activity, waits out its notice, and leaves
// the storage question to host policy.
func TestR398_AnIdleAppIsDeletedAsHostPolicySays(t *testing.T) {
	r := newRig(policy.Document{IdleStopDays: 30, IdleDeleteDays: 90, RequireBackupBeforeDestroy: true})
	r.use(r.runningApp("app_1"))
	ctx := context.Background()

	for range 82 {
		r.clock.Advance(day)
		r.pass.Once(ctx)
	}
	assert.Equal(t, state.StateStopped, r.store.apps["app_1"].DesiredState, "stopped at 30 days")
	assert.Empty(t, r.deleter.reqs)

	r.clock.Advance(day)
	r.pass.Once(ctx)
	note := r.notes.sent[len(r.notes.sent)-1]
	assert.Contains(t, note.Subject, "will be deleted on April 1, 2026")
	assert.Contains(t, note.Body, "backs it up first")

	r.clock.Advance(7 * day)
	r.pass.Once(ctx)
	require.Len(t, r.deleter.reqs, 1)
	req := r.deleter.reqs[0]
	assert.Equal(t, appdelete.StoragePolicy, req.Storage, "host policy answers R-204's question")
	assert.Equal(t, "idle", req.Reason)
	assert.Equal(t, audit.KindSystem, req.Actor.PrincipalKind)
	assert.NotContains(t, r.store.apps, "app_1")
	assert.Contains(t, r.notes.sent[len(r.notes.sent)-1].Body, "bak_1")
}

// TestR398_ADeleteThatFailsWaitsAnotherNotice asserts R-398: an app whose
// backup fails is not deleted, its owner hears why, and the next attempt is
// a notice's length away rather than every pass.
func TestR398_ADeleteThatFailsWaitsAnotherNotice(t *testing.T) {
	r := newRig(policy.Document{IdleDeleteDays: 10})
	r.use(r.runningApp("app_1"))
	// What appdelete returns when the backup fails: a message and remedy for
	// a person, wrapping a cause for the log.
	r.deleter.err = errs.Wrap(errs.StateInvalid,
		"Pando could not back up this app's storage, so it has not been deleted.",
		errors.New("dial tcp 10.0.0.7:9000: connection refused")).
		WithRemedy("Fix the problem and try again.")
	ctx := context.Background()

	r.clock.Advance(3 * day)
	r.pass.Once(ctx)
	r.clock.Advance(7 * day)
	r.pass.Once(ctx)
	require.Len(t, r.deleter.reqs, 1)
	last := r.notes.sent[len(r.notes.sent)-1]
	assert.True(t, strings.HasPrefix(last.Subject, "Pando did not delete"))
	assert.Contains(t, last.Body, "could not back up this app's storage, so it has not been deleted. Fix the problem and try again.")
	assert.NotContains(t, last.Body, "STATE_INVALID", "the code is for machines")
	assert.NotContains(t, last.Body, "10.0.0.7", "the cause is for the log, not a notification that may go by email")

	sent := len(r.notes.sent)
	for range 6 {
		r.clock.Advance(day)
		r.pass.Once(ctx)
	}
	assert.Len(t, r.deleter.reqs, 1, "no second attempt inside the new notice")
	assert.Len(t, r.notes.sent, sent, "and no message every pass")

	r.deleter.err = nil
	r.clock.Advance(day)
	r.pass.Once(ctx)
	assert.Len(t, r.deleter.reqs, 2)
	assert.NotContains(t, r.store.apps, "app_1")
}

// TestR398_DeleteFailureNoticeUsesTheErrorEnvelope asserts R-398 and R-105:
// the owner gets the explanation and remedy, without internal error details.
func TestR398_DeleteFailureNoticeUsesTheErrorEnvelope(t *testing.T) {
	for _, remedy := range []string{"", "Check the backup settings before trying again."} {
		t.Run(remedy, func(t *testing.T) {
			r := newRig(policy.Document{IdleDeleteDays: 10})
			r.use(r.runningApp("app_1"))
			message := "Pando could not back up this app's storage, so it has not been deleted."
			cause := "storage backend: connection refused at internal-backup:9000"
			r.deleter.err = errs.Wrap(errs.StateInvalid, message, errors.New(cause)).WithRemedy(remedy)
			ctx := context.Background()

			r.clock.Advance(3 * day)
			r.pass.Once(ctx)
			r.clock.Advance(7 * day)
			r.pass.Once(ctx)

			require.Len(t, r.deleter.reqs, 1)
			require.Len(t, r.notes.sent, 2)
			body := r.notes.sent[1].Body
			assert.Contains(t, body, message)
			if remedy != "" {
				assert.Contains(t, body, remedy)
			}
			assert.NotContains(t, body, string(errs.StateInvalid))
			assert.NotContains(t, body, cause)
			assert.Contains(t, body, "Pando tries again on January 18, 2026")
		})
	}
}

// TestR393_OffByDefault asserts R-393 and R-270: with nothing set, nothing
// is owed, however long an app sits.
func TestR393_OffByDefault(t *testing.T) {
	r := newRig(policy.Default())
	r.use(r.runningApp("app_1"))

	r.clock.Advance(3650 * day)
	r.pass.Once(context.Background())
	assert.Empty(t, r.notes.sent)
	assert.Empty(t, r.audit.events)
	assert.Equal(t, idle.NoticeDays, r.store.defaults.NoticeDays)
}
