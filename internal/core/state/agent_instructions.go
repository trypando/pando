package state

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// AgentInstructions is the company's instructions for agents (R-424): one
// row at most, holding what administrators wrote. Pando's built-in guidance
// is not stored; it comes with the binary.
type AgentInstructions struct{ db *DB }

// NewAgentInstructions returns the store.
func NewAgentInstructions(db *DB) *AgentInstructions { return &AgentInstructions{db: db} }

// StoredInstructions is the row as saved. A zero UpdatedAt means nobody has
// written any.
type StoredInstructions struct {
	Body      string
	UpdatedBy string
	UpdatedAt time.Time
}

// Load returns the company's instructions, or the zero value when none were
// ever saved.
func (a *AgentInstructions) Load(ctx context.Context) (StoredInstructions, error) {
	var out StoredInstructions
	err := a.db.QueryRow(ctx,
		`SELECT body, updated_by, updated_at FROM agent_instructions WHERE id = 1`).
		Scan(&out.Body, &out.UpdatedBy, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredInstructions{}, nil
	}
	if err != nil {
		return StoredInstructions{}, errs.Wrap(errs.Internal, "Could not read the instructions for agents.", err)
	}
	return out, nil
}

// Save replaces the company's instructions. An empty body is kept as a row,
// so who cleared them and when stays readable.
func (a *AgentInstructions) Save(ctx context.Context, body, updatedBy string) (StoredInstructions, error) {
	var out StoredInstructions
	err := a.db.QueryRow(ctx, `
		INSERT INTO agent_instructions (id, body, updated_by) VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET body = EXCLUDED.body, updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING body, updated_by, updated_at`, body, updatedBy).
		Scan(&out.Body, &out.UpdatedBy, &out.UpdatedAt)
	if err != nil {
		return StoredInstructions{}, errs.Wrap(errs.Internal, "Could not save the instructions for agents.", err)
	}
	return out, nil
}
