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
// taken — leaves the token's use unspent, and the host is recorded with the
// token's roles, not any it asked for. Checking a token spends nothing.
func TestR415_ARefusedJoinGivesItsUseBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, _ := clusterStore(t)
	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)

	_, err = c.AddHost(ctx, state.Host{Name: "taken", Roles: []string{"app"}, Address: "x", CertSHA256: strings.Repeat("a", 64)})
	require.NoError(t, err)

	host := func(name string) state.Host {
		return state.Host{Name: name, Roles: []string{"control"}, Address: "edge:7443", CertSHA256: strings.Repeat("b", 64)}
	}
	_, _, err = c.Join(ctx, issued.Secret, host("taken"))
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	tok, err := c.CheckJoinToken(ctx, issued.Secret)
	require.NoError(t, err)
	require.Zero(t, tok.Uses, "checked, not spent")

	tok, h, err := c.Join(ctx, issued.Secret, host("edge-1"))
	require.NoError(t, err)
	require.Equal(t, 1, tok.Uses)
	require.Equal(t, []string{"app"}, h.Roles, "the token's roles")
	require.Equal(t, issued.Token.ID, h.JoinedWith)
	require.True(t, h.Pending)

	_, _, err = c.Join(ctx, issued.Secret, host("edge-2"))
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
	_, _, err = c.Join(ctx, secret.New("nope"), host("edge-3"))
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
	_, err = c.CheckJoinToken(ctx, secret.New(issued.Token.ID+".wrong"))
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
}

// TestR415_AnUnconfirmedJoinIsNotListedAndCanBeTriedAgain asserts R-415's
// pending join: a host is listed only once it confirms its join; until then
// the same name joins again over it, and once it is past its lifetime it is
// forgotten. Either way its certificate is revoked and its token's use
// given back, so a single-use token is not lost to a join that failed on the
// joining side.
func TestR415_AnUnconfirmedJoinIsNotListedAndCanBeTriedAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, db := clusterStore(t)
	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	host := func(name, cert string) state.Host {
		return state.Host{Name: name, Address: name + ":7443", CertSHA256: strings.Repeat(cert, 64), KeySHA256: strings.Repeat("c", 64)}
	}

	_, first, err := c.Join(ctx, issued.Secret, host("edge-1", "1"))
	require.NoError(t, err)
	list, err := c.Hosts(ctx)
	require.NoError(t, err)
	require.Empty(t, list, "not listed while pending")
	_, err = c.Host(ctx, first.ID)
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	held, found, err := c.HostByCert(ctx, first.CertSHA256)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, held.Pending)

	// The same host, under the same name and key, with the same single-use
	// token, joins over it.
	_, second, err := c.Join(ctx, issued.Secret, host("edge-1", "2"))
	require.NoError(t, err)
	revoked, err := c.CertRevoked(ctx, first.CertSHA256)
	require.NoError(t, err)
	require.True(t, revoked, "the replaced join's certificate is refused")

	// Past its lifetime it is forgotten, and the use comes back.
	_, err = db.Exec(ctx, `UPDATE cluster_hosts SET joined_at = now() - interval '1 hour' WHERE id = $1`, second.ID)
	require.NoError(t, err)
	toks, err := c.JoinTokens(ctx)
	require.NoError(t, err)
	require.Zero(t, toks[0].Uses, "the use is given back")
	_, found, err = c.HostByCert(ctx, second.CertSHA256)
	require.NoError(t, err)
	require.False(t, found)

	// Confirmed, it is listed and not replaced.
	_, third, err := c.Join(ctx, issued.Secret, host("edge-2", "3"))
	require.NoError(t, err)
	confirmed, err := c.ConfirmJoin(ctx, third.ID)
	require.NoError(t, err)
	require.True(t, confirmed)
	confirmed, err = c.ConfirmJoin(ctx, third.ID)
	require.NoError(t, err)
	require.False(t, confirmed, "once")
	list, err = c.Hosts(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.False(t, list[0].Pending)
	more, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	_, _, err = c.Join(ctx, more.Secret, host("edge-2", "4"))
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err), "a confirmed host's name is taken")
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

// TestR415_TwoHostsJoiningUnderOneNameDoNotEvictEachOther asserts R-415's
// pending join against two machines that both call themselves the same name,
// such as cloned VMs that both default to "ubuntu": the second is refused
// while the first's join is pending, rather than evicting it and being
// evicted back, and the refusal names the clash and --name. The same joining
// host trying again, with the same key, replaces its own pending join.
func TestR415_TwoHostsJoiningUnderOneNameDoNotEvictEachOther(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, _ := clusterStore(t)
	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 3, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	host := func(cert, key string) state.Host {
		return state.Host{Name: "ubuntu", Address: "ubuntu:7443", CertSHA256: strings.Repeat(cert, 64), KeySHA256: strings.Repeat(key, 64)}
	}

	_, first, err := c.Join(ctx, issued.Secret, host("1", "a"))
	require.NoError(t, err)

	_, _, err = c.Join(ctx, issued.Secret, host("2", "b"))
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err), "another machine's join of the same name is refused")
	require.Contains(t, errs.As(err).Message, `"ubuntu"`)
	require.Contains(t, errs.As(err).Remedy, "--name")
	held, found, err := c.HostByCert(ctx, first.CertSHA256)
	require.NoError(t, err)
	require.True(t, found, "the first join is still pending, not evicted")
	require.True(t, held.Pending)
	toks, err := c.JoinTokens(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, toks[0].Uses, "the refused join spent nothing")

	// The first host trying again, with its own key, replaces its own join.
	_, again, err := c.Join(ctx, issued.Secret, host("3", "a"))
	require.NoError(t, err)
	revoked, err := c.CertRevoked(ctx, first.CertSHA256)
	require.NoError(t, err)
	require.True(t, revoked)
	toks, err = c.JoinTokens(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, toks[0].Uses, "the replaced join's use is given back")
	confirmed, err := c.ConfirmJoin(ctx, again.ID)
	require.NoError(t, err)
	require.True(t, confirmed)
}
