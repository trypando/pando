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

// Replaced is the adapter that stopped being its category's default when
// another was saved as the default. Zero when none did.
type Replaced struct {
	ID   string
	Name string
}

// defaultLockKey namespaces Save's advisory locks from any other.
const defaultLockKey = "pando.adapter_default:"

// Upsert stores an adapter configuration. See Save.
func (a *Adapters) Upsert(ctx context.Context, c AdapterConfig) error {
	_, err := a.Save(ctx, c)
	return err
}

// Save stores an adapter configuration, and returns the adapter it replaced as
// its category's default, if any.
//
// Saving one as the default is a handover (issue #220): the previous default
// is cleared and the new one set in one transaction, so there is never a
// moment with two defaults or none. The category's default is locked for the
// transaction, so two saves racing to be the default hand over one after the
// other rather than both clearing the same row.
func (a *Adapters) Save(ctx context.Context, c AdapterConfig) (Replaced, error) {
	cfg := c.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return Replaced{}, saveRefusal(c, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	replaced, err := handOver(ctx, tx, c)
	if err == nil {
		_, err = tx.Exec(ctx, `
			INSERT INTO adapter_configs (id, category, kind, name, config, is_default, enabled)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (id) DO UPDATE SET
				kind = EXCLUDED.kind, name = EXCLUDED.name, config = EXCLUDED.config,
				is_default = EXCLUDED.is_default, enabled = EXCLUDED.enabled, updated_at = now()`,
			c.ID, c.Category, c.Kind, c.Name, cfg, c.IsDefault, c.Enabled)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return Replaced{}, saveRefusal(c, err)
	}
	return replaced, nil
}

// handOver clears the default of the category c is saved into, when c is to
// be the default, and returns the adapter that was. An existing row keeps its
// category, which ON CONFLICT does not change, so that is the category read.
//
// The lock is its own statement: a statement's snapshot is taken when it
// starts, so an UPDATE that waited for the lock inside itself would not see
// the default the holder committed.
func handOver(ctx context.Context, tx pgx.Tx, c AdapterConfig) (Replaced, error) {
	var r Replaced
	if !c.IsDefault {
		return r, nil
	}
	var category string
	if err := tx.QueryRow(ctx, `
		SELECT k.category
		FROM (SELECT coalesce((SELECT category FROM adapter_configs WHERE id = $1), $2) AS category) k,
		     pg_advisory_xact_lock(hashtextextended($3 || k.category, 0))`,
		c.ID, c.Category, defaultLockKey).Scan(&category); err != nil {
		return r, err
	}
	err := tx.QueryRow(ctx, `
		UPDATE adapter_configs SET is_default = false, updated_at = now()
		WHERE category = $1 AND is_default AND id <> $2
		RETURNING id, name`, category, c.ID).Scan(&r.ID, &r.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Replaced{}, nil
	}
	return r, err
}

// saveFailed is what a save the database failed for a reason of its own says.
const saveFailed = "Could not save the adapter configuration."

// saveRefusal says why the database refused an adapter configuration (R-105).
// A constraint is a rule about what may be saved, so breaking one is the
// request's to fix, and the refusal names the rule rather than saying only
// that saving failed (issue #220).
func saveRefusal(c AdapterConfig, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return errs.Wrap(errs.Internal, saveFailed, err)
	}
	switch pgErr.ConstraintName {
	case "adapter_configs_one_ai_adapter_per_kind":
		// One AI adapter per provider (R-259). An assignment's model is how
		// one provider serves two models.
		return errs.Newf(errs.ValidInvalid,
			"This installation already has a %s AI adapter, and Pando allows one AI adapter per provider.", c.Kind).
			WithRemedy("Change the existing adapter instead of adding a second one. To run some AI functions on " +
				"a different model, set the model on those functions' assignments.")
	case "adapter_configs_one_default_per_category":
		// Save hands the default over under a lock, so only a default set
		// some other way in between gets here.
		return errs.Newf(errs.ValidInvalid,
			"Another %s adapter became the default while %s was being saved, and each category has one default.",
			c.Category, c.ID).
			WithRemedy("Save it again to make it the default in place of that one.")
	case "adapter_configs_registry_url_no_credential", "adapter_configs_audit_sink_url_no_credential":
		return errs.Newf(errs.ValidInvalid,
			"The address of the %s adapter %s contains a username or password, and Pando stores addresses unencrypted.",
			c.Category, c.ID).
			WithRemedy(`Remove them from the address and send them in the request's "credentials" object instead. Pando stores that encrypted.`)
	}
	switch pgErr.Code {
	case "23514": // check_violation
		return errs.Newf(errs.ValidInvalid,
			"Pando could not save the %s adapter %s, because its settings break the rule %s.",
			c.Category, c.ID, pgErr.ConstraintName).
			WithRemedy(`A credential goes in the request's "credentials" object, never in "config". Otherwise, correct the setting that rule names and save again.`)
	case "23505": // unique_violation
		return errs.Newf(errs.ValidInvalid,
			"Pando could not save the %s adapter %s, because the rule %s allows only one, and another adapter already has it.",
			c.Category, c.ID, pgErr.ConstraintName).
			WithRemedy("Change that adapter instead, or remove it first. GET /api/v1/adapters lists them.")
	}
	return errs.Wrap(errs.Internal, saveFailed, err)
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
