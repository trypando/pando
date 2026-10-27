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

	// Home is the runtime's name for where the app runs, as it last said
	// (api.StandbyHandle.Home, api.PromoteResult.Host): the liveness pass's
	// record of which apps a lost host held (R-421). Empty until said.
	Home string `json:"home,omitempty"`
	// LastErrorCode and LastErrorRemedy are LastError's envelope code and
	// remedy. CAPACITY_WOULD_OVERSUBSCRIBE is the no-room notice (R-422).
	LastErrorCode   string `json:"last_error_code,omitempty"`
	LastErrorRemedy string `json:"last_error_remedy,omitempty"`

	// PromotingAt is when a promotion of the standby began and has not yet
	// ended (migration 000091). It is written ahead with Home set to the
	// standby's host (R-153, R-419): while it is set the standby pass and the
	// reconciler leave the app alone.
	PromotingAt *time.Time `json:"promoting_at,omitempty"`
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
	// HomeLost is whether the host the standby row names as the app's home
	// is marked lost (R-421): the pass then leaves the standby for the
	// failover, which starts it.
	HomeLost bool
}

const standbyColumns = `s.app_id, s.runtime_ref, s.host, coalesce(s.spec_id, ''), s.volumes, s.copies, s.shipped_at,
	s.unreplicated, s.last_error, s.stale_host, s.promoted_at, s.home_host, s.last_error_code, s.last_error_remedy, s.promoting_at`

func scanStandby(row pgx.Row, extra ...any) (Standby, error) {
	var s Standby
	dest := make([]any, 0, len(extra)+15)
	dest = append(dest, extra...)
	dest = append(dest, &s.AppID, &s.RuntimeRef, &s.Host, &s.SpecID, &s.Volumes, &s.Copies, &s.ShippedAt,
		&s.Unreplicated, &s.LastError, &s.StaleHost, &s.PromotedAt, &s.Home, &s.LastErrorCode, &s.LastErrorRemedy,
		&s.PromotingAt)
	err := row.Scan(dest...)
	return s, err
}

// Candidates lists every live app with a pinned spec, with its standby if it
// has one, and every deleted app that still has a standby row.
func (s *Standbys) Candidates(ctx context.Context) ([]StandbyApp, error) {
	rows, err := s.db.Query(ctx, `
		SELECT a.id, a.state, a.desired_state, coalesce(a.pinned_spec_id, ''), a.deleted_at IS NOT NULL,
		       EXISTS (SELECT 1 FROM cluster_hosts h WHERE h.name = s.home_host AND s.home_host <> '' AND h.lost_at IS NOT NULL),
		       s.app_id IS NOT NULL,
		       coalesce(s.app_id, ''), coalesce(s.runtime_ref, ''), coalesce(s.host, ''), coalesce(s.spec_id, ''), coalesce(s.volumes, 0),
		       coalesce(s.copies, 0), s.shipped_at, coalesce(s.unreplicated, '{}'), coalesce(s.last_error, ''),
		       coalesce(s.stale_host, ''), s.promoted_at, coalesce(s.home_host, ''),
		       coalesce(s.last_error_code, ''), coalesce(s.last_error_remedy, ''), s.promoting_at
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
		sb, err := scanStandby(rows, &app.AppID, &app.State, &app.DesiredState, &app.PinnedSpecID, &app.Deleted, &app.HomeLost, &has)
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

// Save writes one app's standby, whatever its row says now. The standby
// pass and promotions write with SaveIf instead.
func (s *Standbys) Save(ctx context.Context, sb Standby) error {
	_, err := s.write(ctx, sb, `ON CONFLICT (app_id) DO UPDATE SET `+standbyUpdate)
	return err
}

// SaveIf writes one app's standby only if its row is still prev, as it was
// read: the same standby host, home, promotion and promotion under way
// (R-153). A nil prev writes only where the app has no row. It reports
// whether it wrote. Two writers working from one reading — the standby pass
// and a promotion — so never put back what the other changed: the one that
// read first loses, and reads again on its next pass.
func (s *Standbys) SaveIf(ctx context.Context, sb Standby, prev *Standby) (bool, error) {
	if prev == nil {
		return s.write(ctx, sb, `ON CONFLICT (app_id) DO NOTHING`)
	}
	return s.write(ctx, sb, `ON CONFLICT (app_id) DO UPDATE SET `+standbyUpdate+`
		WHERE app_standbys.host = $16 AND app_standbys.home_host = $17
		  AND app_standbys.promoted_at IS NOT DISTINCT FROM $18::timestamptz
		  AND app_standbys.promoting_at IS NOT DISTINCT FROM $19::timestamptz`,
		prev.Host, prev.Home, prev.PromotedAt, prev.PromotingAt)
}

const standbyUpdate = `
		    host = $2, spec_id = $3, volumes = $4, copies = $5, shipped_at = $6, unreplicated = $7,
		    last_error = $8, stale_host = $9, promoted_at = $10, runtime_ref = $11,
		    home_host = $12, last_error_code = $13, last_error_remedy = $14, promoting_at = $15,
		    updated_at = now()`

func (s *Standbys) write(ctx context.Context, sb Standby, conflict string, expect ...any) (bool, error) {
	if sb.Unreplicated == nil {
		sb.Unreplicated = []string{}
	}
	var specID *string
	if sb.SpecID != "" {
		specID = &sb.SpecID
	}
	args := append([]any{sb.AppID, sb.Host, specID, sb.Volumes, sb.Copies, sb.ShippedAt, sb.Unreplicated,
		sb.LastError, sb.StaleHost, sb.PromotedAt, sb.RuntimeRef, sb.Home, sb.LastErrorCode, sb.LastErrorRemedy,
		sb.PromotingAt}, expect...)
	tag, err := s.db.Exec(ctx, `
		INSERT INTO app_standbys (app_id, host, spec_id, volumes, copies, shipped_at, unreplicated,
		                          last_error, stale_host, promoted_at, runtime_ref,
		                          home_host, last_error_code, last_error_remedy, promoting_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		`+conflict, args...)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not record the app's standby.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// HomedOn is the standby of every live app whose home is host, the runtime's
// name for it, by app: the apps a lost host held (R-421). An app whose
// promotion away from host began and did not end is one of them, so marking
// the host lost again finishes it (R-153).
func (s *Standbys) HomedOn(ctx context.Context, host string) ([]Standby, error) {
	return s.listWhere(ctx, `(s.home_host = $1 AND s.promoting_at IS NULL) OR (s.promoting_at IS NOT NULL AND s.stale_host = $1)`,
		host, "Could not list the apps on the host.")
}

// LeftOn is the standby of every live app failed over away from host, which
// still holds the app's stale copy (R-419), by app.
func (s *Standbys) LeftOn(ctx context.Context, host string) ([]Standby, error) {
	return s.listWhere(ctx, `s.stale_host = $1`, host, "Could not list the apps failed over from the host.")
}

func (s *Standbys) listWhere(ctx context.Context, where, host, message string) ([]Standby, error) {
	rows, err := s.db.Query(ctx, `SELECT `+standbyColumns+` FROM app_standbys s
		JOIN apps a ON a.id = s.app_id AND a.deleted_at IS NULL
		WHERE `+where+` ORDER BY s.app_id`, host)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, message, err)
	}
	defer rows.Close()
	var out []Standby
	for rows.Next() {
		sb, err := scanStandby(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, message, err)
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// Delete forgets one app's standby, unless a promotion of it is under way
// (R-153): that row is what says where the app now runs.
func (s *Standbys) Delete(ctx context.Context, appID string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM app_standbys WHERE app_id = $1 AND promoting_at IS NULL`, appID); err != nil {
		return errs.Wrap(errs.Internal, "Could not forget the app's standby.", err)
	}
	return nil
}

// Hold is why the reconciler must leave an app alone (R-148, R-153, R-421).
type Hold struct {
	// Promoting is a promotion of its standby under way.
	Promoting bool
	// Home is the host the app's standby row names as its home, and
	// HomeLost and HomeFenced whether that host is marked lost or says it
	// is fenced.
	Home       string
	HomeLost   bool
	HomeFenced bool
}

// Held reads what may hold an app: its standby row's promotion and home.
// An app with no standby row, or whose home is not a cluster host, is held
// by nothing.
func (s *Standbys) Held(ctx context.Context, appID string) (Hold, error) {
	var h Hold
	err := s.db.QueryRow(ctx, `
		SELECT s.promoting_at IS NOT NULL, s.home_host,
		       coalesce(h.lost_at IS NOT NULL, false), coalesce(h.fenced_at IS NOT NULL, false)
		FROM app_standbys s
		LEFT JOIN cluster_hosts h ON h.name = s.home_host AND s.home_host <> ''
		WHERE s.app_id = $1`, appID).Scan(&h.Promoting, &h.Home, &h.HomeLost, &h.HomeFenced)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, nil
	}
	if err != nil {
		return Hold{}, errs.Wrap(errs.Internal, "Could not read whether the app's host is lost.", err)
	}
	return h, nil
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
