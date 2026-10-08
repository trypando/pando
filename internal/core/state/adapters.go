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
//
// Read live on every call, never from a cache: this is the plan-time check of
// R-242, and a stale sum is a deploy let through onto a full host. What each
// app reserves is copied from its pinned revision onto the app by a trigger
// when the pin moves (migration 60), so the sum is an index-only scan of
// apps_allocation_idx rather than a JSON walk of every running app's revision
// (issue #72). Which apps count is still decided here, from state.
//
// A deploy in flight counts too, from the moment it is created: what its
// revision asks for is reserved on its runtime until it ends (migration 62),
// so a deploy not yet pinned is not room for another. An app counts once,
// at the larger of what its pin holds and what its deploy reserves, resource
// by resource: a redeploy of a running app is not two apps.
func (a *Allocations) AllocatedOn(ctx context.Context, adapterRef, excludeAppID string) (planner.Allocation, error) {
	var alloc planner.Allocation
	err := a.db.QueryRow(ctx, `
		SELECT
			coalesce(sum(cpu), 0)::bigint,
			coalesce(sum(memory), 0)::bigint,
			coalesce(sum(disk), 0)::bigint,
			coalesce(sum(logs), 0)::bigint
		FROM (
		    SELECT held.app_id, max(held.cpu) AS cpu, max(held.memory) AS memory,
		           max(held.disk) AS disk, max(held.logs) AS logs
		    FROM (
		        SELECT a.id AS app_id, a.alloc_cpu_millis AS cpu, a.alloc_memory_bytes AS memory,
		               a.alloc_disk_bytes AS disk, a.alloc_log_bytes AS logs
		        FROM apps a
		        WHERE a.deleted_at IS NULL
		          AND a.state IN ('running', 'degraded', 'deploying')
		          AND a.alloc_runtime_ref = $1
		          AND a.id <> $2
		        UNION ALL
		        SELECT d.app_id, d.reserve_cpu_millis, d.reserve_memory_bytes,
		               d.reserve_disk_bytes, d.reserve_log_bytes
		        FROM deployments d
		        WHERE d.status IN ('pending', 'building', 'applying')
		          AND d.reserve_runtime_ref = $1
		          AND d.app_id <> $2
		    ) held
		    GROUP BY held.app_id
		) per_app`,
		adapterRef, excludeAppID).Scan(&alloc.CPUMillis, &alloc.MemoryBytes, &alloc.DiskBytes, &alloc.LogBytes)
	if err != nil {
		return planner.Allocation{}, errs.Wrap(errs.Internal, "Could not read how much is already allocated.", err)
	}
	return alloc, nil
}

// capacityLockKey namespaces Hold's advisory locks from any other.
const capacityLockKey = "pando.capacity:"

// Hold runs fn with the runtime's capacity to itself (R-242): no other Hold on
// the same runtime, on any replica, runs until fn returns. A deploy's plan
// check and its creation run inside one, so the second of two deploys racing
// for the last room is planned against the first one's reservation rather
// than beside it.
//
// A transaction-scoped advisory lock, released when the transaction ends and
// with it if the connection is lost, so a lock cannot outlive the call that
// took it. On a connection of its own, outside the pool: fn does its work on
// pooled connections, and callers waiting for the lock while each holding a
// pooled one could leave the holder none to finish with.
func (a *Allocations) Hold(ctx context.Context, runtimeRef string, fn func(context.Context) error) error {
	conn, err := pgx.ConnectConfig(ctx, a.db.Config().ConnConfig.Copy())
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not reserve room on the runtime.", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not reserve room on the runtime.", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, capacityLockKey+runtimeRef); err != nil {
		return errs.Wrap(errs.Internal, "Could not reserve room on the runtime.", err)
	}
	return fn(ctx)
}

var _ planner.Allocations = (*Allocations)(nil)
