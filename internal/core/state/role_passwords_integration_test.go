//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// TestR256_ARolePasswordChangedByHandIsReplacedAtTheNextStart asserts that a
// stored role password is used only while it still works: one altered by hand,
// or left from a database restored onto another server, is replaced at the
// next start rather than locking every replica out (R-256, issue #72).
func TestR256_ARolePasswordChangedByHandIsReplacedAtTheNextStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL := startPostgres(t)

	first, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL, MaxConns: 3})
	require.NoError(t, err)
	require.EqualValues(t, 3, first.Config().MaxConns, "the pool is capped as configured")
	first.Close()

	owner, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(ctx) })
	readStored := func() string {
		var pw string
		require.NoError(t, owner.QueryRow(ctx,
			`SELECT password FROM pando_private.role_passwords WHERE role = $1`, state.AppRole).Scan(&pw))
		return pw
	}
	before := readStored()

	// Unchanged, the next start keeps it.
	again, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL, SkipMigrate: true})
	require.NoError(t, err)
	again.Close()
	require.Equal(t, before, readStored(), "a password that works is kept")

	// Changed outside Pando: the stored one no longer logs in.
	_, err = owner.Exec(ctx, `ALTER ROLE `+state.AppRole+` PASSWORD 'set-by-hand'`)
	require.NoError(t, err)

	db, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL, SkipMigrate: true})
	require.NoError(t, err)
	t.Cleanup(db.Close)
	require.NoError(t, db.Ping(ctx))
	require.NotEqual(t, before, readStored(), "a password that no longer works is replaced and recorded")
	require.NotEqual(t, "set-by-hand", readStored())
}
