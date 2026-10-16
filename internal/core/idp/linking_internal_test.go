package idp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

type heldRoles struct {
	held map[string][]state.HeldInstallRole
	err  error
}

func (h heldRoles) InstallRolesForUsers(context.Context, []string) (map[string][]state.HeldInstallRole, error) {
	return h.held, h.err
}

// TestR080a_AnAccountHoldingAnInstallRoleIsNotLinkedByEmail asserts R-080a
// and R-045: a provider vouching for an administrator's email does not make
// whoever it signed in that administrator. An administrator links the identity
// explicitly instead, and the message says how.
func TestR080a_AnAccountHoldingAnInstallRoleIsNotLinkedByEmail(t *testing.T) {
	ctx := context.Background()
	provider := state.IdentityProvider{ID: "idp_okta", Name: "Okta"}
	subject := api.Subject{ExternalID: "00u42", Email: "admin@example.com", EmailVerified: true}
	admin := state.User{ID: "usr_admin"}

	s := &Service{Grants: heldRoles{held: map[string][]state.HeldInstallRole{
		"usr_admin": {{RoleID: "role_administrator", Via: state.InstallRoleVia{Kind: "direct"}}},
	}}}
	refusal := s.installRoleRefusal(ctx, provider, subject, admin)
	require.NotNil(t, refusal)
	require.Equal(t, errs.AuthInvalid, refusal.Code)
	require.Contains(t, refusal.Message, "Okta signed you in with an email that belongs to a Pando account holding installation permissions")
	require.Contains(t, refusal.Remedy, "00u42", "names the ID an administrator needs to link it")

	// Through a group counts the same.
	s.Grants = heldRoles{held: map[string][]state.HeldInstallRole{
		"usr_admin": {{RoleID: "role_auditor", Via: state.InstallRoleVia{Kind: "group", GroupID: "grp_sec"}}},
	}}
	require.NotNil(t, s.installRoleRefusal(ctx, provider, subject, admin))

	// An account holding nothing installation-wide is linked as before.
	s.Grants = heldRoles{held: map[string][]state.HeldInstallRole{}}
	require.Nil(t, s.installRoleRefusal(ctx, provider, subject, state.User{ID: "usr_plain"}))

	// Not knowing is not linking.
	s.Grants = heldRoles{err: errors.New("connection refused")}
	require.Equal(t, errs.Internal, s.installRoleRefusal(ctx, provider, subject, admin).Code)
	s.Grants = heldRoles{err: errs.New(errs.Internal, "Could not read installation-wide access.")}
	require.Contains(t, s.installRoleRefusal(ctx, provider, subject, admin).Message, "installation-wide access",
		"an error that already says what happened is passed on as itself")
	s.Grants = nil
	require.NotNil(t, s.installRoleRefusal(ctx, provider, subject, admin))
}
