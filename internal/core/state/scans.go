package state

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Security scans (R-310 – R-319, design 09 §3).
//
// A scan belongs to a spec revision rather than to an app, which is what makes
// a rollback restore the score of what it rolled back to with no rescan: the
// row for that revision is still here.

// Scan is one scanner's answer about one revision.
type Scan struct {
	ID     string `json:"id"`
	AppID  string `json:"app_id"`
	SpecID string `json:"spec_id,omitempty"`
	// Commit names the source the scan read, when it read one: a git commit,
	// an image digest, or an upload's archive digest. A deploy of that source
	// uses this scan rather than scanning again (ForSource).
	Commit     string `json:"commit,omitempty"`
	ScannerRef string `json:"scanner_ref"`
	Scanner    string `json:"scanner,omitempty"`
	Score      *int   `json:"score"`

	// ScoreFixable counts only findings with a fix available. Which of the two
	// an installation means is host policy's (R-313); both are stored because
	// policy changes without rescanning.
	ScoreFixable *int          `json:"score_fixable,omitempty"`
	Findings     []api.Finding `json:"findings"`
	Error        string        `json:"error,omitempty"`
	RanAt        time.Time     `json:"ran_at"`

	// ReusedFrom is the scan this one repeats, when a deploy of a new revision
	// of an unchanged source reused it rather than scanning again (Reuse).
	// RanAt is that scan's: the findings are as old as the scan that found
	// them, not as the deploy that reused them.
	ReusedFrom string `json:"reused_from,omitempty"`
}

// Scans stores them.
type Scans struct{ db *DB }

func NewScans(db *DB) *Scans { return &Scans{db: db} }

// Record writes a scan.
//
// Every scan, including a failed one. A scanner that could not run is a fact
// somebody has to see — silence would leave an unscannable app looking clean
// (R-318).
func (s *Scans) Record(ctx context.Context, scan Scan) (Scan, error) {
	scan.ID = id.New(id.Scan)
	if scan.RanAt.IsZero() {
		scan.RanAt = time.Now().UTC()
	}
	if scan.Findings == nil {
		scan.Findings = []api.Finding{}
	}

	body, err := json.Marshal(scan.Findings)
	if err != nil {
		return Scan{}, errs.Wrap(errs.Internal, "Could not record the scan.", err)
	}

	_, err = s.db.Exec(ctx, `
		INSERT INTO app_scans (id, app_id, spec_id, scanner_ref, scanner, score, score_fixable,
		                       findings, error, ran_at, commit, reused_from)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		scan.ID, scan.AppID, nullable(scan.SpecID), scan.ScannerRef, scan.Scanner,
		scan.Score, scan.ScoreFixable, body, scan.Error, scan.RanAt, nullable(scan.Commit),
		nullable(scan.ReusedFrom))
	if err != nil {
		return Scan{}, errs.Wrap(errs.Internal, "Could not record the scan.", err)
	}
	return scan, nil
}

// Latest returns the scan that describes what this app is running.
//
// The newest scan of that revision, and failing that the newest scan that
// belongs to no revision — which is what detection produces, from the source,
// before there is a revision to attach it to. Never another revision's: a score
// for a spec the app is not running is not a score of what is deployed.
//
// The fallback is the difference between "scanned at discovery" meaning
// something and meaning nothing: accepting a proposal pins a revision, and
// without this the scan taken of that very source a minute earlier disappeared
// behind "this app has not been scanned yet".
func (s *Scans) Latest(ctx context.Context, appID, specID string) (Scan, bool, error) {
	query := `
		SELECT id, app_id, coalesce(spec_id, ''), scanner_ref, scanner, score, score_fixable,
		       findings, error, ran_at, coalesce(reused_from, '')
		FROM app_scans
		WHERE app_id = $1 AND ($2 = '' OR spec_id = $2 OR spec_id IS NULL)
		ORDER BY (spec_id IS NOT NULL) DESC, ran_at DESC
		LIMIT 1`

	var scan Scan
	var findings []byte
	err := s.db.QueryRow(ctx, query, appID, specID).Scan(
		&scan.ID, &scan.AppID, &scan.SpecID, &scan.ScannerRef, &scan.Scanner,
		&scan.Score, &scan.ScoreFixable, &findings, &scan.Error, &scan.RanAt, &scan.ReusedFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return Scan{}, false, nil
	}
	if err != nil {
		return Scan{}, false, errs.Wrap(errs.Internal, "Could not read the app's scans.", err)
	}
	if len(findings) > 0 {
		_ = json.Unmarshal(findings, &scan.Findings)
	}
	return scan, true, nil
}

// ForSource returns the newest scan of an app's source that ran, by the
// scanner named — a scanner that failed produced no finding worth reusing
// (R-318), and another scanner's findings are not this one's. Found is false
// for an empty source, which names nothing.
func (s *Scans) ForSource(ctx context.Context, appID, source, scannerRef string) (Scan, bool, error) {
	if source == "" {
		return Scan{}, false, nil
	}
	var scan Scan
	var findings []byte
	err := s.db.QueryRow(ctx, `
		SELECT id, app_id, coalesce(spec_id, ''), scanner_ref, scanner, score, score_fixable,
		       findings, error, ran_at, coalesce(reused_from, '')
		FROM app_scans
		WHERE app_id = $1 AND commit = $2 AND scanner_ref = $3 AND error = '' AND score IS NOT NULL
		ORDER BY ran_at DESC, (reused_from IS NULL) DESC
		LIMIT 1`, appID, source, scannerRef).Scan(
		&scan.ID, &scan.AppID, &scan.SpecID, &scan.ScannerRef, &scan.Scanner,
		&scan.Score, &scan.ScoreFixable, &findings, &scan.Error, &scan.RanAt, &scan.ReusedFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return Scan{}, false, nil
	}
	if err != nil {
		return Scan{}, false, errs.Wrap(errs.Internal, "Could not read the app's scans.", err)
	}
	if len(findings) > 0 {
		_ = json.Unmarshal(findings, &scan.Findings)
	}
	scan.Commit = source
	return scan, true, nil
}

// Reuse attaches an earlier scan to another revision, as a new row with the
// same findings, score and ran_at, naming the scan it repeats.
//
// A revision's score is the newest scan of that revision (Latest). A deploy of
// a new revision of an unchanged source does not scan again, and without this
// the revision would have no scan: refused as never scanned where a threshold
// is set, and shown as "not scanned" everywhere else.
func (s *Scans) Reuse(ctx context.Context, from Scan, specID string) (Scan, error) {
	reused := from
	reused.SpecID = specID
	// Always the scan that ran, never a copy of a copy: the chain would say
	// nothing a single hop does not.
	reused.ReusedFrom = from.Origin()
	return s.Record(ctx, reused)
}

// Origin is the ID of the scan that ran: this one, or the one it repeats.
func (s Scan) Origin() string {
	if s.ReusedFrom != "" {
		return s.ReusedFrom
	}
	return s.ID
}

// History returns an app's scans, newest first.
//
// The scans that ran. A reuse (Reuse) is the same scan attached to another
// revision, and listing it would show one scan twice at the same time.
func (s *Scans) History(ctx context.Context, appID string, limit int) ([]Scan, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, app_id, coalesce(spec_id, ''), scanner_ref, scanner, score, score_fixable,
		       findings, error, ran_at
		FROM app_scans
		WHERE app_id = $1 AND reused_from IS NULL
		ORDER BY ran_at DESC
		LIMIT $2`, appID, limit)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the app's scans.", err)
	}
	defer rows.Close()

	out := make([]Scan, 0)
	for rows.Next() {
		var scan Scan
		var findings []byte
		if err := rows.Scan(&scan.ID, &scan.AppID, &scan.SpecID, &scan.ScannerRef, &scan.Scanner,
			&scan.Score, &scan.ScoreFixable, &findings, &scan.Error, &scan.RanAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the app's scans.", err)
		}
		if len(findings) > 0 {
			_ = json.Unmarshal(findings, &scan.Findings)
		}
		out = append(out, scan)
	}
	return out, rows.Err()
}

// SecurityState is an app's standing against the installation's threshold.
type SecurityState struct {
	AppID              string
	InsecureSince      *time.Time
	StoppedForSecurity bool
	DesiredState       string
	State              string
	PinnedSpecID       string
	OwnerUserID        string
	Name               string

	// Score and ScoreFixable are the pinned revision's newest scan's two
	// numbers, as Scans.Latest would find it: what the security pass places
	// against policy, read in the same query so the pass is one query rather
	// than three per app (issue #72).
	Score        *int
	ScoreFixable *int
}

// LiveSecurityState returns every app the policy pass has to consider.
//
// Archived apps are not in it, and neither are drafts: an app that has never
// been deployed cannot be running below a threshold. What is in it is anything
// with a pinned revision, including a stopped one — an app Pando stopped for
// being insecure has to be looked at again to be started again.
func (s *Scans) LiveSecurityState(ctx context.Context) ([]SecurityState, error) {
	rows, err := s.db.Query(ctx, `
		SELECT a.id, a.name, coalesce(a.owner_user_id, ''), a.state, a.desired_state,
		       coalesce(a.pinned_spec_id, ''), a.insecure_since, a.stopped_for_security,
		       s.score, s.score_fixable
		FROM apps a
		LEFT JOIN LATERAL (
		    SELECT sc.score, sc.score_fixable
		    FROM app_scans sc
		    WHERE sc.app_id = a.id AND (sc.spec_id = a.pinned_spec_id OR sc.spec_id IS NULL)
		    ORDER BY (sc.spec_id IS NOT NULL) DESC, sc.ran_at DESC
		    LIMIT 1
		) s ON true
		WHERE a.deleted_at IS NULL
		  AND a.pinned_spec_id IS NOT NULL
		  AND a.state <> 'archived'
		ORDER BY a.id`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list apps for the security pass.", err)
	}
	defer rows.Close()

	out := make([]SecurityState, 0)
	for rows.Next() {
		var row SecurityState
		if err := rows.Scan(&row.AppID, &row.Name, &row.OwnerUserID, &row.State, &row.DesiredState,
			&row.PinnedSpecID, &row.InsecureSince, &row.StoppedForSecurity,
			&row.Score, &row.ScoreFixable); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list apps for the security pass.", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SecurityStateFor returns one app's standing.
func (s *Scans) SecurityStateFor(ctx context.Context, appID string) (SecurityState, bool, error) {
	var row SecurityState
	err := s.db.QueryRow(ctx, `
		SELECT a.id, a.name, coalesce(a.owner_user_id, ''), a.state, a.desired_state,
		       coalesce(a.pinned_spec_id, ''), a.insecure_since, a.stopped_for_security
		FROM apps a
		WHERE a.id = $1 AND a.deleted_at IS NULL`, appID).
		Scan(&row.AppID, &row.Name, &row.OwnerUserID, &row.State, &row.DesiredState,
			&row.PinnedSpecID, &row.InsecureSince, &row.StoppedForSecurity)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecurityState{}, false, nil
	}
	if err != nil {
		return SecurityState{}, false, errs.Wrap(errs.Internal, "Could not read the app.", err)
	}
	return row, true, nil
}

// MarkInsecure records when an app was first found below the threshold.
//
// Idempotent on purpose: the pass runs every tick, and the grace period is
// measured from the first time it was true, not the most recent (R-316). A
// clock that restarted on every pass would be a grace period that never expired.
func (s *Scans) MarkInsecure(ctx context.Context, appID string, at time.Time) error {
	_, err := s.db.Exec(ctx, `
		UPDATE apps SET insecure_since = coalesce(insecure_since, $2), updated_at = now()
		WHERE id = $1`, appID, at)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the app's security state.", err)
	}
	return nil
}

// ClearInsecure records that an app is back above the threshold.
func (s *Scans) ClearInsecure(ctx context.Context, appID string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE apps SET insecure_since = NULL, updated_at = now()
		WHERE id = $1`, appID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the app's security state.", err)
	}
	return nil
}

// SetStoppedForSecurity records whether Pando is the reason this app is stopped.
//
// The difference decides whether a recovered score starts it again: an app its
// owner stopped stays stopped (design 09 §4.2).
func (s *Scans) SetStoppedForSecurity(ctx context.Context, appID string, stopped bool) error {
	_, err := s.db.Exec(ctx, `
		UPDATE apps SET stopped_for_security = $2, updated_at = now()
		WHERE id = $1`, appID, stopped)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the app's security state.", err)
	}
	return nil
}
