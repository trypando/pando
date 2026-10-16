//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// installGrant gives a principal the administrator role install-wide, set up
// in the database because the API grants install roles to people only.
func installGrant(t *testing.T, db *state.DB, kind, principalID string) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		INSERT INTO grants (id, app_id, plane, role_scope, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, NULL, 'control', 'install', $2, $3, $4, 'system')`,
		id.New(id.Grant), kind, principalID, authz.RoleAdministrator)
	require.NoError(t, err)
}

// TestR088_DeletingTheGroupThatIsTheOnlyAdministratorIsRefused asserts R-088
// for a group: a group holding the administrator role, with someone in it, is
// the installation's only way to manage accounts, and deleting it would leave
// nobody who can.
func TestR088_DeletingTheGroupThatIsTheOnlyAdministratorIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	groups := state.NewGroups(db)

	admins, err := groups.Create(ctx, "admins")
	require.NoError(t, err)
	installGrant(t, db, "group", admins.ID)
	require.NoError(t, groups.AddMember(ctx, admins.ID, seedUser(t, db, "alice").ID))

	err = groups.Delete(ctx, admins.ID)
	require.Error(t, err)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.ErrorContains(t, err, "only administrators")

	// Nothing was taken: the group and its grant are still there.
	var n int
	require.NoError(t, db.QueryRow(ctx,
		`SELECT count(*) FROM grants WHERE principal_kind = 'group' AND principal_id = $1`, admins.ID).Scan(&n))
	require.Equal(t, 1, n)

	// Another group with the role and nobody in it is not another way.
	empty, err := groups.Create(ctx, "empty admins")
	require.NoError(t, err)
	installGrant(t, db, "group", empty.ID)
	require.Error(t, groups.Delete(ctx, admins.ID), "a group nobody is in administers nothing")

	// With someone else who can manage accounts, it can go.
	other, err := groups.Create(ctx, "backup admins")
	require.NoError(t, err)
	installGrant(t, db, "group", other.ID)
	require.NoError(t, groups.AddMember(ctx, other.ID, seedUser(t, db, "bob").ID))
	require.NoError(t, groups.Delete(ctx, admins.ID))
}

// TestR088_AGrantNobodyCanUseIsNotAnAdministrator asserts that R-088 counts
// people, not grant rows. The sole administrator could be demoted or suspended
// while a grant to an empty group, or to a suspended account, stood in for
// "somebody else who can manage accounts" — and then nobody could.
func TestR088_AGrantNobodyCanUseIsNotAnAdministrator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	groups := state.NewGroups(db)
	users := state.NewUsers(db)
	grants := state.NewGrants(db)

	alice := seedUser(t, db, "alice")
	installGrant(t, db, "user", alice.ID)

	empty, err := groups.Create(ctx, "admins")
	require.NoError(t, err)
	installGrant(t, db, "group", empty.ID)
	bob := seedUser(t, db, "bob")
	installGrant(t, db, "user", bob.ID)
	require.NoError(t, users.SetStatusBy(ctx, bob.ID, "suspended", state.SuspendedByAdmin))

	err = grants.RevokeInstall(ctx, "user", alice.ID)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "an empty group and a suspended account are nobody")
	err = users.SetStatusBy(ctx, alice.ID, "suspended", state.SuspendedByAdmin)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))

	// The last person who manages through a group is protected the same way.
	carol := seedUser(t, db, "carol")
	require.NoError(t, groups.AddMember(ctx, empty.ID, carol.ID))
	require.NoError(t, grants.RevokeInstall(ctx, "user", alice.ID), "carol can manage accounts now")
	err = users.SetStatusBy(ctx, carol.ID, "suspended", state.SuspendedByAdmin)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "carol is the last one, through her group")
}

// TestR078_ManyGroupsCanBeMadeAndNamesAreUnique asserts that an installation
// can hold more than one Pando-made group — it could not, because the uniqueness
// meant for synced groups counted every Pando-made one as the same — and that a
// name is still taken once.
func TestR078_ManyGroupsCanBeMadeAndNamesAreUnique(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	groups := state.NewGroups(connected(t))

	for _, name := range []string{"engineering", "design", "support"} {
		_, err := groups.Create(ctx, name)
		require.NoError(t, err, name)
	}
	_, err := groups.Create(ctx, "design")
	require.ErrorContains(t, err, `already a group called "design"`)

	listed, err := groups.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 3)
}
