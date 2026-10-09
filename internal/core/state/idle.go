package state

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// Idle apps (R-393 – R-398, issue #131).
//
// An app's last activity is the latest of its creation, a request the proxy
// let through to it, a deploy and a start. The proxy writes requests here in
// batches (RecordActivity); deploys and starts are written by triggers in
// migration 69, so no path that deploys or starts an app can forget to.

// Activity is the store half of idle apps.
type Activity struct{ db *DB }

func NewActivity(db *DB) *Activity { return &Activity{db: db} }

// RecordActivity writes when each app was last used, one statement for the
// batch. GREATEST, because two replicas flush independently and an older
// flush landing second must not move an app's clock backwards.
func (a *Activity) RecordActivity(ctx context.Context, seen map[string]time.Time) error {
	if len(seen) == 0 {
		return nil
	}
	ids := make([]string, 0, len(seen))
	ats := make([]time.Time, 0, len(seen))
	for id, at := range seen {
		ids = append(ids, id)
		ats = append(ats, at)
	}
	// Only apps that still exist: an app deleted since its last request has
	// no row to hang activity on, and the batch must not fail for it.
	_, err := a.db.Exec(ctx, `
		INSERT INTO app_activity (app_id, last_activity_at)
		SELECT s.app_id, s.at
		FROM unnest($1::text[], $2::timestamptz[]) AS s(app_id, at)
		JOIN apps ON apps.id = s.app_id
		ON CONFLICT (app_id) DO UPDATE
		    SET last_activity_at = GREATEST(app_activity.last_activity_at, EXCLUDED.last_activity_at)`,
		ids, ats)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record app activity.", err)
	}
	return nil
}

// IdleSettings is an app's own idle settings (R-397). Nil is the install's;
// zero is off.
type IdleSettings struct {
	StopDays   *int `json:"stop_days"`
	DeleteDays *int `json:"delete_days"`
}

// IdleStatus is an app's idle settings and where its clock stands.
type IdleStatus struct {
	IdleSettings
	DesiredState    string
	State           string
	StoppedForIdle  bool
	LastActivity    time.Time
	StopNoticedAt   *time.Time
	DeleteNoticedAt *time.Time
}

// IdleStatus reads an app's idle status.
func (a *Activity) IdleStatus(ctx context.Context, appID string) (IdleStatus, error) {
	var s IdleStatus
	err := a.db.QueryRow(ctx, `
		SELECT a.idle_stop_days, a.idle_delete_days, a.desired_state, a.state, a.stopped_for_idle,
		       GREATEST(a.created_at, coalesce(act.last_activity_at, a.created_at)),
		       a.idle_stop_noticed_at, a.idle_delete_noticed_at
		FROM apps a LEFT JOIN app_activity act ON act.app_id = a.id
		WHERE a.id = $1 AND a.deleted_at IS NULL`, appID).
		Scan(&s.StopDays, &s.DeleteDays, &s.DesiredState, &s.State, &s.StoppedForIdle,
			&s.LastActivity, &s.StopNoticedAt, &s.DeleteNoticedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return IdleStatus{}, errs.New(errs.NotFound, "There is no such app.")
	}
	if err != nil {
		return IdleStatus{}, errs.Wrap(errs.Internal, "Could not read the app's idle settings.", err)
	}
	return s, nil
}

// SetIdleSettings replaces an app's own settings. Any notice already given is
// withdrawn, because it was given under the old settings; the next pass gives
// a new one if the new settings call for it.
func (a *Activity) SetIdleSettings(ctx context.Context, appID string, s IdleSettings) error {
	_, err := a.db.Exec(ctx, `
		UPDATE apps SET idle_stop_days = $2, idle_delete_days = $3,
		       idle_stop_noticed_at = NULL, idle_delete_noticed_at = NULL, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, appID, s.StopDays, s.DeleteDays)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not save the app's idle settings.", err)
	}
	return nil
}

// IdleDefaults is host policy's part of the question: the install's days,
// and the notice every action waits out.
type IdleDefaults struct {
	StopDays   int
	DeleteDays int
	NoticeDays int
}

// IdleApp is an app the idle pass may have something to do for.
type IdleApp struct {
	AppID          string
	Name           string
	OwnerUserID    string
	State          string
	DesiredState   string
	StoppedForIdle bool

	// StopDays and DeleteDays are in force for this app: its own, or the
	// install's. Zero is off.
	StopDays   int
	DeleteDays int

	LastActivity    time.Time
	StopNoticedAt   *time.Time
	DeleteNoticedAt *time.Time
}

// IdleCandidates lists the apps the idle pass could act on now: those idle
// long enough to be owed a notice for something in force, and those holding a
// notice, which may be stale. Every other app would be read and left alone.
func (a *Activity) IdleCandidates(ctx context.Context, d IdleDefaults, now time.Time) ([]IdleApp, error) {
	rows, err := a.db.Query(ctx, `
		WITH eff AS (
		    SELECT a.id, a.name, coalesce(a.owner_user_id, '') AS owner, a.state, a.desired_state,
		           a.stopped_for_idle, a.idle_stop_noticed_at, a.idle_delete_noticed_at,
		           coalesce(a.idle_stop_days, $1) AS stop_days,
		           coalesce(a.idle_delete_days, $2) AS delete_days,
		           GREATEST(a.created_at, coalesce(act.last_activity_at, a.created_at)) AS last_activity
		    FROM apps a
		    LEFT JOIN app_activity act ON act.app_id = a.id
		    WHERE a.deleted_at IS NULL
		)
		SELECT id, name, owner, state, desired_state, stopped_for_idle, stop_days, delete_days,
		       last_activity, idle_stop_noticed_at, idle_delete_noticed_at
		FROM eff
		WHERE (stop_days > 0 AND last_activity <= $4::timestamptz - make_interval(days => GREATEST(stop_days - $3::int, 1)))
		   OR (delete_days > 0 AND last_activity <= $4::timestamptz - make_interval(days => GREATEST(delete_days - $3::int, 1)))
		   OR idle_stop_noticed_at IS NOT NULL
		   OR idle_delete_noticed_at IS NOT NULL
		ORDER BY id`,
		d.StopDays, d.DeleteDays, d.NoticeDays, now)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list idle apps.", err)
	}
	defer rows.Close()

	var out []IdleApp
	for rows.Next() {
		var app IdleApp
		if err := rows.Scan(&app.AppID, &app.Name, &app.OwnerUserID, &app.State, &app.DesiredState,
			&app.StoppedForIdle, &app.StopDays, &app.DeleteDays, &app.LastActivity,
			&app.StopNoticedAt, &app.DeleteNoticedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list idle apps.", err)
		}
		out = append(out, app)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list idle apps.", err)
	}
	return out, nil
}

// IdleNotice is which action a notice is for.
type IdleNotice string

const (
	IdleNoticeStop   IdleNotice = "stop"
	IdleNoticeDelete IdleNotice = "delete"
)

// SetIdleNotice records when the owner was told, or clears it with nil.
func (a *Activity) SetIdleNotice(ctx context.Context, appID string, which IdleNotice, at *time.Time) error {
	column := "idle_stop_noticed_at"
	if which == IdleNoticeDelete {
		column = "idle_delete_noticed_at"
	}
	_, err := a.db.Exec(ctx, `UPDATE apps SET `+column+` = $2 WHERE id = $1`, appID, at)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the idle notice.", err)
	}
	return nil
}

// StopForIdle stops an app and records why, in one write: the proxy cache is
// emptied by the desired state changing, and the page a visitor sees must say
// why in the same moment (R-396). Only an app that is meant to be running, so
// an app its owner stopped since the pass read it is left as they left it.
func (a *Activity) StopForIdle(ctx context.Context, appID string) (bool, error) {
	tag, err := a.db.Exec(ctx, `
		UPDATE apps SET desired_state = 'stopped', stopped_for_idle = true,
		       idle_stop_noticed_at = NULL, updated_at = now(), `+dueAgain+`
		WHERE id = $1 AND deleted_at IS NULL AND desired_state = 'running'`, appID)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not stop the app.", err)
	}
	return tag.RowsAffected() == 1, nil
}
