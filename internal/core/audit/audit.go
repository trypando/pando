package audit

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trypando/pando/internal/core/events"
	"github.com/trypando/pando/internal/errs"
)

// PrincipalKind identifies who acted.
type PrincipalKind string

const (
	KindUser      PrincipalKind = "user"
	KindToken     PrincipalKind = "token"
	KindSystem    PrincipalKind = "system"
	KindAnonymous PrincipalKind = "anonymous"
)

// Event is one audit record.
//
// Nothing here is ever updated or deleted — the database enforces that, not
// this type (R-027). Detail must never contain a secret value; secret.Value
// makes that structural on the way in.
type Event struct {
	PrincipalKind PrincipalKind
	PrincipalID   string

	// OnBehalfOf is the owning user when a delegated token acted (R-229).
	// Authorization already resolved through the owner; the audit log records
	// both so "what did this person do" stays answerable.
	OnBehalfOf string

	Action     string
	AppID      string
	TargetKind string
	TargetID   string
	RequestID  string
	Detail     map[string]any
}

// Writer appends to the audit log.
type Writer struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Writer { return &Writer{pool: pool} }

// Write appends an event.
//
// Callers write the event *before* the privileged action, not after (R-228):
// an exec session that fails to open is still recorded as attempted. Denials
// are audited as well as successes — a denial pattern is the signal that
// matters for detecting misuse, and it is the thing most commonly left out.
func (w *Writer) Write(ctx context.Context, e Event) error {
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not encode audit detail.", err)
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not write an audit event.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			principal_kind, principal_id, on_behalf_of,
			action, app_id, target_kind, target_id, request_id, detail
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.PrincipalKind,
		nullable(e.PrincipalID),
		nullable(e.OnBehalfOf),
		e.Action,
		nullable(e.AppID),
		nullable(e.TargetKind),
		nullable(e.TargetID),
		nullable(e.RequestID),
		encoded,
	)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not write an audit event.", err)
	}
	if err := mirror(ctx, tx, e); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not write an audit event.", err)
	}
	return nil
}

// mirror copies a catalogued action into the event outbox, in the transaction
// the audit row is written in (R-365, issue #50). Both rows or neither: an
// action recorded is an action subscribers hear about, and a subscriber never
// hears about an action the log does not hold.
//
// Only the fields the catalog lists are copied from the detail, so what reaches
// a subscriber's endpoint is what docs/events.md says it is.
func mirror(ctx context.Context, tx pgx.Tx, e Event) error {
	def, ok := events.ForAction(e.Action)
	if !ok {
		return nil
	}
	data := def.Data(e.Detail)
	if e.TargetKind != "" {
		data["target_kind"] = e.TargetKind
		data["target_id"] = e.TargetID
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not encode an event.", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO events (name, app_id, actor_kind, actor_id, on_behalf_of, request_id, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		def.Name, nullable(e.AppID), e.PrincipalKind, nullable(e.PrincipalID),
		nullable(e.OnBehalfOf), nullable(e.RequestID), encoded); err != nil {
		return errs.Wrap(errs.Internal, "Could not record an event.", err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
