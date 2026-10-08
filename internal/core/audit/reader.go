package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trypando/pando/internal/errs"
)

// Reader queries the audit log (R-227, R-229).
//
// A separate type from Writer, because they are separate privileges: writing is
// something every code path does, and reading is gated by install.audit.read.
// Neither can update or delete — the database refuses both to the application
// role (R-027), so this type could not offer it even if someone added a method.
type Reader struct {
	pool *pgxpool.Pool
}

func NewReader(pool *pgxpool.Pool) *Reader { return &Reader{pool: pool} }

// Record is one stored event, as read back.
type Record struct {
	ID            int64          `json:"id"`
	OccurredAt    time.Time      `json:"occurred_at"`
	PrincipalKind string         `json:"principal_kind"`
	PrincipalID   string         `json:"principal_id,omitempty"`
	OnBehalfOf    string         `json:"on_behalf_of,omitempty"`
	Action        string         `json:"action"`
	AppID         string         `json:"app_id,omitempty"`
	TargetKind    string         `json:"target_kind,omitempty"`
	TargetID      string         `json:"target_id,omitempty"`
	RequestID     string         `json:"request_id,omitempty"`
	Detail        map[string]any `json:"detail,omitempty"`
}

// Query narrows a read. Every field is optional; the zero value reads the whole
// log, newest first.
type Query struct {
	// Action matches a prefix, so "app." finds every app event and
	// "session.denied" finds exactly one kind. A prefix rather than a substring
	// because actions are dotted namespaces — a substring match would make
	// "grant.delete" a result for a search for "delete".
	Action string

	// Actions are more prefixes, any of which matches, alongside Action:
	// "apps someone added or deleted" is app.create or app.delete, and filters
	// otherwise combine with AND.
	Actions []string

	AppID       string
	PrincipalID string

	// PrincipalKind narrows to one kind of actor: user, token, system or
	// anonymous. The only way to find anonymous events, which have no ID, and
	// every system process at once rather than one at a time.
	PrincipalKind string

	// TargetKind and TargetID narrow to what the action was done to — "every
	// change to this user", "everything done to any role". Exact matches.
	TargetKind string
	TargetID   string

	// Involving narrows to events where one ID is on either side: the actor
	// (or the person a token acted for) or the target. "Everything to do with
	// this account" — what it did and what was done to it — which the actor and
	// target filters cannot say, because filters combine with AND.
	Involving string

	// Since and Until bound when it happened: Since inclusive, Until
	// exclusive, so consecutive ranges neither overlap nor leave a gap. Zero
	// means unbounded.
	Since time.Time
	Until time.Time

	// Limit defaults to 100, capped at 500. PageSize is the clamp.
	Limit int

	// Cursor is the ID of the last record on the previous page. The log is
	// append-only with monotonic IDs, so "before this ID" is a stable page
	// boundary in a way an offset is not — with an offset, events arriving
	// between requests shift every later page.
	Cursor int64
}

const defaultLimit = 100

const maxAuditLimit = 500

// PageSize is how many records a Query asking for `requested` will return.
//
// Exported because the API layer has to know it to decide whether a page was
// full, and a function rather than the constant it used to export because
// knowing the default is not enough: a caller asking for 1000 got 500 records
// and a layer above comparing them against 1000, so the page never looked full
// and the cursor for the next page was never sent. Asking the same code that
// does the clamping is the only version of this that cannot drift.
//
// It is also what keeps the allocation below bounded by a constant rather than
// by whatever number arrived in a query string.
func PageSize(requested int) int {
	if requested <= 0 {
		return defaultLimit
	}
	if requested > maxAuditLimit {
		return maxAuditLimit
	}
	return requested
}

// List reads the log, newest first.
func (r *Reader) List(ctx context.Context, q Query) ([]Record, error) {
	limit := PageSize(q.Limit)

	var where []string
	var args []any

	// arg appends a value and returns its placeholder, so the clauses below
	// cannot drift out of step with the argument list.
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	var prefixes []string
	for _, a := range append([]string{q.Action}, q.Actions...) {
		if a != "" {
			// The prefix is escaped: an action containing % or _ would
			// otherwise widen the filter rather than narrow it.
			prefixes = append(prefixes, "action LIKE "+arg(escapeLike(a))+" || '%' ESCAPE '\\'")
		}
	}
	if len(prefixes) > 0 {
		where = append(where, "("+strings.Join(prefixes, " OR ")+")")
	}
	if q.AppID != "" {
		where = append(where, "app_id = "+arg(q.AppID))
	}
	if q.PrincipalID != "" {
		// Either column: a delegated token records both itself and its owner
		// (R-229), and "what did this person do" must find both.
		p := arg(q.PrincipalID)
		where = append(where, "(principal_id = "+p+" OR on_behalf_of = "+p+")")
	}
	if q.PrincipalKind != "" {
		where = append(where, "principal_kind = "+arg(q.PrincipalKind))
	}
	if q.TargetKind != "" {
		where = append(where, "target_kind = "+arg(q.TargetKind))
	}
	if q.TargetID != "" {
		where = append(where, "target_id = "+arg(q.TargetID))
	}
	if q.Involving != "" {
		v := arg(q.Involving)
		where = append(where, "(principal_id = "+v+" OR on_behalf_of = "+v+" OR target_id = "+v+")")
	}
	if !q.Since.IsZero() {
		where = append(where, "occurred_at >= "+arg(q.Since))
	}
	if !q.Until.IsZero() {
		where = append(where, "occurred_at < "+arg(q.Until))
	}
	if q.Cursor > 0 {
		where = append(where, "id < "+arg(q.Cursor))
	}

	sql := `SELECT id, occurred_at, principal_kind, coalesce(principal_id, ''),
	               coalesce(on_behalf_of, ''), action, coalesce(app_id, ''),
	               coalesce(target_kind, ''), coalesce(target_id, ''),
	               coalesce(request_id, ''), detail
	        FROM audit_events`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY id DESC LIMIT " + arg(limit)

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the audit log.", err)
	}
	defer rows.Close()

	out := make([]Record, 0, limit)
	for rows.Next() {
		var rec Record
		var detail []byte
		if err := rows.Scan(&rec.ID, &rec.OccurredAt, &rec.PrincipalKind, &rec.PrincipalID,
			&rec.OnBehalfOf, &rec.Action, &rec.AppID, &rec.TargetKind, &rec.TargetID,
			&rec.RequestID, &detail); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the audit log.", err)
		}
		if len(detail) > 0 {
			if err := json.Unmarshal(detail, &rec.Detail); err != nil {
				// A detail blob that will not parse is a damaged row, not a
				// reason to withhold the event. The who/what/when is the part
				// an investigation needs first.
				rec.Detail = map[string]any{"unreadable": true}
			}
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// escapeLike neutralizes the wildcards in a user-supplied prefix.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
