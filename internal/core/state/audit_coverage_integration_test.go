//go:build integration

package state_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// TestR389_ADeploysOutcomeIsAnAuditEvent asserts R-389: when a deploy
// finishes, however it finishes, the system writes deploy.finish with the
// outcome and the error code, in the same transaction as the status.
func TestR389_ADeploysOutcomeIsAnAuditEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedUser(t, db, "deploy-finish-owner")
	apps := state.NewApps(db)
	deployments := state.NewDeployments(db)

	app, err := apps.Create(ctx, "finish", id.New(id.App), owner.ID, owner.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion, AppID: app.ID,
		Source: spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"},
	}, spec.OriginManual, owner.ID)
	require.NoError(t, err)

	failing, err := deployments.Create(ctx, app.ID, rev.ID, "manual", owner.ID)
	require.NoError(t, err)
	require.NoError(t, deployments.SetStatus(ctx, failing.ID, state.DeployBuilding))
	require.NoError(t, deployments.Finish(ctx, failing.ID, state.DeployFailed, "BUILD_FAILED", "The build exited with status 1."))

	working, err := deployments.Create(ctx, app.ID, rev.ID, "rollback", owner.ID)
	require.NoError(t, err)
	require.NoError(t, deployments.Finish(ctx, working.ID, state.DeploySucceeded, "", ""))

	rows, err := db.Query(ctx, `
		SELECT principal_kind, target_id, outcome, detail FROM audit_events
		WHERE action = 'deploy.finish' AND app_id = $1 ORDER BY id`, app.ID)
	require.NoError(t, err)
	defer rows.Close()
	type finish struct {
		kind, target, outcome string
		detail                map[string]any
	}
	var got []finish
	for rows.Next() {
		var f finish
		var raw []byte
		require.NoError(t, rows.Scan(&f.kind, &f.target, &f.outcome, &raw))
		require.NoError(t, json.Unmarshal(raw, &f.detail))
		got = append(got, f)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 2, "one event per finished deploy")

	require.Equal(t, "system", got[0].kind)
	require.Equal(t, failing.ID, got[0].target)
	require.Equal(t, "failed", got[0].outcome)
	require.Equal(t, "BUILD_FAILED", got[0].detail["error_code"])
	require.Equal(t, rev.ID, got[0].detail["spec_revision"])

	require.Equal(t, "success", got[1].outcome)
	require.Equal(t, "rollback", got[1].detail["trigger"])
	require.NotContains(t, got[1].detail, "error_code")
}
