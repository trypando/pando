//go:build integration

package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR415_ARefusedJoinGivesItsUseBack asserts R-415: a join is one
// transaction, so a host refused after its token checked out — a name already
// taken, a cluster not ready — leaves the token's use unspent, and the host
// is recorded with the token's roles, not any it asked for.
func TestR415_ARefusedJoinGivesItsUseBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, _ := clusterStore(t)
	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)

	_, err = c.AddHost(ctx, state.Host{Name: "taken", Roles: []string{"app"}, Address: "x", CertSHA256: strings.Repeat("a", 64)})
	require.NoError(t, err)

	admit := func(name string) func(state.JoinToken) (state.Host, error) {
		return func(state.JoinToken) (state.Host, error) {
			return state.Host{Name: name, Roles: []string{"control"}, Address: "edge:7443", CertSHA256: strings.Repeat("b", 64)}, nil
		}
	}
	_, _, err = c.Join(ctx, issued.Secret, admit("taken"))
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	_, _, err = c.Join(ctx, issued.Secret, func(state.JoinToken) (state.Host, error) {
		return state.Host{}, errs.New(errs.StateInvalid, "not ready")
	})
	require.Contains(t, errs.As(err).Message, "not ready")

	tok, h, err := c.Join(ctx, issued.Secret, admit("edge-1"))
	require.NoError(t, err)
	require.Equal(t, 1, tok.Uses)
	require.Equal(t, []string{"app"}, h.Roles, "the token's roles")
	require.Equal(t, issued.Token.ID, h.JoinedWith)

	_, _, err = c.Join(ctx, issued.Secret, admit("edge-2"))
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
	_, _, err = c.Join(ctx, secret.New("nope"), admit("edge-3"))
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
}

// TestR415_OnlyANewInstallIsFresh asserts R-415's precondition: an account
// or an app, even a deleted one, means the install cannot join.
func TestR415_OnlyANewInstallIsFresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, db := clusterStore(t)
	accounts, apps, err := c.Fresh(ctx)
	require.NoError(t, err)
	require.False(t, accounts)
	require.False(t, apps)

	seedUser(t, db, "ada")
	accounts, _, err = c.Fresh(ctx)
	require.NoError(t, err)
	require.True(t, accounts)
}
