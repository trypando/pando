package clisignin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clisignin"
	"github.com/trypando/pando/internal/errs"
)

type stubGroups struct{ err error }

func (s stubGroups) GroupsForUser(context.Context, string) ([]string, error) {
	return []string{"grp_admins"}, s.err
}

type stubChecker struct {
	asked []authz.Principal
	verb  authz.Verb
}

func (s *stubChecker) CheckInstall(_ context.Context, p authz.Principal, verb authz.Verb) error {
	s.asked, s.verb = append(s.asked, p), verb
	if p.ID == "usr_ada" {
		return nil
	}
	return errs.New(errs.PermDenied, "no")
}

// TestR060_OnlyAnAccountThatManagesTokensCreatesOneFromTheCLI asserts the
// check pando token create --service is held to (R-060, issue #396): the
// approving account, with its live groups, must hold install.tokens.manage.
func TestR060_OnlyAnAccountThatManagesTokensCreatesOneFromTheCLI(t *testing.T) {
	users := newRig().users
	checker := &stubChecker{}
	check := clisignin.ServiceTokenCheck(users, stubGroups{}, checker)
	ctx := context.Background()

	require.NoError(t, check(ctx, "usr_ada"))
	require.Equal(t, authz.InstallTokensManage, checker.verb)
	require.Equal(t, authz.Principal{Kind: authz.KindUser, ID: "usr_ada", UserID: "usr_ada",
		Groups: []string{"grp_admins"}, Status: "active"}, checker.asked[0])

	require.Equal(t, errs.PermDenied, errs.As(check(ctx, "usr_pending")).Code)
	require.Equal(t, errs.AuthInvalid, errs.As(check(ctx, "usr_nobody")).Code)
	require.Error(t, check(ctx, "usr_broken"))
	require.Error(t, clisignin.ServiceTokenCheck(users, stubGroups{err: errors.New("down")}, checker)(ctx, "usr_ada"))
}
