package authz_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
)

// TestR405_ASessionThatMustSetUpTwoFactorHoldsNothing asserts the backstop at
// step 2: a session whose account must still set up two-factor authentication
// is refused even by a route that forgot the API's gate, administrator or not.
func TestR405_ASessionThatMustSetUpTwoFactorHoldsNothing(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAdministrator}}

	a := authz.New(s, nil, nil)
	pending := authz.Principal{Kind: authz.KindUser, ID: bob, UserID: bob, Status: "active", TwoFactorSetupPending: true}

	err := a.CheckInstall(ctx, pending, authz.InstallUsersManage)
	require.Error(t, err)
	require.Equal(t, errs.PolicyTwoFactorRequired, errs.CodeOf(err))

	pending.TwoFactorSetupPending = false
	require.NoError(t, a.CheckInstall(ctx, pending, authz.InstallUsersManage), "set up, the grant counts again")
}
