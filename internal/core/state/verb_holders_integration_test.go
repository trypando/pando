//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// TestR362_UpdateAvailableGoesToEveryoneHoldingInstallUpgrade asserts who
// R-362's notification reaches: holders of install.upgrade directly or through
// a group, active ones only, and nobody who merely uses an app.
func TestR362_UpdateAvailableGoesToEveryoneHoldingInstallUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)

	direct := seedUser(t, db, "direct")
	viaGroup := seedUser(t, db, "via-group")
	suspended := seedUser(t, db, "suspended")
	nobody := seedUser(t, db, "nobody")

	grant := func(kind, principal string) {
		t.Helper()
		_, err := db.Exec(ctx, `
			INSERT INTO grants (id, app_id, plane, role_scope, principal_kind, principal_id, role_id, created_by)
			VALUES ($1, NULL, 'control', 'install', $2, $3, $4, 'system')`,
			id.New(id.Grant), kind, principal, authz.RoleAdministrator)
		require.NoError(t, err)
	}
	grant("user", direct.ID)
	grant("user", suspended.ID)

	admins, err := state.NewGroups(db).Create(ctx, "admins")
	require.NoError(t, err)
	grant("group", admins.ID)
	_, err = db.Exec(ctx, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, admins.ID, viaGroup.ID)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, suspended.ID)
	require.NoError(t, err)

	holders, err := state.NewAuthzStore(db).InstallVerbHolders(ctx, authz.InstallUpgrade)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{direct.ID, viaGroup.ID}, holders)
	require.NotContains(t, holders, nobody.ID)
	require.NotContains(t, holders, suspended.ID, "a suspended account is told nothing")

	none, err := state.NewAuthzStore(db).InstallVerbHolders(ctx, authz.Verb("install.no.such.verb"))
	require.NoError(t, err)
	require.Empty(t, none)
}
