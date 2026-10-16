package httpapi

import (
	"context"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
)

// What the account and group endpoints ask before they change anything, so
// that holding one installation permission is never a way to obtain another
// (R-080a, core/authz/confer.go). The rule is authz's; these only find what a
// group holds so that authz can be asked about it.

// confersGroup refuses p changing what a group hands on — its membership, its
// role, its links to a provider's groups, or the group itself — unless p holds
// every install verb the group's role carries.
func (s *Server) confersGroup(ctx context.Context, p authz.Principal, groupID, act string) error {
	if s.Grants == nil {
		// Closed rather than open: without the grants there is no telling what
		// the group hands on.
		return errs.New(errs.Internal, "Pando cannot read what this group holds, so it changed nothing.")
	}
	roleID, found, err := s.Grants.InstallRoleOf(ctx, "group", groupID)
	if err != nil || !found {
		return err
	}
	return s.Authz.CheckConfersRole(ctx, p, roleID, act)
}
