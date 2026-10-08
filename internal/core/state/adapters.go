package state

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/errs"
)

// AdapterConfig is a configured adapter instance.
type AdapterConfig struct {
	ID        string          `json:"id"`
	Category  string          `json:"category"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"-"`
	IsDefault bool            `json:"is_default"`
	Enabled   bool            `json:"enabled"`

	// UpdatedAt is when the configuration last changed. Adapters are loaded
	// at startup (R-253), so one changed after Pando started is not what is
	// running.
	UpdatedAt time.Time `json:"-"`
}

// Adapters reads configured adapter instances.
type Adapters struct{ db *DB }

func NewAdapters(db *DB) *Adapters { return &Adapters{db: db} }

// List returns configured adapters.
//
// Config is loaded but never serialized: an adapter's configuration can hold
// credentials, and GET /adapters is a list of what exists, not a dump of how it
// authenticates.
func (a *Adapters) List(ctx context.Context) ([]AdapterConfig, error) {
	rows, err := a.db.Query(ctx, `
		SELECT id, category, kind, name, config, is_default, enabled, updated_at
		FROM adapter_configs ORDER BY category, name`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the configured adapters.", err)
	}
	defer rows.Close()

	var out []AdapterConfig
	for rows.Next() {
		var c AdapterConfig
		if err := rows.Scan(&c.ID, &c.Category, &c.Kind, &c.Name, &c.Config, &c.IsDefault, &c.Enabled, &c.UpdatedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the configured adapters.", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Upsert stores an adapter configuration.
func (a *Adapters) Upsert(ctx context.Context, c AdapterConfig) error {
	cfg := c.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	_, err := a.db.Exec(ctx, `
		INSERT INTO adapter_configs (id, category, kind, name, config, is_default, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			kind = EXCLUDED.kind, name = EXCLUDED.name, config = EXCLUDED.config,
			is_default = EXCLUDED.is_default, enabled = EXCLUDED.enabled, updated_at = now()`,
		c.ID, c.Category, c.Kind, c.Name, cfg, c.IsDefault, c.Enabled)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "adapter_configs_one_ai_adapter_per_kind" {
			// One AI adapter per provider (R-259). An assignment's model is how
			// one provider serves two models.
			return errs.Newf(errs.ValidInvalid,
				"This installation already has a %s AI adapter, and Pando allows one AI adapter per provider.", c.Kind).
				WithRemedy("Change the existing adapter instead of adding a second one. To run some AI functions on " +
					"a different model, set the model on those functions' assignments.")
		}
		return errs.Wrap(errs.Internal, "Could not save the adapter configuration.", err)
	}
	return nil
}

// DeleteInCategory removes an adapter configuration of one category, with its
// credentials (ON DELETE CASCADE). The category is part of the condition so a
// route for one kind of adapter cannot remove another kind's — disconnecting
// a source cannot remove the runtime.
func (a *Adapters) DeleteInCategory(ctx context.Context, id, category string) error {
	tag, err := a.db.Exec(ctx,
		`DELETE FROM adapter_configs WHERE id = $1 AND category = $2`, id, category)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the adapter configuration.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.Newf(errs.NotFound, "There is no %s adapter %q on this installation.", category, id)
	}
	return nil
}

// Policy reads and writes the host policy singleton.
type Policy struct{ db *DB }

func NewPolicy(db *DB) *Policy { return &Policy{db: db} }

// Load returns the current policy.
//
// Read per evaluation rather than cached, because R-274 says policy applies to a
// running install: a cached policy would keep allowing, or keep denying, for
// however long the cache lived.
func (p *Policy) Load(ctx context.Context) (policy.Document, error) {
	var body []byte
	err := p.db.QueryRow(ctx, `SELECT body FROM host_policy WHERE id = 1`).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy.Default(), nil
	}
	if err != nil {
		return policy.Document{}, errs.Wrap(errs.Internal, "Could not read the installation's policy.", err)
	}

	var doc policy.Document
	if err := json.Unmarshal(body, &doc); err != nil {
		return policy.Document{}, errs.Wrap(errs.Internal, "The installation's policy could not be read.", err)
	}
	return doc, nil
}

// Save replaces the policy. One row, one transaction, one audit event (R-274).
func (p *Policy) Save(ctx context.Context, doc policy.Document, updatedBy string) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not encode the policy.", err)
	}
	_, err = p.db.Exec(ctx, `
		INSERT INTO host_policy (id, body, updated_by) VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET body = EXCLUDED.body, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		body, updatedBy)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not save the policy.", err)
	}
	return nil
}

// Allocations reports what apps already hold on a runtime adapter (R-242).
type Allocations struct{ db *DB }

func NewAllocations(db *DB) *Allocations { return &Allocations{db: db} }

// AllocatedOn sums the resources of apps pinned to a runtime adapter.
//
// Excludes the app being planned: replanning must not count an app's own
// current allocation against itself, which would make any app that already fits
// look like it no longer does.
//
// Only apps that are actually running or deploying count. A stopped app holds no
// CPU or memory, and counting it would refuse deploys to make room for something
// that is not there.
func (a *Allocations) AllocatedOn(ctx context.Context, adapterRef, excludeAppID string) (planner.Allocation, error) {
	var alloc planner.Allocation
	err := a.db.QueryRow(ctx, `
		SELECT
			coalesce(sum((r.body->'resources'->>'cpu_millis')::int), 0),
			coalesce(sum((r.body->'resources'->>'memory_bytes')::bigint), 0),
			coalesce(sum((r.body->'resources'->>'disk_bytes')::bigint), 0),
			coalesce(sum((r.body->'retention'->>'log_bytes')::bigint), 0)
		FROM apps a
		JOIN spec_revisions r ON r.id = a.pinned_spec_id
		WHERE a.deleted_at IS NULL
		  AND a.id <> $2
		  AND a.state IN ('running', 'degraded', 'deploying')
		  AND r.body->'runtime'->>'adapter_ref' = $1`,
		adapterRef, excludeAppID).Scan(&alloc.CPUMillis, &alloc.MemoryBytes, &alloc.DiskBytes, &alloc.LogBytes)
	if err != nil {
		return planner.Allocation{}, errs.Wrap(errs.Internal, "Could not read how much is already allocated.", err)
	}
	return alloc, nil
}

var _ planner.Allocations = (*Allocations)(nil)
