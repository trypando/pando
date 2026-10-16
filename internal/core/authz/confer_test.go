package authz_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
)

const (
	roleUsersOnly    = "role_users_only"
	roleAdaptersOnly = "role_adapters_only"
)

// conferStore is alice holding a custom role with install.users.manage alone,
// bob holding Administrator, and carol holding nothing.
func conferStore() *store {
	s := newStore()
	s.roles[roleUsersOnly] = authz.Role{ID: roleUsersOnly, Name: "Account desk", Verbs: []authz.Verb{authz.InstallUsersManage}}
	s.roles[roleAdaptersOnly] = authz.Role{ID: roleAdaptersOnly, Name: "Platform", Verbs: []authz.Verb{authz.InstallAdaptersManage}}
	s.install = []authz.Grant{
		{ID: "gr_a", PrincipalKind: "user", PrincipalID: alice, RoleID: roleUsersOnly},
		{ID: "gr_b", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAdministrator},
	}
	return s
}

// TestR080a_AnAccountManagerCannotHandOnARoleTheyDoNotHold asserts R-080a: a
// custom role holding install.users.manage alone cannot grant Administrator,
// to themselves or anyone, and the refusal is a denial that is audited.
func TestR080a_AnAccountManagerCannotHandOnARoleTheyDoNotHold(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	a := authz.New(conferStore(), nil, rec)

	err := a.CheckConfersRole(ctx, activeUser(alice), authz.RoleAdministrator, "grant the role %s")
	require.Error(t, err)
	require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err))
	require.Contains(t, err.Error(), "grant the role administrator")
	require.Contains(t, err.Error(), string(authz.InstallPolicyManage), "names what they lack")
	require.NotContains(t, err.Error(), string(authz.InstallUsersManage)+",", "and not what they hold")
	require.Equal(t, 1, rec.denials)

	// What they do hold, they may hand on.
	require.NoError(t, a.CheckConfersRole(ctx, activeUser(alice), roleUsersOnly, "grant the role %s"))
	// An app role carries no install verb, so there is nothing to check here.
	require.NoError(t, a.CheckConfersRole(ctx, activeUser(alice), authz.RoleOwner, "grant the role %s"))
	// An administrator may hand on anything.
	require.NoError(t, a.CheckConfersRole(ctx, activeUser(bob), authz.RoleAdministrator, "grant the role %s"))
}

// TestR080a_AnAccountManagerCannotChangeAnAccountThatOutranksThem asserts
// R-080a: a password reset or a suspension is a way into an account or out of
// its permissions, so it needs every install verb the account holds.
func TestR080a_AnAccountManagerCannotChangeAnAccountThatOutranksThem(t *testing.T) {
	ctx := context.Background()
	a := authz.New(conferStore(), nil, &recorder{})

	err := a.CheckMayChangeAccount(ctx, activeUser(alice), bob, "reset this account's password")
	require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err))
	require.Contains(t, err.Error(), "that account holds installation permissions you do not")

	require.NoError(t, a.CheckMayChangeAccount(ctx, activeUser(alice), "usr_carol", "reset this account's password"),
		"an account holding nothing is any account manager's to change")
	require.NoError(t, a.CheckMayChangeAccount(ctx, activeUser(alice), alice, "change this account"),
		"and your own is yours")
	require.NoError(t, a.CheckMayChangeAccount(ctx, activeUser(bob), alice, "reset this account's password"))
}

// TestR080a_ConfiguringAnIdentityProviderNeedsEveryInstallVerb asserts R-080a
// for finding 4 of the 2026-10-16 assessment: install.adapters.manage alone
// could register a provider and sign in as an administrator through it.
func TestR080a_ConfiguringAnIdentityProviderNeedsEveryInstallVerb(t *testing.T) {
	ctx := context.Background()
	s := conferStore()
	s.install = append(s.install, authz.Grant{ID: "gr_c", PrincipalKind: "user", PrincipalID: "usr_carol", RoleID: roleAdaptersOnly})
	a := authz.New(s, nil, &recorder{})

	err := a.CheckHoldsEveryInstallVerb(ctx, activeUser("usr_carol"), "add an identity provider", "it decides who signs in")
	require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err))
	require.Contains(t, err.Error(), "Pando refused to add an identity provider, because it decides who signs in.")

	require.NoError(t, a.CheckHoldsEveryInstallVerb(ctx, activeUser(bob), "add an identity provider", "it decides who signs in"))
	require.NoError(t, a.CheckHoldsEveryInstallVerb(ctx, authz.System(), "add an identity provider", "it decides who signs in"))
}

// A failure to read grants or roles is a failure, never an answer: nothing is
// handed on, and nothing is audited as a denial (as TestAStoreFailureIsNeverAnAnswer).
func TestR080a_AStoreFailureConfersNothing(t *testing.T) {
	ctx := context.Background()
	for name, f := range map[string]failing{
		"install grants": {store: conferStore(), install: true},
		"a held role":    {store: conferStore(), role: roleUsersOnly},
	} {
		rec := &recorder{}
		a := authz.New(f, nil, rec)
		require.ErrorIs(t, a.CheckConfers(ctx, activeUser(alice), []authz.Verb{authz.InstallView}, "grant it"), errStore, name)
		require.ErrorIs(t, a.CheckMayChangeAccount(ctx, activeUser(bob), alice, "change it"), errStore, name)
		require.ErrorIs(t, a.CheckHoldsEveryInstallVerb(ctx, activeUser(alice), "do it", "it reaches everything"), errStore, name)
		require.Zero(t, rec.denials, name)
	}

	a := authz.New(failing{store: conferStore(), role: authz.RoleAdministrator}, nil, &recorder{})
	require.ErrorIs(t, a.CheckConfersRole(ctx, activeUser(alice), authz.RoleAdministrator, "grant the role %s"), errStore)
}

// The system holds every install verb, and anonymous none.
func TestInstallVerbsHeldBySystemAndAnonymous(t *testing.T) {
	ctx := context.Background()
	a := authz.New(conferStore(), nil, &recorder{})

	held, err := a.InstallVerbsHeld(ctx, authz.System())
	require.NoError(t, err)
	for _, v := range installVerbs() {
		require.True(t, held[v], v)
	}
	held, err = a.InstallVerbsHeld(ctx, authz.Anonymous())
	require.NoError(t, err)
	require.Empty(t, held)

	require.NoError(t, a.CheckConfersRole(ctx, activeUser(alice), "", "grant the role %s"), "no role carries nothing")
	require.NoError(t, a.CheckConfersRole(ctx, activeUser(alice), "role_missing", "grant the role %s"),
		"nor does one that does not exist")
}

// An app verb in an install check is a caller mistake, refused rather than
// evaluated (R-080): install roles cannot carry one.
func TestConferringAnAppVerbInstallWideIsAnInternalError(t *testing.T) {
	a := authz.New(conferStore(), nil, &recorder{})
	err := a.CheckConfers(context.Background(), activeUser(bob), []authz.Verb{authz.AppExec}, "grant it")
	require.Equal(t, errs.Internal, errs.CodeOf(err))
}
