// Package idle stops, and later deletes, apps nobody uses (R-393 – R-398,
// issue #131).
//
// Two halves. The Recorder takes the proxy's word that an app was used and
// writes it at most once a minute (R-394). The Pass runs on the leader, reads
// which apps have been idle long enough to be owed something, and does it: a
// notice, then 7 days later at the earliest a stop or a deletion (R-395).
//
// Nothing here starts an app. An app stopped for being idle stays stopped
// until somebody starts it (R-396), and starting it clears the stop and every
// notice in the same write (migration 69), so this package never has to.
package idle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/appdelete"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// NoticeDays is how long before a stop or a deletion the owner is told, and
// so the least time between the two (R-395 [P]).
const NoticeDays = 7

// DefaultEvery is how often the pass runs. Days are the unit of every
// setting, so an hour late is nothing, and an hour is often enough that a
// notice lands the day it is owed.
const DefaultEvery = time.Hour

const day = 24 * time.Hour

// Store is the state the pass reads and writes.
type Store interface {
	IdleCandidates(ctx context.Context, d state.IdleDefaults, now time.Time) ([]state.IdleApp, error)
	SetIdleNotice(ctx context.Context, appID string, which state.IdleNotice, at *time.Time) error
	StopForIdle(ctx context.Context, appID string) (bool, error)
}

// AppReader reads the app a deletion is for.
type AppReader interface {
	ByID(ctx context.Context, appID string) (state.App, bool, error)
}

// Deleter deletes an app. *appdelete.Service is one.
type Deleter interface {
	Delete(ctx context.Context, app state.App, req appdelete.Request) (appdelete.Result, error)
}

// PolicyLoader reads host policy.
type PolicyLoader interface {
	Load(ctx context.Context) (policy.Document, error)
}

// Notifier tells people things. subscription.Router is one.
type Notifier interface {
	Notify(ctx context.Context, n api.Notification) error
}

// Auditor writes audit events.
type Auditor interface {
	Write(ctx context.Context, e audit.Event) error
}

// Pass is the idle job.
type Pass struct {
	Store   Store
	Apps    AppReader
	Deleter Deleter
	Policy  PolicyLoader

	// Notifier and Auditor are optional, and nil says nothing. Production
	// wires both: R-395 is a promise that nothing goes without warning.
	Notifier Notifier
	Auditor  Auditor

	Clock  clock.Clock
	Logger *zap.Logger

	// Every is how often Run runs a pass. Zero is DefaultEvery.
	Every time.Duration
}

// Run passes until ctx is canceled.
func (p *Pass) Run(ctx context.Context) {
	every := p.Every
	if every <= 0 {
		every = DefaultEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		p.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Once runs one pass.
func (p *Pass) Once(ctx context.Context) {
	doc, err := p.Policy.Load(ctx)
	if err != nil {
		p.logger().Warn("could not read host policy for the idle pass", zap.Error(err))
		return
	}
	now := p.now()
	apps, err := p.Store.IdleCandidates(ctx, state.IdleDefaults{
		StopDays: doc.IdleStopDays, DeleteDays: doc.IdleDeleteDays, NoticeDays: NoticeDays,
	}, now)
	if err != nil {
		p.logger().Warn("could not list idle apps", zap.Error(err))
		return
	}
	for _, app := range apps {
		if ctx.Err() != nil {
			return
		}
		p.visit(ctx, doc, app, now)
	}
}

// visit decides one app. Deletion first: an app that is going is not also
// owed a stop.
func (p *Pass) visit(ctx context.Context, doc policy.Document, app state.IdleApp, now time.Time) {
	idleFor := now.Sub(app.LastActivity)
	stopApplies := app.StopDays > 0 && app.DesiredState == state.StateRunning && app.State != state.StateFailed
	app = p.withdrawStale(ctx, app, stopApplies)

	if app.DeleteDays > 0 {
		switch owed(idleFor, app.DeleteDays, app.DeleteNoticedAt, now) {
		case stepNotice:
			p.notice(ctx, doc, app, state.IdleNoticeDelete, app.DeleteDays, now)
			return
		case stepAct:
			p.delete(ctx, doc, app, idleFor, now)
			return
		}
	}

	if stopApplies {
		switch owed(idleFor, app.StopDays, app.StopNoticedAt, now) {
		case stepNotice:
			p.notice(ctx, doc, app, state.IdleNoticeStop, app.StopDays, now)
		case stepAct:
			p.stop(ctx, app, idleFor)
		}
	}
}

// withdrawStale withdraws the notices no longer owed. A notice older than the
// app's last activity was answered: somebody used it. One for an action that
// no longer applies — the setting was turned off, or the owner stopped the app
// themselves — is withdrawn too, so it does not hold the app in the list of
// candidates forever.
func (p *Pass) withdrawStale(ctx context.Context, app state.IdleApp, stopApplies bool) state.IdleApp {
	if app.StopNoticedAt != nil && (app.StopNoticedAt.Before(app.LastActivity) || !stopApplies) {
		p.clearNotice(ctx, app.AppID, state.IdleNoticeStop)
		app.StopNoticedAt = nil
	}
	if app.DeleteNoticedAt != nil && (app.DeleteNoticedAt.Before(app.LastActivity) || app.DeleteDays == 0) {
		p.clearNotice(ctx, app.AppID, state.IdleNoticeDelete)
		app.DeleteNoticedAt = nil
	}
	return app
}

// step is what an app is owed under one setting.
type step int

const (
	stepNone step = iota
	stepNotice
	stepAct
)

// owed is what an app idle for idleFor is owed under a setting of n days:
// a notice once it is within the notice of the setting, and the action once
// it is past the setting and the notice has run its full length (R-395).
func owed(idleFor time.Duration, n int, noticedAt *time.Time, now time.Time) step {
	switch {
	case idleFor < noticeAfter(n):
		return stepNone
	case noticedAt == nil:
		return stepNotice
	case idleFor >= days(n) && now.Sub(*noticedAt) >= days(NoticeDays):
		return stepAct
	}
	return stepNone
}

// noticeAfter is how long an app is idle before a notice is owed: the
// setting less the notice, and never before a whole day without activity, so
// an app set to stop after three days is not warned after every request.
func noticeAfter(setting int) time.Duration {
	return days(max(setting-NoticeDays, 1))
}

func days(n int) time.Duration { return time.Duration(n) * day }

// ActionAt is when an app idle since lastActivity, given notice at noticedAt,
// is stopped or deleted under a setting of n days: n days after its last
// activity, and never sooner than NoticeDays after the notice.
func ActionAt(lastActivity, noticedAt time.Time, n int) time.Time {
	at := lastActivity.Add(days(n))
	if earliest := noticedAt.Add(days(NoticeDays)); at.Before(earliest) {
		return earliest
	}
	return at
}

func (p *Pass) notice(ctx context.Context, doc policy.Document, app state.IdleApp, which state.IdleNotice, setting int, now time.Time) {
	if err := p.Store.SetIdleNotice(ctx, app.AppID, which, &now); err != nil {
		p.logger().Warn("could not record an idle notice", zap.String("app_id", app.AppID), zap.Error(err))
		return
	}
	at := ActionAt(app.LastActivity, now, setting)

	var subject, body string
	if which == state.IdleNoticeDelete {
		subject = fmt.Sprintf("%s will be deleted on %s", app.Name, date(at))
		body = fmt.Sprintf("Nobody has used %s since %s. Pando will delete it on %s unless somebody uses, starts or deploys it before then. %s "+
			"To keep it, change its idle settings in Pando.",
			app.Name, date(app.LastActivity), date(at), storageSentence(doc))
	} else {
		subject = fmt.Sprintf("%s will be stopped on %s", app.Name, date(at))
		body = fmt.Sprintf("Nobody has used %s since %s. Pando will stop it on %s unless somebody uses or deploys it before then. "+
			"A stopped app keeps its data, and anybody who manages it can start it again from its page in Pando. "+
			"To keep it running, change its idle settings in Pando.",
			app.Name, date(app.LastActivity), date(at))
	}
	p.notify(ctx, app, subject, body)
	p.audit(ctx, "app.idle.notice", app.AppID, map[string]any{
		"action":        string(which),
		"days":          setting,
		"last_activity": app.LastActivity,
		"action_at":     at,
	})
}

func storageSentence(doc policy.Document) string {
	if doc.RequireBackupBeforeDestroy {
		return "If it has storage, Pando backs it up first and keeps the backup until somebody discards it."
	}
	return "If it has storage, the storage is deleted with it."
}

func (p *Pass) stop(ctx context.Context, app state.IdleApp, idleFor time.Duration) {
	stopped, err := p.Store.StopForIdle(ctx, app.AppID)
	if err != nil {
		p.logger().Warn("could not stop an idle app", zap.String("app_id", app.AppID), zap.Error(err))
		return
	}
	if !stopped {
		// Stopped or deleted by somebody since the pass read it; theirs is
		// the newer decision.
		return
	}
	p.audit(ctx, "app.idle.stopped", app.AppID, map[string]any{
		"days":          app.StopDays,
		"last_activity": app.LastActivity,
	})
	p.notify(ctx, app, app.Name+" was stopped",
		fmt.Sprintf("Nobody had used %s since %s, so Pando stopped it. Its data is kept. "+
			"Anybody who manages it can start it again from its page in Pando.",
			app.Name, date(app.LastActivity)))
	p.logger().Info("stopped an idle app", zap.String("app_id", app.AppID),
		zap.Int("idle_days", int(idleFor/day)))
}

func (p *Pass) delete(ctx context.Context, doc policy.Document, app state.IdleApp, idleFor time.Duration, now time.Time) {
	current, found, err := p.Apps.ByID(ctx, app.AppID)
	if err != nil {
		p.logger().Warn("could not read an idle app to delete it", zap.String("app_id", app.AppID), zap.Error(err))
		return
	}
	if !found {
		return
	}

	result, err := p.Deleter.Delete(ctx, current, appdelete.Request{
		Storage: appdelete.StoragePolicy,
		Actor:   audit.Event{PrincipalKind: audit.KindSystem, PrincipalID: "idle"},
		Reason:  "idle",
	})
	if err != nil {
		// Not deleted, so the data is where it was. The notice starts again
		// from now: the owner hears why, and the next attempt is a notice's
		// length away rather than an hour, which would repeat the message
		// every pass (R-398).
		p.logger().Warn("could not delete an idle app", zap.String("app_id", app.AppID), zap.Error(err))
		if err := p.Store.SetIdleNotice(ctx, app.AppID, state.IdleNoticeDelete, &now); err != nil {
			p.logger().Warn("could not record an idle notice", zap.String("app_id", app.AppID), zap.Error(err))
		}
		p.notify(ctx, app, "Pando did not delete "+app.Name,
			fmt.Sprintf("Nobody has used %s since %s, and Pando was to delete it today, but could not. %s "+
				"Pando tries again on %s unless somebody uses, starts or deploys it before then.",
				app.Name, date(app.LastActivity), readable(err), date(now.Add(days(NoticeDays)))))
		return
	}

	body := fmt.Sprintf("Nobody had used %s since %s, so Pando deleted it.", app.Name, date(app.LastActivity))
	if result.BackupID != "" {
		body += fmt.Sprintf(" A final backup of its storage, %s, is kept until somebody discards it.", result.BackupID)
	} else if result.VolumesDiscarded > 0 {
		body += " Its storage was deleted with it."
	}
	p.notify(ctx, app, app.Name+" was deleted", body)
	p.logger().Info("deleted an idle app", zap.String("app_id", app.AppID),
		zap.Int("idle_days", int(idleFor/day)), zap.Bool("backup_required", doc.RequireBackupBeforeDestroy))
}

// readable is what a person is told about a failed deletion: the error's
// message and remedy, written for them (R-105). Never the code or a wrapped
// cause, which are for the log line above it — this can go out by email or to
// a chat channel.
func readable(err error) string {
	var e *errs.Error
	if !errors.As(err, &e) {
		return "Its storage could not be backed up."
	}
	if e.Remedy == "" {
		return e.Message
	}
	return e.Message + " " + e.Remedy
}

func (p *Pass) clearNotice(ctx context.Context, appID string, which state.IdleNotice) {
	if err := p.Store.SetIdleNotice(ctx, appID, which, nil); err != nil {
		p.logger().Warn("could not withdraw an idle notice", zap.String("app_id", appID), zap.Error(err))
	}
}

func (p *Pass) notify(ctx context.Context, app state.IdleApp, subject, body string) {
	if p.Notifier == nil || app.OwnerUserID == "" {
		return
	}
	if err := p.Notifier.Notify(ctx, api.Notification{
		Kind:       api.NotifyAppIdle,
		AppID:      app.AppID,
		Recipients: []api.Recipient{{UserID: app.OwnerUserID}},
		Subject:    subject,
		Body:       body,
	}); err != nil {
		p.logger().Warn("could not tell an app's owner about an idle action",
			zap.String("app_id", app.AppID), zap.Error(err))
	}
}

func (p *Pass) audit(ctx context.Context, action, appID string, detail map[string]any) {
	if p.Auditor == nil {
		return
	}
	if err := p.Auditor.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem, PrincipalID: "idle",
		Action: action, AppID: appID, TargetKind: "app", TargetID: appID, Detail: detail,
	}); err != nil {
		p.logger().Error("audit write failed", zap.String("action", action), zap.Error(err))
	}
}

func (p *Pass) now() time.Time {
	if p.Clock == nil {
		return time.Now().UTC()
	}
	return p.Clock.Now()
}

func (p *Pass) logger() *zap.Logger {
	if p.Logger == nil {
		return zap.NewNop()
	}
	return p.Logger
}

// date is a day as a notice names it. UTC, like every time in Pando.
func date(t time.Time) string { return t.UTC().Format("January 2, 2006") }
