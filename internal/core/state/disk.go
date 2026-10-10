package state

import (
	"context"
	"time"

	"github.com/trypando/pando/internal/errs"
)

// Disk is the state the disk pass reads and writes (R-403, issue #130).
type Disk struct{ db *DB }

// NewDisk returns the disk pass's store.
func NewDisk(db *DB) *Disk { return &Disk{db: db} }

// DiskApp is a running app the disk pass measures: its limit and runtime from
// its pinned spec, and what Pando has already said about it.
type DiskApp struct {
	AppID       string
	Name        string
	OwnerUserID string
	RuntimeRef  string
	DiskBytes   int64
	WarnedAt    *time.Time
	OverAt      *time.Time
	// StartedAt is when the app was last started. Every write the pass makes
	// requires it unchanged, so nothing read before a restart lands after it.
	StartedAt *time.Time
}

// DiskMark is one of the two marks the pass keeps on an app.
type DiskMark string

const (
	// DiskWarned is when the owner was told the app was near its limit.
	DiskWarned DiskMark = "warned"
	// DiskOver is the first reading over the limit.
	DiskOver DiskMark = "over"
)

// DiskCandidates lists every app meant to be running with a pinned spec. A
// failed app is left out: it is not running, and a failed app stays failed
// (R-151).
func (d *Disk) DiskCandidates(ctx context.Context) ([]DiskApp, error) {
	rows, err := d.db.Query(ctx, `
		SELECT a.id, a.name, coalesce(a.owner_user_id, ''),
		       coalesce(r.body->'runtime'->>'adapter_ref', ''),
		       coalesce((r.body->'resources'->>'disk_bytes')::bigint, 0),
		       a.disk_warned_at, a.disk_over_at, a.started_at
		FROM apps a
		JOIN spec_revisions r ON r.id = a.pinned_spec_id
		WHERE a.deleted_at IS NULL AND a.desired_state = 'running' AND a.state <> 'failed'
		ORDER BY a.id`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list the apps to measure.", err)
	}
	defer rows.Close()
	var out []DiskApp
	for rows.Next() {
		var app DiskApp
		if err := rows.Scan(&app.AppID, &app.Name, &app.OwnerUserID, &app.RuntimeRef, &app.DiskBytes,
			&app.WarnedAt, &app.OverAt, &app.StartedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list the apps to measure.", err)
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

// SetDiskMark records, or with nil clears, one of an app's disk marks. Only
// on the run of the app the pass read: false, and nothing written, when it has
// been stopped, deleted or restarted since (CodeRabbit on #180).
func (d *Disk) SetDiskMark(ctx context.Context, app DiskApp, which DiskMark, at *time.Time) (bool, error) {
	column := "disk_warned_at"
	if which == DiskOver {
		column = "disk_over_at"
	}
	tag, err := d.db.Exec(ctx, `
		UPDATE apps SET `+column+` = $2
		WHERE id = $1 AND deleted_at IS NULL AND desired_state = 'running'
		  AND started_at IS NOT DISTINCT FROM $3`, app.AppID, at, app.StartedAt)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not record the app's disk use.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// StopForDisk stops an app and records why, in one write, as StopForIdle
// does: the visitor page says why in the same moment the app stops. Only the
// run of the app the pass read, still holding the over-limit mark the pass
// read, and not failed (R-151): one its owner stopped or restarted since is
// left as they left it, so a stop always follows two readings over in the
// same run (R-403, CodeRabbit on #180).
func (d *Disk) StopForDisk(ctx context.Context, app DiskApp) (bool, error) {
	if app.OverAt == nil {
		return false, nil
	}
	tag, err := d.db.Exec(ctx, `
		UPDATE apps SET desired_state = 'stopped', stopped_for_disk = true,
		       disk_over_at = NULL, updated_at = now(), `+dueAgain+`
		WHERE id = $1 AND deleted_at IS NULL AND desired_state = 'running' AND state <> 'failed'
		  AND started_at IS NOT DISTINCT FROM $2 AND disk_over_at = $3`, app.AppID, app.StartedAt, app.OverAt)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not stop the app.", err)
	}
	return tag.RowsAffected() == 1, nil
}
