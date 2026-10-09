package state

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// AuditSinkState is where one audit sink has got to (design 12 §5.2).
type AuditSinkState struct {
	AdapterID string `json:"adapter_id"`

	// HasCursor is false until the first delivery. CursorTxID and CursorID are
	// the last event delivered, in the stream's (txid, id) order.
	HasCursor  bool   `json:"-"`
	CursorTxID uint64 `json:"-"`
	CursorID   int64  `json:"-"`

	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
	CursorAt       *time.Time `json:"cursor_at,omitempty"`
	DeliveredCount int64      `json:"delivered_count"`

	LastError     string     `json:"last_error,omitempty"`
	LastErrorAt   *time.Time `json:"last_error_at,omitempty"`
	FailingSince  *time.Time `json:"failing_since,omitempty"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`

	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
	DisabledReason string     `json:"disabled_reason,omitempty"`

	GapFrom *time.Time `json:"gap_from,omitempty"`
	GapTo   *time.Time `json:"gap_to,omitempty"`
}

// AuditSinks stores audit sink delivery state. It never touches audit_events
// except to read it (R-027).
type AuditSinks struct{ db *DB }

func NewAuditSinks(db *DB) *AuditSinks { return &AuditSinks{db: db} }

const auditSinkColumns = `adapter_id, cursor_txid::text, cursor_id, delivered_at, cursor_at, delivered_count,
	coalesce(last_error, ''), last_error_at, failing_since, attempts, next_attempt_at,
	disabled_at, coalesce(disabled_reason, ''), gap_from, gap_to`

func scanAuditSink(row pgx.Row) (AuditSinkState, error) {
	var s AuditSinkState
	var txid *string
	var id *int64
	if err := row.Scan(&s.AdapterID, &txid, &id, &s.DeliveredAt, &s.CursorAt, &s.DeliveredCount,
		&s.LastError, &s.LastErrorAt, &s.FailingSince, &s.Attempts, &s.NextAttemptAt,
		&s.DisabledAt, &s.DisabledReason, &s.GapFrom, &s.GapTo); err != nil {
		return s, err
	}
	if txid != nil && id != nil {
		t, err := strconv.ParseUint(*txid, 10, 64)
		if err != nil {
			return s, err
		}
		s.HasCursor, s.CursorTxID, s.CursorID = true, t, *id
	}
	return s, nil
}

// List returns every sink's state.
func (s *AuditSinks) List(ctx context.Context) ([]AuditSinkState, error) {
	rows, err := s.db.Query(ctx, `SELECT `+auditSinkColumns+` FROM audit_sink_state ORDER BY adapter_id`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the audit sinks' delivery state.", err)
	}
	defer rows.Close()
	var out []AuditSinkState
	for rows.Next() {
		st, err := scanAuditSink(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the audit sinks' delivery state.", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// AuditCursor is a position in the audit stream, as the state store keeps
// it: the stream's (txid, id) order (design 12 §2).
type AuditCursor struct {
	TxID uint64
	ID   int64
}

// Get returns one sink's state, creating it when it has none: positioned at
// the start of the live log, or at start when given — a sink that starts now
// (R-382).
func (s *AuditSinks) Get(ctx context.Context, adapterID string, start *AuditCursor) (AuditSinkState, error) {
	var txid, id any
	if start != nil {
		txid, id = strconv.FormatUint(start.TxID, 10), start.ID
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO audit_sink_state (adapter_id, cursor_txid, cursor_id)
		VALUES ($1, $2::xid8, $3) ON CONFLICT (adapter_id) DO NOTHING`, adapterID, txid, id); err != nil {
		return AuditSinkState{}, errs.Wrap(errs.Internal, "Could not record an audit sink's delivery state.", err)
	}
	st, err := scanAuditSink(s.db.QueryRow(ctx, `SELECT `+auditSinkColumns+` FROM audit_sink_state WHERE adapter_id = $1`, adapterID))
	if err != nil {
		return st, errs.Wrap(errs.Internal, "Could not read an audit sink's delivery state.", err)
	}
	return st, nil
}

// Delivered moves a sink's cursor after a successful send and clears its
// error. It returns whether the sink had been failing, so the recovery is
// audited once (R-383).
func (s *AuditSinks) Delivered(ctx context.Context, adapterID string, txid uint64, id int64, n int, at time.Time) (bool, error) {
	var wasFailing bool
	err := s.db.QueryRow(ctx, `
		WITH before AS (SELECT failing_since IS NOT NULL AS failing FROM audit_sink_state WHERE adapter_id = $1)
		UPDATE audit_sink_state SET
			cursor_txid = $2::xid8, cursor_id = $3,
			cursor_at = (SELECT occurred_at FROM audit_events WHERE txid = $2::xid8 AND id = $3 LIMIT 1),
			delivered_at = $5, delivered_count = delivered_count + $4,
			last_error = NULL, failing_since = NULL, attempts = 0, next_attempt_at = NULL,
			updated_at = $5
		WHERE adapter_id = $1
		RETURNING (SELECT failing FROM before)`,
		adapterID, strconv.FormatUint(txid, 10), id, n, at).Scan(&wasFailing)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not record an audit sink's delivery.", err)
	}
	return wasFailing, nil
}

// Skipped moves a sink's cursor past events it was not sent — every one a
// filter dropped — without counting a delivery.
func (s *AuditSinks) Skipped(ctx context.Context, adapterID string, txid uint64, id int64, at time.Time) error {
	if _, err := s.db.Exec(ctx, `
		UPDATE audit_sink_state SET cursor_txid = $2::xid8, cursor_id = $3,
			cursor_at = (SELECT occurred_at FROM audit_events WHERE txid = $2::xid8 AND id = $3 LIMIT 1),
			updated_at = $4
		WHERE adapter_id = $1`, adapterID, strconv.FormatUint(txid, 10), id, at); err != nil {
		return errs.Wrap(errs.Internal, "Could not record an audit sink's position.", err)
	}
	return nil
}

// Failed records a failed send and when to try again. It returns the state
// after, so the caller can tell a first failure from a repeated one.
func (s *AuditSinks) Failed(ctx context.Context, adapterID, message string, at, next time.Time) (AuditSinkState, error) {
	st, err := scanAuditSink(s.db.QueryRow(ctx, `
		UPDATE audit_sink_state SET
			last_error = $2, last_error_at = $3, failing_since = coalesce(failing_since, $3),
			attempts = attempts + 1, next_attempt_at = $4, updated_at = $3
		WHERE adapter_id = $1
		RETURNING `+auditSinkColumns, adapterID, message, at, next))
	if err != nil {
		return st, errs.Wrap(errs.Internal, "Could not record an audit sink's failure.", err)
	}
	return st, nil
}

// Disable turns a failing sink off: its adapter row, so it stops holding
// archival and the adapters screen shows it off, and its state, so the reason
// is shown beside it (R-383, R-386). It reports whether this call changed
// anything, so the caller audits and notifies once. A sink declared in the
// configuration file has no row to turn off; its state alone records it.
func (s *AuditSinks) Disable(ctx context.Context, adapterID, reason string, at time.Time) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not turn off an audit sink.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE audit_sink_state SET disabled_at = $3, disabled_reason = $2, next_attempt_at = NULL, updated_at = $3
		WHERE adapter_id = $1 AND disabled_at IS NULL`, adapterID, reason, at)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not turn off an audit sink.", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE adapter_configs SET enabled = false, updated_at = $2
		WHERE id = $1 AND category = 'audit_sink'`, adapterID, at); err != nil {
		return false, errs.Wrap(errs.Internal, "Could not turn off an audit sink.", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, errs.Wrap(errs.Internal, "Could not turn off an audit sink.", err)
	}
	return true, nil
}

// Resume clears a sink Pando turned off and somebody turned back on, and
// records the range it missed (R-386): events after its cursor that the
// archiver removed while it did not hold. The archive covering them is the
// backfill.
func (s *AuditSinks) Resume(ctx context.Context, adapterID string, at time.Time) (AuditSinkState, error) {
	st, err := scanAuditSink(s.db.QueryRow(ctx, `
		WITH me AS (SELECT * FROM audit_sink_state WHERE adapter_id = $1),
		     lost AS (
		         -- Archived months past the cursor whose rows have left the
		         -- live log: an archive whose drop has not happened yet is
		         -- not a gap.
		         SELECT min(a.first_at) AS from_at, max(a.last_at) AS to_at FROM audit_archives a, me
		         WHERE a.row_count > 0
		           AND (me.cursor_id IS NULL OR a.last_at > coalesce(me.cursor_at, '-infinity'))
		           AND NOT EXISTS (SELECT 1 FROM audit_events e WHERE e.id = a.last_id)
		     )
		UPDATE audit_sink_state s SET
			disabled_at = NULL, disabled_reason = NULL, failing_since = NULL, attempts = 0,
			next_attempt_at = NULL, last_error = NULL,
			gap_from = CASE WHEN lost.to_at IS NULL THEN s.gap_from ELSE greatest(lost.from_at, coalesce(s.cursor_at, lost.from_at)) END,
			gap_to   = coalesce(lost.to_at, s.gap_to),
			updated_at = $2
		FROM lost
		WHERE s.adapter_id = $1
		RETURNING `+auditSinkColumns, adapterID, at))
	if err != nil {
		return st, errs.Wrap(errs.Internal, "Could not turn an audit sink back on.", err)
	}
	return st, nil
}

// Backlog counts events past a cursor, at most limit: counting a large
// backlog exactly would cost what the backlog does.
func (s *AuditSinks) Backlog(ctx context.Context, st AuditSinkState, limit int) (int, error) {
	txid, id := "0", int64(0)
	if st.HasCursor {
		txid, id = strconv.FormatUint(st.CursorTxID, 10), st.CursorID
	}
	var n int
	if err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT 1 FROM audit_events WHERE (txid, id) > ($1::xid8, $2::bigint) LIMIT $3
		) AS past`, txid, id, limit).Scan(&n); err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not count an audit sink's backlog.", err)
	}
	return n, nil
}

// Holds reports whether a sink has events in [lo, hi) it has not been sent
// (R-386). A sink with no cursor yet starts at the oldest event, so it holds
// every month that has any.
func (s *AuditSinks) Holds(ctx context.Context, st AuditSinkState, lo, hi time.Time) (bool, error) {
	txid, id := "0", int64(0)
	if st.HasCursor {
		txid, id = strconv.FormatUint(st.CursorTxID, 10), st.CursorID
	}
	var held bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM audit_events
			WHERE occurred_at >= $3 AND occurred_at < $4 AND (txid, id) > ($1::xid8, $2::bigint)
		)`, txid, id, lo, hi).Scan(&held)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, errs.Wrap(errs.Internal, "Could not read what an audit sink has been sent.", err)
	}
	return held, nil
}
