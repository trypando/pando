package state

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// SSOFlow is a redirect sign-in in progress (design 06 §3.2).
type SSOFlow struct {
	ID           string
	AdapterID    string
	Purpose      string // sign_in | test
	BindHash     string
	ReturnOrigin string
	NextPath     string
	CallbackURL  string
	EntityID     string
	Flow         []byte
	InitiatedBy  string
	UserID       string
	Result       json.RawMessage
	Failed       bool
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ConsumedAt   *time.Time
}

// The two things a flow is for.
const (
	FlowSignIn = "sign_in"
	FlowTest   = "test"
)

// SSOFlows stores flows.
type SSOFlows struct{ db *DB }

func NewSSOFlows(db *DB) *SSOFlows { return &SSOFlows{db: db} }

// Create stores a new flow. Expired flows and spent one-time identifiers are
// removed by the retention job (Retention.SSOFlows), not here: two deletes on
// every sign-in were two full scans on the path everyone waits on (issue #72).
func (s *SSOFlows) Create(ctx context.Context, f SSOFlow) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO sso_flows (id, adapter_id, purpose, bind_hash, return_origin, next_path, callback_url,
		                       entity_id, flow, initiated_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		f.ID, f.AdapterID, f.Purpose, nullable(f.BindHash), f.ReturnOrigin, f.NextPath, f.CallbackURL,
		f.EntityID, nullableBytes(f.Flow), nullable(f.InitiatedBy), f.ExpiresAt)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not start the sign-in.", err)
	}
	return nil
}

const flowColumns = `id, adapter_id, purpose, coalesce(bind_hash, ''), return_origin, next_path, callback_url,
	entity_id, flow, coalesce(initiated_by, ''), coalesce(user_id, ''), result, failed,
	created_at, expires_at, consumed_at`

func scanFlow(row pgx.Row) (SSOFlow, error) {
	var f SSOFlow
	err := row.Scan(&f.ID, &f.AdapterID, &f.Purpose, &f.BindHash, &f.ReturnOrigin, &f.NextPath, &f.CallbackURL,
		&f.EntityID, &f.Flow, &f.InitiatedBy, &f.UserID, &f.Result, &f.Failed,
		&f.CreatedAt, &f.ExpiresAt, &f.ConsumedAt)
	return f, err
}

// ByID returns a flow by its state, whatever became of it.
func (s *SSOFlows) ByID(ctx context.Context, flowID string) (SSOFlow, bool, error) {
	f, err := scanFlow(s.db.QueryRow(ctx, `SELECT `+flowColumns+` FROM sso_flows WHERE id = $1`, flowID))
	if errors.Is(err, pgx.ErrNoRows) {
		return SSOFlow{}, false, nil
	}
	if err != nil {
		return SSOFlow{}, false, errs.Wrap(errs.Internal, "Could not read the sign-in.", err)
	}
	return f, true, nil
}

// TakeForCallback claims a live flow for its callback, exactly once: a
// replayed callback finds the flow already taken. The adapter's data is
// cleared as it is handed over.
func (s *SSOFlows) TakeForCallback(ctx context.Context, flowID, adapterID string) (SSOFlow, bool, error) {
	f, err := scanFlow(s.db.QueryRow(ctx, `
		WITH taken AS (
			SELECT id, flow FROM sso_flows
			WHERE id = $1 AND adapter_id = $2 AND expires_at > now()
			  AND consumed_at IS NULL AND handoff_hash IS NULL AND result IS NULL
			FOR UPDATE
		)
		UPDATE sso_flows f SET flow = NULL, result = '{}'::jsonb
		FROM taken WHERE f.id = taken.id
		RETURNING f.id, f.adapter_id, f.purpose, coalesce(f.bind_hash, ''), f.return_origin, f.next_path,
		          f.callback_url, f.entity_id, taken.flow, coalesce(f.initiated_by, ''), coalesce(f.user_id, ''),
		          f.result, f.failed, f.created_at, f.expires_at, f.consumed_at`, flowID, adapterID))
	if errors.Is(err, pgx.ErrNoRows) {
		return SSOFlow{}, false, nil
	}
	if err != nil {
		return SSOFlow{}, false, errs.Wrap(errs.Internal, "Could not read the sign-in.", err)
	}
	return f, true, nil
}

// CreateUnsolicited stores a flow the provider started (SAML IdP-initiated),
// already past its callback. It has no browser binding to check.
func (s *SSOFlows) CreateUnsolicited(ctx context.Context, f SSOFlow) error {
	f.Purpose = FlowSignIn
	if err := s.Create(ctx, f); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE sso_flows SET result = '{}'::jsonb WHERE id = $1`, f.ID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the sign-in.", err)
	}
	return nil
}

// Authenticated records who the callback signed in and the digest of the
// one-time code that finishes the flow in the browser that started it.
func (s *SSOFlows) Authenticated(ctx context.Context, flowID, userID, handoffHash string, result json.RawMessage) error {
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	_, err := s.db.Exec(ctx, `
		UPDATE sso_flows SET user_id = nullif($2, ''), handoff_hash = $3, result = $4,
		       expires_at = least(expires_at, now() + interval '2 minutes')
		WHERE id = $1`,
		flowID, userID, handoffHash, result)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the sign-in.", err)
	}
	return nil
}

// Fail records why a flow did not sign anyone in. Kept for a day so the
// sign-in page can say so, and a test sign-in can show it.
func (s *SSOFlows) Fail(ctx context.Context, flowID string, result json.RawMessage) error {
	_, err := s.db.Exec(ctx, `
		UPDATE sso_flows SET failed = true, result = $2, flow = NULL, handoff_hash = NULL
		WHERE id = $1`, flowID, result)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the sign-in.", err)
	}
	return nil
}

// Complete consumes the one-time code, once, within its two minutes.
func (s *SSOFlows) Complete(ctx context.Context, handoffHash string) (SSOFlow, bool, error) {
	f, err := scanFlow(s.db.QueryRow(ctx, `
		UPDATE sso_flows SET consumed_at = now(), handoff_hash = NULL
		WHERE handoff_hash = $1 AND consumed_at IS NULL AND expires_at > now()
		RETURNING `+flowColumns, handoffHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return SSOFlow{}, false, nil
	}
	if err != nil {
		return SSOFlow{}, false, errs.Wrap(errs.Internal, "Could not finish the sign-in.", err)
	}
	return f, true, nil
}

// UseOnce records a provider's one-time identifier, and reports false if it
// was already used — a replayed SAML assertion.
func (s *SSOFlows) UseOnce(ctx context.Context, adapterID, oneTimeID string, until time.Time) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		INSERT INTO sso_replay (adapter_id, one_time_id, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, adapterID, oneTimeID, until)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not record the sign-in.", err)
	}
	return tag.RowsAffected() == 1, nil
}
