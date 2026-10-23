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

// TestR046_ASessionThatMustChangeItsPasswordHoldsNothing asserts the same
// backstop for a session whose password somebody else set: it holds no verb
// and no app until the password is changed, whatever its grants.
func TestR046_ASessionThatMustChangeItsPasswordHoldsNothing(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAdministrator}}
	s.owner["app_1"] = bob

	a := authz.New(s, nil, nil)
	pending := authz.Principal{Kind: authz.KindUser, ID: bob, UserID: bob, Status: "active", PasswordChangePending: true}

	err := a.CheckInstall(ctx, pending, authz.InstallUsersManage)
	require.Equal(t, errs.PermPasswordChangeRequired, errs.CodeOf(err))
	err = a.CheckData(ctx, pending, "app_1")
	require.Equal(t, errs.PermPasswordChangeRequired, errs.CodeOf(err), "not even the owner's own app")

	pending.PasswordChangePending = false
	require.NoError(t, a.CheckInstall(ctx, pending, authz.InstallUsersManage), "changed, the grant counts again")
}
