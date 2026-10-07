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

// TestR081_GrantsAreReadWithTheirRoles asserts that the grants the authorizer
// reads carry their role, read in the same query, and that it is the role
// Role answers — so joining them changes how often the database is asked and
// nothing about the answer.
func TestR081_GrantsAreReadWithTheirRoles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	appID := seedApp(t, db, alice.ID)

	_, err := db.Exec(ctx, `
		INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, $2, 'control', 'user', $3, $4, $3)`,
		id.New(id.Grant), appID, alice.ID, authz.RoleOperator)
	require.NoError(t, err)
	_, err = state.NewGrants(db).GrantInstall(ctx, "user", alice.ID, authz.RoleAppViewer, "system")
	require.NoError(t, err)

	store := state.NewAuthzStore(db)
	p := authz.Principal{Kind: authz.KindUser, ID: alice.ID, UserID: alice.ID, Status: "active"}

	control, err := store.ControlGrantsFor(ctx, appID, p)
	require.NoError(t, err)
	install, err := store.InstallGrantsFor(ctx, p)
	require.NoError(t, err)
	require.Len(t, control, 1)
	require.Len(t, install, 1)

	for _, g := range append(control, install...) {
		require.NotNil(t, g.Role, g.RoleID)
		want, err := store.Role(ctx, g.RoleID)
		require.NoError(t, err)
		require.Equal(t, want, *g.Role, g.RoleID)
	}

	// And the authorizer answers from them as it did a role at a time.
	a := authz.New(store, nil, nil)
	verbs, err := a.AppVerbs(ctx, p, appID)
	require.NoError(t, err)
	for _, v := range authz.AppVerbs() {
		allowed, err := a.Allows(ctx, p, appID, v)
		require.NoError(t, err)
		require.Equal(t, allowed, containsVerb(verbs, v), v)
	}
	require.Contains(t, verbs, authz.AppDeploy, "Operator on the app")
	require.Contains(t, verbs, authz.AppView, "and App viewer install-wide")
}

func containsVerb(vs []authz.Verb, v authz.Verb) bool {
	for _, got := range vs {
		if got == v {
			return true
		}
	}
	return false
}
