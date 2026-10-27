package state

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// Standbys is the state of each app's warm standby (R-419, R-420, migration
// 000087).
type Standbys struct{ db *DB }

// NewStandbys returns the standby store.
func NewStandbys(db *DB) *Standbys { return &Standbys{db: db} }

// Standby is one app's standby as Pando last left it.
type Standby struct {
	AppID      string `json:"app_id"`
	RuntimeRef string `json:"runtime_ref,omitempty"`
	// Host is the runtime's name for where the standby is; empty before the
	// runtime has made one.
	Host         string     `json:"host,omitempty"`
	SpecID       string     `json:"spec_id,omitempty"`
	Volumes      int        `json:"volumes"`
	Copies       int        `json:"copies"`
	ShippedAt    *time.Time `json:"shipped_at,omitempty"`
	Unreplicated []string   `json:"unreplicated,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	StaleHost    string     `json:"stale_host,omitempty"`
	PromotedAt   *time.Time `json:"promoted_at,omitempty"`
}

// StandbyApp is an app the standby pass looks at: a live app with a pinned
// spec, or a deleted one that still has a standby row to clear.
type StandbyApp struct {
	AppID        string
	State        string
	DesiredState string
	PinnedSpecID string
	Deleted      bool
	// Standby is nil when the app has none recorded.
	Standby *Standby
}

const standbyColumns = `s.app_id, s.runtime_ref, s.host, coalesce(s.spec_id, ''), s.volumes, s.copies, s.shipped_at,
	s.unreplicated, s.last_error, s.stale_host, s.promoted_at`

func scanStandby(row pgx.Row, extra ...any) (Standby, error) {
	var s Standby
	dest := make([]any, 0, len(extra)+10)
	dest = append(dest, extra...)
	dest = append(dest, &s.AppID, &s.RuntimeRef, &s.Host, &s.SpecID, &s.Volumes, &s.Copies, &s.ShippedAt,
		&s.Unreplicated, &s.LastError, &s.StaleHost, &s.PromotedAt)
	err := row.Scan(dest...)
	return s, err
}

// Candidates lists every live app with a pinned spec, with its standby if it
// has one, and every deleted app that still has a standby row.
func (s *Standbys) Candidates(ctx context.Context) ([]StandbyApp, error) {
	rows, err := s.db.Query(ctx, `
		SELECT a.id, a.state, a.desired_state, coalesce(a.pinned_spec_id, ''), a.deleted_at IS NOT NULL,
		       s.app_id IS NOT NULL,
		       coalesce(s.app_id, ''), coalesce(s.runtime_ref, ''), coalesce(s.host, ''), coalesce(s.spec_id, ''), coalesce(s.volumes, 0),
		       coalesce(s.copies, 0), s.shipped_at, coalesce(s.unreplicated, '{}'), coalesce(s.last_error, ''),
		       coalesce(s.stale_host, ''), s.promoted_at
		FROM apps a
		LEFT JOIN app_standbys s ON s.app_id = a.id
		WHERE (a.deleted_at IS NULL AND a.pinned_spec_id IS NOT NULL) OR s.app_id IS NOT NULL
		ORDER BY a.id`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list the apps whose standbys to keep.", err)
	}
	defer rows.Close()
	var out []StandbyApp
	for rows.Next() {
		var app StandbyApp
		var has bool
		sb, err := scanStandby(rows, &app.AppID, &app.State, &app.DesiredState, &app.PinnedSpecID, &app.Deleted, &has)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list the apps whose standbys to keep.", err)
		}
		if has {
			app.Standby = &sb
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

// Get reads one app's standby.
func (s *Standbys) Get(ctx context.Context, appID string) (Standby, bool, error) {
	sb, err := scanStandby(s.db.QueryRow(ctx, `SELECT `+standbyColumns+` FROM app_standbys s WHERE s.app_id = $1`, appID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Standby{}, false, nil
	}
	if err != nil {
		return Standby{}, false, errs.Wrap(errs.Internal, "Could not read the app's standby.", err)
	}
	return sb, true, nil
}

// Save writes one app's standby.
func (s *Standbys) Save(ctx context.Context, sb Standby) error {
	if sb.Unreplicated == nil {
		sb.Unreplicated = []string{}
	}
	var specID *string
	if sb.SpecID != "" {
		specID = &sb.SpecID
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO app_standbys (app_id, host, spec_id, volumes, copies, shipped_at, unreplicated,
		                          last_error, stale_host, promoted_at, runtime_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (app_id) DO UPDATE SET
		    host = $2, spec_id = $3, volumes = $4, copies = $5, shipped_at = $6, unreplicated = $7,
		    last_error = $8, stale_host = $9, promoted_at = $10, runtime_ref = $11, updated_at = now()`,
		sb.AppID, sb.Host, specID, sb.Volumes, sb.Copies, sb.ShippedAt, sb.Unreplicated,
		sb.LastError, sb.StaleHost, sb.PromotedAt, sb.RuntimeRef)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the app's standby.", err)
	}
	return nil
}

// Delete forgets one app's standby.
func (s *Standbys) Delete(ctx context.Context, appID string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM app_standbys WHERE app_id = $1`, appID); err != nil {
		return errs.Wrap(errs.Internal, "Could not forget the app's standby.", err)
	}
	return nil
}

// AppState is a live app's state: false when there is no such app.
func (s *Standbys) AppState(ctx context.Context, appID string) (string, bool, error) {
	var st string
	err := s.db.QueryRow(ctx, `SELECT state FROM apps WHERE id = $1 AND deleted_at IS NULL`, appID).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errs.Wrap(errs.Internal, "Could not read the app.", err)
	}
	return st, true, nil
}
