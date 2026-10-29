package state

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// Ports allocates the host ports that port-mode routing hands out.
//
// [P] — unspecified in the requirements. R-166 prefers subdomain and falls back
// to path, and the loopback adapter that ships as the laptop default supports
// neither: it is port mode only. So an install using it has to get a port from
// somewhere, and nothing said where. See docs/plan/open-decisions.md O-15.
//
// The rule is the lowest free port in a configured range. Not random, so an app
// keeps its port across a rebuild and a bookmark keeps working; reused rather
// than ever-increasing, so a deleted app's port comes back.
//
// It is a row in a table rather than a number derived from existing specs. The
// derived version raced: detection runs in the background per app, and three
// apps added together had two of them compute the same "lowest free" port
// before either had written anything down. A unique constraint cannot be raced
// — one writer wins and the other is told — which is the same reason every
// other invariant here lives in the schema.
type Ports struct{ db *DB }

// NewPorts returns a port allocator over db.
func NewPorts(db *DB) *Ports { return &Ports{db: db} }

// allocateAttempts bounds the retry loop.
//
// A retry means another app took the port between this one choosing it and
// claiming it, which needs one more pass through a range that is now smaller.
// Several in a row means sustained concurrent adds, not a stuck loop.
const allocateAttempts = 10

// Allocate reserves the lowest free port in [from, to] for an app.
//
// Idempotent: an app that already holds a port on this adapter gets the same
// one back. Re-detection (R-022) must not consume a second port and leave every
// bookmark pointing at nothing.
func (p *Ports) Allocate(ctx context.Context, adapterRef, appID string, from, to int) (int, error) {
	if from <= 0 || to < from || to > 65535 {
		return 0, errs.Newf(errs.Internal,
			"The configured port range %d-%d is not usable.", from, to)
	}

	// A deleted app's port comes back. Deleting an app archives it — the row
	// stays, with deleted_at set — so ON DELETE CASCADE never fires and the
	// allocation outlived the app. An install filled its range with apps that
	// no longer exist, and the next app was refused a port. Reclaimed here
	// rather than only at deletion, so a range already full of them recovers
	// without a migration. InUse has always ignored these rows, which is why
	// nothing was listening on those ports either.
	if _, err := p.db.Exec(ctx, `
		DELETE FROM port_allocations
		WHERE adapter_ref = $1
		  AND app_id IN (SELECT id FROM apps WHERE deleted_at IS NOT NULL)`, adapterRef); err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not release the ports of deleted apps.", err)
	}

	var existing int
	err := p.db.QueryRow(ctx,
		`SELECT port FROM port_allocations WHERE adapter_ref = $1 AND app_id = $2`,
		adapterRef, appID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, errs.Wrap(errs.Internal, "Could not read this app's port.", err)
	}

	for attempt := 0; attempt < allocateAttempts; attempt++ {
		// One statement: choose and claim together. Two concurrent callers can
		// still pick the same port, and then exactly one INSERT survives — the
		// other returns no rows and tries again against a range it now sees
		// correctly.
		//
		// Free means free on every adapter, and not the port of any live app:
		// the proxy listens and resolves by number alone, so a port is one
		// app's on every adapter (migration 97), and an app pinned before pins
		// claimed their port holds one with no allocation behind it (issue
		// #348).
		var port int
		err := p.db.QueryRow(ctx, `
			INSERT INTO port_allocations (adapter_ref, port, app_id)
			SELECT $1, candidate, $2
			FROM generate_series($3::int, $4::int) AS candidate
			WHERE NOT EXISTS (
				SELECT 1 FROM port_allocations existing WHERE existing.port = candidate
			)
			AND NOT EXISTS (
				SELECT 1 FROM apps
				WHERE apps.deleted_at IS NULL AND apps.id <> $2 AND apps.address_port = candidate
			)
			ORDER BY candidate
			LIMIT 1
			ON CONFLICT (port) DO NOTHING
			RETURNING port
		`, adapterRef, appID, from, to).Scan(&port)

		if err == nil {
			return port, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			// Either the range is full, or a concurrent writer took the
			// candidate. Distinguishing them costs a query; retrying settles it
			// either way, and the loop ends by reporting exhaustion.
			continue
		}
		return 0, errs.Wrap(errs.Internal, "Could not assign a port to this app.", err)
	}

	return 0, errs.Newf(errs.CapacityNoFreePort,
		"Every port between %d and %d is already assigned to an app.", from, to).
		WithDetail("range_start", from).
		WithDetail("range_end", to).
		WithRemedy("Delete an app that is no longer needed, or widen the port range with " +
			"PANDO_SERVER_PORT_RANGE_START and PANDO_SERVER_PORT_RANGE_END.")
}

// InUse lists every port currently allocated to an app.
//
// What the proxy opens listeners on (design 03 §4.2). Allocations rather than
// running apps, deliberately: a port belongs to an app from the moment it is
// allocated until the app is gone, and an app that is stopped or mid-deploy
// should answer "this app isn't running right now" at its own address rather
// than refuse the connection — the two are very different things to debug.
func (p *Ports) InUse(ctx context.Context) ([]int, error) {
	rows, err := p.db.Query(ctx,
		`SELECT a.port FROM port_allocations a
		 JOIN apps ON apps.id = a.app_id AND apps.deleted_at IS NULL
		 ORDER BY a.port`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the ports apps are using.", err)
	}
	defer rows.Close()

	var ports []int
	for rows.Next() {
		var port int
		if err := rows.Scan(&port); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the ports apps are using.", err)
		}
		ports = append(ports, port)
	}
	return ports, rows.Err()
}
