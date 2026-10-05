//go:build integration

package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/id"
)

func eventsNamed(t *testing.T, db *state.DB, appID, name string) []state.Event {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT id, data FROM events WHERE coalesce(app_id, '') = $1 AND name = $2 ORDER BY seq`, appID, name)
	require.NoError(t, err)
	defer rows.Close()
	var out []state.Event
	for rows.Next() {
		var e state.Event
		require.NoError(t, rows.Scan(&e.ID, &e.Data))
		out = append(out, e)
	}
	return out
}

// TestR365_StateChangesReachTheOutboxThroughTriggers asserts R-365 and R-366:
// an app's state, a deploy's outcome and a scheduled backup that failed are
// written to the outbox by the database, whoever changed the row, with IDs in
// the same prefixed-ULID shape core makes.
func TestR365_StateChangesReachTheOutboxThroughTriggers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedUser(t, db, "events-owner")
	apps := state.NewApps(db)
	deployments := state.NewDeployments(db)

	app, err := apps.Create(ctx, "events", id.New(id.App), owner.ID, owner.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)

	require.NoError(t, apps.SetState(ctx, app.ID, state.StateDeploying))
	require.NoError(t, apps.SetState(ctx, app.ID, state.StateDeploying), "no change, no event")
	changed := eventsNamed(t, db, app.ID, "app.state_changed")
	require.Len(t, changed, 1)
	require.Equal(t, "deploying", changed[0].Data["to"])
	kind, _, err := id.Parse(changed[0].ID)
	require.NoError(t, err, "a trigger's ID parses as a ULID: %s", changed[0].ID)
	require.Equal(t, id.Event, kind)

	rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion, AppID: app.ID,
		Source: spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"},
	}, spec.OriginManual, owner.ID)
	require.NoError(t, err)
	dep, err := deployments.Create(ctx, app.ID, rev.ID, "manual", owner.ID)
	require.NoError(t, err)
	require.NoError(t, deployments.SetStatus(ctx, dep.ID, state.DeployBuilding))
	require.Empty(t, eventsNamed(t, db, app.ID, "deploy.failed"), "only an outcome is an event")
	require.NoError(t, deployments.Finish(ctx, dep.ID, state.DeployFailed, "BUILD_FAILED", "The build exited with status 1."))

	failed := eventsNamed(t, db, app.ID, "deploy.failed")
	require.Len(t, failed, 1)
	require.Equal(t, dep.ID, failed[0].Data["deployment_id"])
	require.Equal(t, "BUILD_FAILED", failed[0].Data["error_code"])
	require.Equal(t, "The build exited with status 1.", failed[0].Data["message"])

	require.NoError(t, state.NewBackups(db).RecordAttempt(ctx, state.BackupAttempt{
		AppID: app.ID, AttemptedAt: time.Now().UTC(), Outcome: state.AttemptFailed,
		Message: "The destination is full.", Remedy: "Free space on it.",
	}))
	backup := eventsNamed(t, db, app.ID, "backup.failed")
	require.Len(t, backup, 1)
	require.Equal(t, true, backup[0].Data["scheduled"])
}

// TestR365_AnAuditedActionAndItsEventAreOneWrite asserts R-365: a catalogued
// audit action and its outbox row are written in one transaction, with only
// the catalogued fields; an uncatalogued action makes no event.
func TestR365_AnAuditedActionAndItsEventAreOneWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	w := audit.New(db.Pool)

	require.NoError(t, w.Write(ctx, audit.Event{
		PrincipalKind: audit.KindUser, PrincipalID: "usr_1", Action: "token.create",
		TargetKind: "token", TargetID: "tok_1",
		Detail: map[string]any{"name": "ci", "kind": "account", "secret": "should-not-travel"},
	}))
	require.NoError(t, w.Write(ctx, audit.Event{
		PrincipalKind: audit.KindUser, PrincipalID: "usr_1", Action: "app.use",
	}))

	created := eventsNamed(t, db, "", "token.created")
	require.Len(t, created, 1)
	require.Equal(t, "ci", created[0].Data["name"])
	require.Equal(t, "tok_1", created[0].Data["target_id"])
	require.NotContains(t, created[0].Data, "secret")

	var uncatalogued int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM events WHERE name LIKE 'app.use%'`).Scan(&uncatalogued))
	require.Zero(t, uncatalogued)
}

// TestR368_TheAdministratorHoldsInstallEventsManage asserts migration 000043:
// the built-in Administrator gains install.events.manage, and only it (R-081).
func TestR368_TheAdministratorHoldsInstallEventsManage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)

	conn, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var held bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT 'install.events.manage' = ANY (verbs) FROM roles WHERE id = 'role_administrator'`).Scan(&held))
	require.True(t, held)

	var elsewhere int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM roles WHERE id <> 'role_administrator' AND 'install.events.manage' = ANY (verbs)`).Scan(&elsewhere))
	require.Zero(t, elsewhere)
}
