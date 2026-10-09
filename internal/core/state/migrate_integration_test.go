//go:build integration

package state_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Every start migrates, so migrating a database that is already current is the
// ordinary case rather than an edge: it must change nothing and say so quietly.
// `pando migrate` is the same call.
func TestMigratingACurrentDatabaseChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, password := statetest.Database(t)

	require.NoError(t, state.Migrate(ctx, ownerURL))
	require.NoError(t, state.Migrate(ctx, ownerURL), "a second run finds nothing to do")

	// The version is read back from the database rather than remembered by
	// the process, which is how a copy that was never migrated here knows it.
	db, err := state.ConnectCopy(ctx, ownerURL, password)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	require.NotZero(t, db.SchemaVersion())
}

// A copy is served only as the application role, with the role's real
// password. Anything else is refused at connect rather than at the first query.
func TestACopyIsNotServedWithTheWrongPassword(t *testing.T) {
	t.Parallel()
	ownerURL, _ := statetest.Database(t)

	_, err := state.ConnectCopy(context.Background(), ownerURL, secret.New("not-the-password"))
	require.Error(t, err)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
}

// A database that was never migrated has no version to read, and is refused
// rather than served as version zero. In a Postgres of its own, because this is
// state.Connect, which a shared cluster must never see (statetest).
func TestAnUnmigratedDatabaseIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: startPostgres(t), SkipMigrate: true})
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "schema version")
}

// TestR027_ACopyWhoseAuditLogTheAppRoleOwnsIsRefused asserts R-027 on the path
// tests connect by. The copy's grants are the template's, but ownership is
// checked again on every copy: a copy the application role owns the audit
// table of is one where it can grant itself UPDATE, and it is not served.
func TestR027_ACopyWhoseAuditLogTheAppRoleOwnsIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, password := statetest.Database(t)
	asOwner(t, ownerURL, `ALTER TABLE audit_events OWNER TO `+state.AppRole)

	_, err := state.ConnectCopy(ctx, ownerURL, password)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "not tamper-proof")
}

// A database that was never migrated is not a copy of a prepared one, and
// ConnectCopy refuses it rather than serving an empty schema.
func TestAnUnmigratedDatabaseIsNotServedAsACopy(t *testing.T) {
	t.Parallel()
	ownerURL, password := statetest.Database(t)
	empty := emptyBeside(t, ownerURL)

	_, err := state.ConnectCopy(context.Background(), empty, password)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "schema version")
}

// A migration that failed partway leaves the schema marked dirty. Migrating
// again is refused with what happened and what to do, whether a server is
// starting or a test suite is preparing its template.
func TestADirtySchemaIsRefusedWithWhatToDo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)
	asOwner(t, ownerURL, `UPDATE schema_migrations SET dirty = true`)

	err := state.Migrate(ctx, ownerURL)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "marked dirty")
	require.Contains(t, errs.As(err).Remedy, "Restore from a backup")

	// Refused before AppRole is touched, so this is safe on a shared cluster.
	_, err = state.PrepareTemplate(ctx, ownerURL)
	require.ErrorContains(t, err, "marked dirty")
}

// TestR354_ADatabaseANewerPandoMigratedIsRefused asserts R-354: an older
// Pando — put back by a Compose file or IaC that still names it — refuses a
// database a newer one migrated, saying so, instead of "migration failed".
func TestR354_ADatabaseANewerPandoMigratedIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)
	asOwner(t, ownerURL, `UPDATE schema_migrations SET version = version + 1000`)

	err := state.Migrate(ctx, ownerURL)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "migrated by a newer version of Pando")
	require.Contains(t, errs.As(err).Remedy, "restore the backup taken before the upgrade")

	// Refused before AppRole is touched, like a dirty schema.
	_, err = state.PrepareTemplate(ctx, ownerURL)
	require.ErrorContains(t, err, "migrated by a newer version of Pando")
}

// TestR356_TheAdministratorHoldsInstallUpgradeAndAgentsDoNot asserts
// migration 000042: the built-in Administrator gains install.upgrade (R-081
// says only a migration may), and a stored policy denies it to agents as
// policy.Default() does.
func TestR356_TheAdministratorHoldsInstallUpgradeAndAgentsDoNot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)

	conn, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var held bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT 'install.upgrade' = ANY (verbs) FROM roles WHERE id = 'role_administrator'`).Scan(&held))
	require.True(t, held)

	var elsewhere int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM roles WHERE id <> 'role_administrator' AND 'install.upgrade' = ANY (verbs)`).Scan(&elsewhere))
	require.Zero(t, elsewhere, "no other built-in role holds it")
}

// TestR385_OnlyTheAdministratorHoldsInstallAuditExport asserts migration
// 000067: sending the audit log off the installation is the Administrator's
// alone among built-in roles, and the auditor, who reads the log, does not
// gain it.
func TestR385_OnlyTheAdministratorHoldsInstallAuditExport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)

	conn, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var held bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT 'install.audit.export' = ANY (verbs) FROM roles WHERE id = 'role_administrator'`).Scan(&held))
	require.True(t, held)

	var elsewhere int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM roles WHERE id <> 'role_administrator' AND 'install.audit.export' = ANY (verbs)`).Scan(&elsewhere))
	require.Zero(t, elsewhere, "no other built-in role holds it, the auditor included")
}

// asOwner runs one statement against a database as its owner.
func asOwner(t *testing.T, ownerURL, stmt string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, stmt)
	require.NoError(t, err)
}

// emptyBeside creates an empty database in the same cluster as ownerURL's and
// returns its URL.
func emptyBeside(t *testing.T, ownerURL string) string {
	t.Helper()
	u, err := url.Parse(ownerURL)
	require.NoError(t, err)
	name := strings.TrimPrefix(u.Path, "/") + "_empty"
	asOwner(t, ownerURL, `CREATE DATABASE "`+name+`"`)
	u.Path = "/" + name
	return u.String()
}
