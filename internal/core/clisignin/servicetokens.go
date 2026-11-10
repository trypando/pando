package clisignin

import (
	"context"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
)

// ServiceTokenCheck is a MayCreateServiceTokens: whether an account holds
// install.tokens.manage (R-060), asked as POST /tokens/service asks a
// signed-in person, with the account's groups read live (R-079).
func ServiceTokenCheck(
	users AccountReader,
	groups interface {
		GroupsForUser(ctx context.Context, userID string) ([]string, error)
	},
	checker interface {
		CheckInstall(ctx context.Context, p authz.Principal, verb authz.Verb) error
	},
) func(ctx context.Context, userID string) error {
	return func(ctx context.Context, userID string) error {
		user, found, err := users.ByID(ctx, userID)
		if err != nil {
			return err
		}
		if !found {
			return errs.New(errs.AuthInvalid, "This account no longer exists, so it cannot create a service token.")
		}
		memberOf, err := groups.GroupsForUser(ctx, user.ID)
		if err != nil {
			return err
		}
		return checker.CheckInstall(ctx, authz.Principal{
			Kind: authz.KindUser, ID: user.ID, UserID: user.ID, Groups: memberOf,
			AdapterID: user.AdapterID, Status: user.Status,
		}, authz.InstallTokensManage)
	}
}
