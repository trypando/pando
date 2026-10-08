package audit

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trypando/pando/internal/clientaddr"
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

	// Outcome is success, denied or failed (R-379). Empty derives it from
	// the action (OutcomeOf), which is right for every event whose action
	// name already says how it ended. A finished deploy, whose one action
	// covers both, sets it.
	Outcome Outcome
}

// Outcome is how the action an event records ended (R-379).
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeDenied  Outcome = "denied"
	OutcomeFailed  Outcome = "failed"
)

// failedActions end in failure without saying so in their last segment.
var failedActions = map[string]bool{
	"app.failed":               true,
	"app.delete.backup_failed": true,
	"upgrade.rolled_back":      true,
}

// OutcomeOf is the outcome an action's name says (design 12 §3.1): .denied
// and .refused are denials, .failed and the few listed above are failures,
// and everything else succeeded. TestR379_EveryActionHasAnOutcome holds the
// catalog to it.
func OutcomeOf(action string) Outcome {
	switch {
	case strings.HasSuffix(action, ".denied"), strings.HasSuffix(action, ".refused"):
		return OutcomeDenied
	case strings.HasSuffix(action, ".failed"), failedActions[action]:
		return OutcomeFailed
	default:
		return OutcomeSuccess
	}
}

// target is what the row records the event as being about. Always set
// (R-379): an event naming an app and nothing narrower is about the app, and
// one naming neither is about the installation.
func (e Event) target() (kind, id string) {
	switch {
	case e.TargetKind != "":
		return e.TargetKind, e.TargetID
	case e.AppID != "":
		return "app", e.AppID
	default:
		return "install", "install"
	}
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

	outcome := e.Outcome
	if outcome == "" {
		outcome = OutcomeOf(e.Action)
	}
	targetKind, targetID := e.target()
	// Where the request came from (R-379, R-380), set by the API and the
	// proxy. A system process's context has none, and its rows say so.
	from, _ := clientaddr.From(ctx)

	// The acting person's name and email as they are now, looked up in the
	// insert so a renamed user's past events keep the name they acted under,
	// and a SIEM that cannot join on Pando's IDs still has one. A token acting
	// for someone is that person; an account token is nobody's.
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			principal_kind, principal_id, on_behalf_of,
			action, app_id, target_kind, target_id, request_id, detail,
			outcome, source_ip, peer_ip, user_agent, actor_name, actor_email
		)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, u.display_name, u.email
		FROM (SELECT 1) AS one
		LEFT JOIN users u ON u.id = coalesce($3::text, CASE WHEN $1::text = 'user' THEN $2::text END)`,
		string(e.PrincipalKind),
		nullable(e.PrincipalID),
		nullable(e.OnBehalfOf),
		e.Action,
		nullable(e.AppID),
		targetKind,
		nullable(targetID),
		nullable(e.RequestID),
		encoded,
		string(outcome),
		nullable(from.SourceIP),
		nullable(from.PeerIP),
		nullable(from.UserAgent),
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
