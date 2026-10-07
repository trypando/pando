package state

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// Idempotency replays the result of a request that has already been made.
//
// For MCP above all (R-262): an agent retries on a timeout, and a retried
// deploy that deploys twice is the failure mode that makes agents dangerous to
// give infrastructure to. A deploy resolves a ref — which means cloning — and
// regularly outlasts a client's patience, so the retry is ordinary rather than
// exceptional.
type Idempotency struct{ db *DB }

func NewIdempotency(db *DB) *Idempotency { return &Idempotency{db: db} }

// Replayed is a stored response.
type Replayed struct {
	StatusCode int
	Body       json.RawMessage
}

// Lookup returns a previous response for this key, if there is one.
func (i *Idempotency) Lookup(ctx context.Context, key, principalID, endpoint string) (Replayed, bool, error) {
	var out Replayed
	err := i.db.QueryRow(ctx, `
		SELECT status_code, body FROM idempotency_keys
		WHERE key = $1 AND principal_id = $2 AND endpoint = $3`,
		key, principalID, endpoint).Scan(&out.StatusCode, &out.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Replayed{}, false, nil
	}
	if err != nil {
		return Replayed{}, false, errs.Wrap(errs.Internal, "Could not check whether this was already done.", err)
	}
	return out, true, nil
}

// Remember stores a response for replay.
//
// ON CONFLICT DO NOTHING: two concurrent requests with the same key both do the
// work — this does not prevent that, and claiming otherwise would be a lie
// about a race it cannot win without locking the endpoint. What it prevents is
// the far more common sequential retry, which is the case that actually happens.
func (i *Idempotency) Remember(ctx context.Context, key, principalID, endpoint string, status int, body []byte) error {
	if len(body) == 0 {
		body = []byte("null")
	}
	_, err := i.db.Exec(ctx, `
		INSERT INTO idempotency_keys (key, principal_id, endpoint, status_code, body)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (key, principal_id, endpoint) DO NOTHING`,
		key, principalID, endpoint, status, body)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record that this was done.", err)
	}
	return nil
}
