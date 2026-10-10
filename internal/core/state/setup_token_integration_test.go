//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR046_NoTokenMeansNoClaim asserts an installation with no setup token —
// none made yet, or the last one used — is claimed by nobody, whatever token
// is presented.
func TestR046_NoTokenMeansNoClaim(t *testing.T) {
	ctx := context.Background()
	users := state.NewUsers(connected(t))
	require.NoError(t, users.EnsureLocalAdapter(ctx))
	_, _, err := users.ClaimFirst(ctx, secret.New("anything"), "mallory", "", "digest", "role_administrator")
	require.ErrorIs(t, err, state.ErrSetupTokenWrong)
}

// TestR046_ASetupTokenIsNeverMadeOrTakenWhenTheDatabaseFails asserts the
// setup token's store says so when Postgres cannot answer, rather than
// reporting a token made, cleared or accepted that was not (issue #130).
func TestR046_ASetupTokenIsNeverMadeOrTakenWhenTheDatabaseFails(t *testing.T) {
	ctx := context.Background()
	db := connected(t)
	users := state.NewUsers(db)
	db.Close()

	token, _, err := users.EnsureSetupToken(ctx)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Empty(t, token)

	_, err = users.ReplaceSetupToken(ctx)
	require.Error(t, err)

	require.Equal(t, errs.Internal, errs.CodeOf(users.ClearSetupToken(ctx)))

	_, _, err = users.ClaimFirst(ctx, secret.New("token"), "ada", "", "digest", "role_administrator")
	require.Equal(t, errs.Internal, errs.CodeOf(err))
}
