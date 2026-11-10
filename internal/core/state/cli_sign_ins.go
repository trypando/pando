package state

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// CLISignIn is a CLI sign-in someone approved in the browser and the CLI has
// not collected yet (R-406, design 06 §3.5).
type CLISignIn struct {
	CodeHash      string
	UserID        string
	ApprovedBy    string
	CodeChallenge string
	TokenName     string

	// TokenKind is what the approval mints: TokenDelegated, acting as UserID
	// (R-058), or TokenAccount, its own principal (R-060).
	TokenKind string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CLISignIns stores approvals.
type CLISignIns struct{ db *DB }

func NewCLISignIns(db *DB) *CLISignIns { return &CLISignIns{db: db} }

// Create stores an approval. Expired ones are removed by the retention job
// (Retention.SSOFlows), not here, as with sign-in flows.
func (c *CLISignIns) Create(ctx context.Context, s CLISignIn) error {
	kind := s.TokenKind
	if kind == "" {
		kind = TokenDelegated
	}
	_, err := c.db.Exec(ctx, `
		INSERT INTO cli_sign_ins (code_hash, user_id, approved_by, code_challenge, token_name, token_kind, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		s.CodeHash, s.UserID, s.ApprovedBy, s.CodeChallenge, s.TokenName, kind, s.ExpiresAt)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the CLI sign-in.", err)
	}
	return nil
}

// Take removes the approval a code digest names and returns it, if it has not
// expired. Removed whatever the caller does next: a code is tried once, so a
// wrong verifier spends it as surely as the right one.
func (c *CLISignIns) Take(ctx context.Context, codeHash string) (CLISignIn, bool, error) {
	var s CLISignIn
	err := c.db.QueryRow(ctx, `
		DELETE FROM cli_sign_ins WHERE code_hash = $1
		RETURNING code_hash, user_id, approved_by, code_challenge, token_name, token_kind, created_at, expires_at`,
		codeHash).Scan(&s.CodeHash, &s.UserID, &s.ApprovedBy, &s.CodeChallenge, &s.TokenName, &s.TokenKind,
		&s.CreatedAt, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CLISignIn{}, false, nil
	}
	if err != nil {
		return CLISignIn{}, false, errs.Wrap(errs.Internal, "Could not read the CLI sign-in.", err)
	}
	return s, true, nil
}
