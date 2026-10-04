//go:build integration

package state_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
)

func scalar[T any](t *testing.T, dbURL, query string) T {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	var v T
	require.NoError(t, conn.QueryRow(ctx, query).Scan(&v))
	return v
}

// TestR359_ARollbackRestoresTheDatabaseTheOldVersionLeft asserts the database
// half of R-359: the copy taken while Pando is stopped is what a failed
// upgrade goes back to, migrations and all undone.
func TestR359_ARollbackRestoresTheDatabaseTheOldVersionLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)
	t.Cleanup(func() { _ = state.DropSnapshot(context.Background(), ownerURL) })

	require.NoError(t, state.CanCopyDatabase(ctx, ownerURL), "the test cluster's account is a superuser, like the bundled Compose file's")
	before := scalar[int](t, ownerURL, `SELECT version FROM schema_migrations`)

	require.NoError(t, state.Snapshot(ctx, ownerURL))

	// What a newer version's migrations do.
	asOwner(t, ownerURL, `UPDATE schema_migrations SET version = version + 1`)
	asOwner(t, ownerURL, `CREATE TABLE from_the_new_version (id int)`)

	require.NoError(t, state.RestoreSnapshot(ctx, ownerURL))
	require.Equal(t, before, scalar[int](t, ownerURL, `SELECT version FROM schema_migrations`))
	require.False(t, scalar[bool](t, ownerURL, `SELECT to_regclass('from_the_new_version') IS NOT NULL`))

	// The copy is gone once restored, and the database still migrates and serves.
	snap, err := state.SnapshotName(ownerURL)
	require.NoError(t, err)
	require.False(t, scalar[bool](t, ownerURL, `SELECT EXISTS (SELECT FROM pg_database WHERE datname = '`+snap+`')`))
	require.NoError(t, state.Migrate(ctx, ownerURL))

	// Taking one again replaces it, and dropping it twice is not an error.
	require.NoError(t, state.Snapshot(ctx, ownerURL))
	require.NoError(t, state.Snapshot(ctx, ownerURL))
	require.NoError(t, state.DropSnapshot(ctx, ownerURL))
	require.NoError(t, state.DropSnapshot(ctx, ownerURL))
}

// An external database's account without CREATEDB is told so, by name, before
// anything stops (R-359, design 00 §1.1).
func TestR359_AnAccountThatCannotCopyTheDatabaseIsRefusedByName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)

	u, err := url.Parse(ownerURL)
	require.NoError(t, err)
	role := "nocreatedb_" + u.Path[1:]
	asOwner(t, ownerURL, `CREATE ROLE "`+role+`" LOGIN PASSWORD 'pw' NOCREATEDB`)
	t.Cleanup(func() {
		asOwner(t, ownerURL, `DROP OWNED BY "`+role+`"`)
		asOwner(t, ownerURL, `DROP ROLE IF EXISTS "`+role+`"`)
	})
	u.User = url.UserPassword(role, "pw")

	err = state.CanCopyDatabase(ctx, u.String())
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "CREATEDB")
	require.Contains(t, errs.As(err).Remedy, "changing Pando's image")
}
