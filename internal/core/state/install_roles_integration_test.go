//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// A failed read of who holds what install-wide is an internal error with a
// sentence, never an empty answer: an empty answer would read as "nobody
// holds any role" on the accounts screen (issue #221).
func TestReadingInstallRolesThatFailsSaysSo(t *testing.T) {
	t.Parallel()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	grants := state.NewGrants(db)

	gone, cancel := context.WithCancel(context.Background())
	cancel()

	held, err := grants.InstallRolesForUsers(gone, []string{alice.ID})
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Nil(t, held)

	byPrincipal, err := grants.InstallRolesByPrincipal(gone)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Nil(t, byPrincipal)
}
