package state

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// OAuthClient is an MCP client that registered itself (R-413, design 06
// §3.6). Public: it has no secret.
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time

	// UsedAt is when someone first approved it. Nil for a client nobody has,
	// which retention removes after a day.
	UsedAt *time.Time
}

// OAuthCode is an approval a client has not collected yet.
type OAuthCode struct {
	CodeHash      string
	ClientID      string
	UserID        string
	ApprovedBy    string
	CodeChallenge string
	RedirectURI   string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// OAuthClients stores registered clients.
type OAuthClients struct{ db *DB }

func NewOAuthClients(db *DB) *OAuthClients { return &OAuthClients{db: db} }

// Create stores a client.
func (c *OAuthClients) Create(ctx context.Context, cl OAuthClient) error {
	_, err := c.db.Exec(ctx, `
		INSERT INTO oauth_clients (id, name, redirect_uris) VALUES ($1, $2, $3)`,
		cl.ID, cl.Name, cl.RedirectURIs)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the MCP client.", err)
	}
	return nil
}

// ByID reads a client.
func (c *OAuthClients) ByID(ctx context.Context, id string) (OAuthClient, bool, error) {
	var cl OAuthClient
	err := c.db.QueryRow(ctx, `
		SELECT id, name, redirect_uris, created_at, used_at FROM oauth_clients WHERE id = $1`,
		id).Scan(&cl.ID, &cl.Name, &cl.RedirectURIs, &cl.CreatedAt, &cl.UsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthClient{}, false, nil
	}
	if err != nil {
		return OAuthClient{}, false, errs.Wrap(errs.Internal, "Could not read the MCP client.", err)
	}
	return cl, true, nil
}

// OAuthCodes stores approvals.
type OAuthCodes struct{ db *DB }

func NewOAuthCodes(db *DB) *OAuthCodes { return &OAuthCodes{db: db} }

// Create stores an approval and marks its client used, so retention keeps it.
func (c *OAuthCodes) Create(ctx context.Context, code OAuthCode) error {
	_, err := c.db.Exec(ctx, `
		WITH used AS (
		    UPDATE oauth_clients SET used_at = now() WHERE id = $2 AND used_at IS NULL
		)
		INSERT INTO oauth_codes (code_hash, client_id, user_id, approved_by, code_challenge, redirect_uri, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		code.CodeHash, code.ClientID, code.UserID, code.ApprovedBy, code.CodeChallenge,
		code.RedirectURI, code.ExpiresAt)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the MCP client's sign-in.", err)
	}
	return nil
}

// Take removes the approval a code digest names and returns it. Removed
// whatever the caller does next, as CLISignIns.Take: a code is tried once.
func (c *OAuthCodes) Take(ctx context.Context, codeHash string) (OAuthCode, bool, error) {
	var code OAuthCode
	err := c.db.QueryRow(ctx, `
		DELETE FROM oauth_codes WHERE code_hash = $1
		RETURNING code_hash, client_id, user_id, approved_by, code_challenge, redirect_uri, created_at, expires_at`,
		codeHash).Scan(&code.CodeHash, &code.ClientID, &code.UserID, &code.ApprovedBy, &code.CodeChallenge,
		&code.RedirectURI, &code.CreatedAt, &code.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthCode{}, false, nil
	}
	if err != nil {
		return OAuthCode{}, false, errs.Wrap(errs.Internal, "Could not read the MCP client's sign-in.", err)
	}
	return code, true, nil
}
