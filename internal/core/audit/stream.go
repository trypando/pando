package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/errs"
)

// The audit stream (R-381, design 12 §2).
//
// The log's ids come from a sequence, taken at insert and not at commit, so a
// reader paging by id can pass an event that has not committed yet and never
// see it. Each row also records the transaction that wrote it, and the stream
// reads only rows written by transactions older than the oldest one still
// running — pg_snapshot_xmin — in (txid, id) order. Nothing can commit behind
// that horizon, so nothing can ever appear behind a cursor.

// Cursor is a position in the stream: the last event read. The zero Cursor is
// before the first event in the live log.
type Cursor struct {
	TxID uint64
	ID   int64
}

// cursorPrefix versions the wire form, so a later cursor can change shape
// without a consumer's stored one being misread.
const cursorPrefix = "c1."

// CursorNow is the wire value meaning "after the newest settled event".
const CursorNow = "now"

func (c Cursor) String() string {
	return fmt.Sprintf("%s%d.%d", cursorPrefix, c.TxID, c.ID)
}

// IsZero reports whether c is before the first event.
func (c Cursor) IsZero() bool { return c.TxID == 0 && c.ID == 0 }

// ParseCursor reads a cursor as String writes it. Empty is the zero Cursor.
// "now" is not a cursor and is resolved by Reader.Now.
func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	bad := errs.Newf(errs.ValidInvalid,
		"%q is not an audit stream cursor. Pass back the cursor a previous read returned, such as c1.48213.10442, "+
			"or leave it out to start at the oldest event, or use now to start after the newest.", s)
	rest, ok := strings.CutPrefix(s, cursorPrefix)
	if !ok {
		return Cursor{}, bad
	}
	tx, id, ok := strings.Cut(rest, ".")
	if !ok {
		return Cursor{}, bad
	}
	t, err1 := strconv.ParseUint(tx, 10, 64)
	i, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil || i < 0 {
		return Cursor{}, bad
	}
	return Cursor{TxID: t, ID: i}, nil
}

// StreamQuery is one page of the stream.
type StreamQuery struct {
	After Cursor

	// Limit defaults to 500, capped at 1000 (StreamPageSize).
	Limit int

	// Actions and Exclude are prefixes. An event is read when it matches one
	// of Actions (or Actions is empty) and none of Exclude. The cursor moves
	// past the events a filter drops, so a filtered reader never reads them
	// again.
	Actions []string
	Exclude []string

	// Since and Until bound occurred_at, Since inclusive and Until exclusive,
	// for an export of a range. Zero is unbounded.
	Since time.Time
	Until time.Time
}

const (
	defaultStreamLimit = 500
	maxStreamLimit     = 1000
)

// StreamPageSize is how many events a StreamQuery asking for `requested`
// scans.
func StreamPageSize(requested int) int {
	if requested <= 0 {
		return defaultStreamLimit
	}
	return min(requested, maxStreamLimit)
}

// StreamPage is what one read returns.
type StreamPage struct {
	// Events are native lines (design 12 §6.1): each row as the archiver
	// writes it, oldest first.
	Events []json.RawMessage

	// IDs are the events' ids, in the same order.
	IDs []int64

	// Cursor is where the next read starts. It has moved past every event
	// scanned, including those a filter dropped.
	Cursor Cursor

	// CaughtUp says the read reached the horizon: nothing settled lies past
	// Cursor right now.
	CaughtUp bool
}

// Stream reads one page past q.After.
func (r *Reader) Stream(ctx context.Context, q StreamQuery) (StreamPage, error) {
	limit := StreamPageSize(q.Limit)
	page := StreamPage{Cursor: q.After}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, errs.Wrap(errs.Internal, "Could not read the audit stream.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lines carry UTC times whichever zone the session would have used, as
	// the archive's do, so a live and an archived event are one shape.
	if _, err := tx.Exec(ctx, `SET LOCAL TimeZone = 'UTC'`); err != nil {
		return page, errs.Wrap(errs.Internal, "Could not read the audit stream.", err)
	}

	args := []any{strconv.FormatUint(q.After.TxID, 10), q.After.ID}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	where := []string{
		"e.txid < pg_snapshot_xmin(pg_current_snapshot())",
		"(e.txid, e.id) > ($1::xid8, $2::bigint)",
	}
	if !q.Since.IsZero() {
		where = append(where, "e.occurred_at >= "+arg(q.Since))
	}
	if !q.Until.IsZero() {
		where = append(where, "e.occurred_at < "+arg(q.Until))
	}
	match := "true"
	var include []string
	for _, a := range q.Actions {
		if a != "" {
			include = append(include, "e.action LIKE "+arg(escapeLike(a))+" || '%' ESCAPE '\\'")
		}
	}
	if len(include) > 0 {
		match = "(" + strings.Join(include, " OR ") + ")"
	}
	for _, a := range q.Exclude {
		if a != "" {
			match += " AND e.action NOT LIKE " + arg(escapeLike(a)) + " || '%' ESCAPE '\\'"
		}
	}

	// Every row past the cursor is scanned, matching or not, so the cursor
	// can move past the ones a filter drops.
	sql := `SELECT e.txid::text, e.id, ` + match + `, row_to_json(e)::text
	        FROM audit_events e
	        WHERE ` + strings.Join(where, " AND ") + `
	        ORDER BY e.txid, e.id
	        LIMIT ` + arg(limit)
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return page, errs.Wrap(errs.Internal, "Could not read the audit stream.", err)
	}
	defer rows.Close()

	scanned := 0
	for rows.Next() {
		var txid string
		var id int64
		var matched bool
		var line string
		if err := rows.Scan(&txid, &id, &matched, &line); err != nil {
			return page, errs.Wrap(errs.Internal, "Could not read the audit stream.", err)
		}
		t, err := strconv.ParseUint(txid, 10, 64)
		if err != nil {
			return page, errs.Wrap(errs.Internal, "Could not read the audit stream.", err)
		}
		scanned++
		page.Cursor = Cursor{TxID: t, ID: id}
		if matched {
			page.Events = append(page.Events, json.RawMessage(line))
			page.IDs = append(page.IDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return page, errs.Wrap(errs.Internal, "Could not read the audit stream.", err)
	}
	page.CaughtUp = scanned < limit
	return page, nil
}

// Now is the cursor after the newest settled event: what a reader that wants
// only what happens next starts from.
func (r *Reader) Now(ctx context.Context) (Cursor, error) {
	var txid string
	var id int64
	err := r.pool.QueryRow(ctx, `
		SELECT e.txid::text, e.id FROM audit_events e
		WHERE e.txid < pg_snapshot_xmin(pg_current_snapshot())
		ORDER BY e.txid DESC, e.id DESC LIMIT 1`).Scan(&txid, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cursor{}, nil
	}
	if err != nil {
		return Cursor{}, errs.Wrap(errs.Internal, "Could not read the audit stream.", err)
	}
	t, err := strconv.ParseUint(txid, 10, 64)
	if err != nil {
		return Cursor{}, errs.Wrap(errs.Internal, "Could not read the audit stream.", err)
	}
	return Cursor{TxID: t, ID: id}, nil
}

// ResolveCursor reads a cursor from the wire, "now" included.
func (r *Reader) ResolveCursor(ctx context.Context, s string) (Cursor, error) {
	if s == CursorNow {
		return r.Now(ctx)
	}
	return ParseCursor(s)
}

// MaxStreamWait bounds a long poll.
const MaxStreamWait = 60 * time.Second

// streamPoll is how often a waiting read looks again. A second: the stream
// is for collectors, which batch anyway, and a waiting read costs one indexed
// query a second while it waits.
const streamPoll = time.Second

// Wait reads like Stream, but when nothing settled lies past the cursor it
// looks again every second until something does or wait runs out, and then
// returns the empty page with the cursor unchanged.
func (r *Reader) Wait(ctx context.Context, q StreamQuery, wait time.Duration, c clock.Clock) (StreamPage, error) {
	wait = min(wait, MaxStreamWait)
	if c == nil {
		c = clock.System{}
	}
	deadline := c.Now().Add(wait)
	for {
		page, err := r.Stream(ctx, q)
		if err != nil || len(page.Events) > 0 || !page.CaughtUp {
			return page, err
		}
		// A page that moved the cursor past only filtered events is not
		// news, but its cursor is: keep it.
		q.After = page.Cursor
		if !c.Now().Before(deadline) {
			return page, nil
		}
		select {
		case <-ctx.Done():
			return page, nil
		case <-c.After(streamPoll):
		}
	}
}

// Encoder turns a native line into another format, such as OCSF.
type Encoder func(line json.RawMessage) (json.RawMessage, error)

// Export writes every settled event in [q.Since, q.Until), in commit order,
// one line each, through enc (nil for native). It pages from a cursor, so an
// export of a year is never held in memory.
func (r *Reader) Export(ctx context.Context, q StreamQuery, enc Encoder, write func(line []byte) error) (int, error) {
	q.Limit = maxStreamLimit
	n := 0
	for {
		page, err := r.Stream(ctx, q)
		if err != nil {
			return n, err
		}
		for _, line := range page.Events {
			out := line
			if enc != nil {
				if out, err = enc(line); err != nil {
					return n, err
				}
			}
			if err := write(out); err != nil {
				return n, err
			}
			n++
		}
		if page.CaughtUp {
			return n, nil
		}
		q.After = page.Cursor
	}
}

// Oldest is when the oldest event in the live log happened, and false when it
// is empty. An export reaching before it says so (R-387).
func (r *Reader) Oldest(ctx context.Context) (time.Time, bool, error) {
	var at *time.Time
	if err := r.pool.QueryRow(ctx, `SELECT min(occurred_at) FROM audit_events`).Scan(&at); err != nil {
		return time.Time{}, false, errs.Wrap(errs.Internal, "Could not read the audit log.", err)
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return at.UTC(), true, nil
}
