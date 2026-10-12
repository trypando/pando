//go:build integration

package state_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR105_ATakenUsernameIsAConflictThatNamesIt asserts issue #212: creating an
// account with a username another account has is a 409 naming the username and
// what to do, not a 500. A deleted account keeps its username, and the refusal
// says so rather than pointing at an account that cannot be edited.
func TestR105_ATakenUsernameIsAConflictThatNamesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	users := state.NewUsers(db)
	sam := seedUser(t, db, "sam")

	_, err := users.Create(ctx, state.LocalAdapterID, "sam", "", "Another Sam", "digest", true)
	e := errs.As(err)
	require.NotNil(t, e, "%v", err)
	require.Equal(t, errs.StateUsernameTaken, e.Code)
	require.Equal(t, http.StatusConflict, e.Status())
	require.Equal(t, "An account named sam already exists.", e.Message)
	require.Equal(t, "Choose another username, or edit that account.", e.Remedy)
	require.Equal(t, "sam", e.Details["username"])

	require.NoError(t, users.Delete(ctx, sam.ID))
	_, err = users.Create(ctx, state.LocalAdapterID, "sam", "", "Another Sam", "digest", true)
	e = errs.As(err)
	require.NotNil(t, e, "%v", err)
	require.Equal(t, errs.StateUsernameTaken, e.Code)
	require.Equal(t, "A deleted account was named sam, and its username cannot be reused.", e.Message)
	require.Equal(t, "Choose another username.", e.Remedy)

	// With every account deleted the installation can be set up again, and
	// the deleted account's username is just as unavailable there.
	token, _, err := users.EnsureSetupToken(ctx)
	require.NoError(t, err)
	_, _, err = users.ClaimFirst(ctx, secret.New(token), "sam", "", "digest", "role_administrator")
	e = errs.As(err)
	require.NotNil(t, e, "%v", err)
	require.Equal(t, errs.StateUsernameTaken, e.Code)
	require.Equal(t, "A deleted account was named sam, and its username cannot be reused.", e.Message)
}

// TestR105_AnAccountTheDatabaseCannotTakeIsNotCalledATakenUsername asserts the
// conflict is kept for the one cause it names: a database that cannot answer
// is a server fault, not a reason to choose another username.
func TestR105_AnAccountTheDatabaseCannotTakeIsNotCalledATakenUsername(t *testing.T) {
	t.Parallel()
	db := connected(t)
	users := state.NewUsers(db)
	db.Close()

	_, err := users.Create(context.Background(), state.LocalAdapterID, "sam", "", "Sam", "digest", true)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
}
