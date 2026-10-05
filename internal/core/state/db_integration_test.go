//go:build integration

package state_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
)

// startPostgres brings up a real Postgres and returns an owner connection URL.
//
// A cluster of the test's own, for the tests here: each creates roles, changes
// ownership or connects twice, and AppRole's password and every role belong to
// the cluster rather than to one database. Everything else uses connected.
func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	container, err := statetest.Postgres(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	return dsn
}

// TestR027_AuditLogIsNotRewritable asserts R-027.
//
// This is phase 0's acceptance condition: an audit event can be written and
// provably not modified. The proof has to be attempted-and-refused, not
// inspected. Reading the grant table would only show that the REVOKE ran, and a
// revoke that ran is not the same as a privilege that cannot be regained — see
// TestR027_AnOwningRoleCanUndoTheRevoke.
func TestR027_AuditLogIsNotRewritable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL := startPostgres(t)

	db, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL})
	require.NoError(t, err)
	t.Cleanup(db.Close)

	require.NoError(t, audit.New(db.Pool).Write(ctx, audit.Event{
		PrincipalKind: audit.KindUser,
		PrincipalID:   "usr_01HQ8",
		Action:        "app.deploy",
		AppID:         "app_01HQ8",
		Detail:        map[string]any{"spec_revision": 1},
	}))

	var count int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&count))
	require.Equal(t, 1, count, "the event should have been written")

	// The application role must be refused both ways.
	_, err = db.Exec(ctx, `UPDATE audit_events SET action = 'app.nothing-happened'`)
	require.Error(t, err, "UPDATE on audit_events must be refused")
	require.Contains(t, err.Error(), "permission denied")

	_, err = db.Exec(ctx, `DELETE FROM audit_events`)
	require.Error(t, err, "DELETE on audit_events must be refused")
	require.Contains(t, err.Error(), "permission denied")

	_, err = db.Exec(ctx, `TRUNCATE audit_events`)
	require.Error(t, err, "TRUNCATE on audit_events must be refused")

	// The record is untouched and still readable.
	var action string
	require.NoError(t, db.QueryRow(ctx, `SELECT action FROM audit_events`).Scan(&action))
	require.Equal(t, "app.deploy", action)
}

// TestR027_ApplicationRoleDoesNotOwnTheSchema asserts the mechanism behind the
// test above, because it is the part a later refactor would quietly undo.
//
// REVOKE does work against an owner — see TestR027_AnOwningRoleCanUndoTheRevoke
// for what actually goes wrong. The property that has to hold is that the role
// serving traffic owns nothing, because an owner can restore its own privileges
// whenever it likes.
func TestR027_ApplicationRoleDoesNotOwnTheSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL := startPostgres(t)

	db, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL})
	require.NoError(t, err)
	t.Cleanup(db.Close)

	var currentUser, tableOwner string
	require.NoError(t, db.QueryRow(ctx, `SELECT current_user`).Scan(&currentUser))
	require.NoError(t, db.QueryRow(ctx,
		`SELECT tableowner FROM pg_tables WHERE tablename = 'audit_events'`).Scan(&tableOwner))

	require.Equal(t, state.AppRole, currentUser, "traffic should be served as the restricted role")
	require.NotEqual(t, currentUser, tableOwner,
		"the application role must not own audit_events — an owner can re-grant itself UPDATE")
}

// TestR027_AuditRemainsImmutableAcrossRestarts asserts that the grant policy is
// re-applied rather than assumed, so a grant that drifts is corrected.
func TestR027_AuditRemainsImmutableAcrossRestarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL := startPostgres(t)

	db, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL})
	require.NoError(t, err)
	db.Close()

	// Simulate drift: an operator, or a bad migration, hands the app role
	// UPDATE on the audit log.
	owner, err := pgxpool.New(ctx, ownerURL)
	require.NoError(t, err)
	_, err = owner.Exec(ctx, fmt.Sprintf(`GRANT UPDATE, DELETE ON audit_events TO %s`, state.AppRole))
	require.NoError(t, err)
	owner.Close()

	// Starting again must put it back.
	db2, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL})
	require.NoError(t, err)
	t.Cleanup(db2.Close)

	require.NoError(t, audit.New(db2.Pool).Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem,
		PrincipalID:   "system",
		Action:        "server.start",
	}))

	_, err = db2.Exec(ctx, `UPDATE audit_events SET action = 'tampered'`)
	require.Error(t, err, "a drifted grant should have been revoked on restart")
}

// TestR027_AnOwningRoleCanUndoTheRevoke documents why two roles are required,
// by demonstrating the attack the design prevents.
//
// The intuitive claim — that a table's owner ignores REVOKE — is false: after
// REVOKE UPDATE, has_table_privilege reports false even for the owner. What is
// true, and worse, is that an owner holds grant option implicitly and can hand
// the privilege back to itself in one statement, from exactly the connection an
// attacker would already be using.
//
// This test deliberately builds the broken configuration and proves it is
// broken. If a future change lets Pando serve traffic as a schema-owning role,
// the design note this test guards will have been lost.
func TestR027_AnOwningRoleCanUndoTheRevoke(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL := startPostgres(t)

	owner, err := pgxpool.New(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(owner.Close)

	const badRole = "owning_role"
	for _, stmt := range []string{
		`CREATE TABLE audit_events (id bigserial PRIMARY KEY, action text)`,
		`CREATE ROLE ` + badRole + ` LOGIN PASSWORD 'probe-password'`,
		`GRANT USAGE ON SCHEMA public TO ` + badRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON audit_events TO ` + badRole,
		`ALTER TABLE audit_events OWNER TO ` + badRole,
		`REVOKE UPDATE, DELETE, TRUNCATE ON audit_events FROM ` + badRole,
		`INSERT INTO audit_events (action) VALUES ('app.deploy')`,
	} {
		_, err := owner.Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	// The revoke appears to have worked.
	var canUpdate bool
	require.NoError(t, owner.QueryRow(ctx,
		`SELECT has_table_privilege($1, 'audit_events', 'UPDATE')`, badRole).Scan(&canUpdate))
	require.False(t, canUpdate, "REVOKE does take effect against an owner")

	// It has not. The owning role restores it and rewrites history.
	badURL, err := urlAs(ownerURL, badRole, "probe-password")
	require.NoError(t, err)
	bad, err := pgxpool.New(ctx, badURL)
	require.NoError(t, err)
	t.Cleanup(bad.Close)

	_, err = bad.Exec(ctx, `GRANT UPDATE ON audit_events TO `+badRole)
	require.NoError(t, err, "an owner can grant itself privileges back")

	_, err = bad.Exec(ctx, `UPDATE audit_events SET action = 'tampered'`)
	require.NoError(t, err, "and can then rewrite the audit log")

	var action string
	require.NoError(t, owner.QueryRow(ctx, `SELECT action FROM audit_events`).Scan(&action))
	require.Equal(t, "tampered", action,
		"this is the configuration Pando must refuse to run in")
}

func urlAs(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}

// TestStartupFailsLoudlyWhenAuditCannotBeProtected asserts the external-database
// preflight: Pando refuses to run rather than serving with a rewritable audit
// log. A degraded mode is not acceptable — the value of enforcing R-027 at the
// database is that it holds without anyone checking.
//
// The trigger is ownership, not current privilege. Pando's own applyGrants would
// revoke the privilege on the way past and the check would pass, while the role
// retained the ability to grant it straight back.
func TestStartupFailsLoudlyWhenAuditCannotBeProtected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL := startPostgres(t)

	// Bring the schema up normally, then hand the app role ownership of the
	// audit table — the situation a managed-Postgres install can land in.
	db, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL})
	require.NoError(t, err)
	db.Close()

	owner, err := pgxpool.New(ctx, ownerURL)
	require.NoError(t, err)
	_, err = owner.Exec(ctx, fmt.Sprintf(`ALTER TABLE audit_events OWNER TO %s`, state.AppRole))
	require.NoError(t, err)
	owner.Close()

	_, err = state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL})
	require.Error(t, err, "Pando must refuse to start when its audit log is rewritable")
	require.Contains(t, err.Error(), "tamper-proof")
	require.Contains(t, err.Error(), "owns the audit table")
}

// TestConnectRetriesUntilPostgresIsReady asserts the Compose race is handled:
// both services start at once, so Postgres will not be accepting connections
// when Pando first dials.
func TestConnectRetriesUntilPostgresIsReady(t *testing.T) {
	// Not parallel: it measures how long the retry loop waits, and serial
	// tests finish before any parallel one starts.
	ctx := context.Background()

	// A URL pointing at nothing must fail within the timeout rather than hang,
	// and must say something a person can act on.
	start := time.Now()
	_, err := state.Connect(ctx, state.ConnectOptions{
		OwnerURL:       "postgres://pando:pando@127.0.0.1:1/pando?sslmode=disable",
		ConnectTimeout: 3 * time.Second,
	})
	require.Error(t, err)
	require.WithinDuration(t, start.Add(3*time.Second), time.Now(), 5*time.Second)
	require.Contains(t, err.Error(), "could not reach its state database")
}

// TestR348_AMonthOfTheAuditLogIsReprotectedOnRestart asserts that the grant
// policy reaches every partition of the audit log, not only the table: a month
// is a table, and DELETE on it is DELETE on the audit log. A start puts back a
// grant that drifted, and the archiver role Connect hands back can make a
// month and cannot delete from one (R-027, R-348).
func TestR348_AMonthOfTheAuditLogIsReprotectedOnRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL := startPostgres(t)

	db, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL})
	require.NoError(t, err)
	require.NotNil(t, db.Archiver(), "a server is handed the archiver's pool")
	_, err = db.Archiver().Exec(ctx, `SELECT audit_ensure_partition('2025-01-01')`)
	require.NoError(t, err)
	_, err = db.Archiver().Exec(ctx, `DELETE FROM audit_events_2025_01`)
	require.Error(t, err, "the archiver deletes nothing itself")
	db.Close()

	owner, err := pgxpool.New(ctx, ownerURL)
	require.NoError(t, err)
	_, err = owner.Exec(ctx, fmt.Sprintf(`GRANT UPDATE, DELETE ON audit_events_2025_01 TO %s`, state.AppRole))
	require.NoError(t, err)
	owner.Close()

	db2, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL})
	require.NoError(t, err)
	t.Cleanup(db2.Close)
	_, err = db2.Exec(ctx, `DELETE FROM audit_events_2025_01`)
	require.Error(t, err, "a drifted grant on a month should have been revoked on restart")
}
