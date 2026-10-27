//go:build integration

package state_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR414_TheFirstHostIsInTheClusterFromItsFirstStart asserts R-414 against
// the database: replicas starting together on one install record one first
// host between them, a restart records none, a host that joined does not
// stand in for it, and the cluster then counts it.
func TestR414_TheFirstHostIsInTheClusterFromItsFirstStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, _ := clusterStore(t)

	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	_, joined, err := c.Join(ctx, issued.Secret, state.Host{Name: "edge-1", Address: "10.0.0.2:7443", CertSHA256: strings.Repeat("b", 64)})
	require.NoError(t, err)
	_, err = c.ConfirmJoin(ctx, joined.ID)
	require.NoError(t, err)

	var wg sync.WaitGroup
	type outcome struct {
		ok  bool
		err error
	}
	recorded := make(chan outcome, 4)
	for _, name := range []string{"core-a", "core-b", "core-c", "core-d"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := hosts.RecordInstallHost(ctx, c, hosts.InstallHostInfo{Hostname: name, Address: "10.0.0.5:7443", Version: "1.2.3"})
			recorded <- outcome{ok, err}
		}()
	}
	wg.Wait()
	close(recorded)
	n := 0
	for o := range recorded {
		require.NoError(t, o.err)
		if o.ok {
			n++
		}
	}
	require.Equal(t, 1, n, "one first host between the replicas")

	list, err := c.Hosts(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	first := 0
	for _, h := range list {
		if hosts.InstallHost(h) {
			first++
			require.Equal(t, []string{"control", "app"}, h.Roles)
		}
	}
	require.Equal(t, 1, first)

	count, err := (&hosts.Service{Store: c}).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

// TestR046_TheSetupTokenIsCheckedWithoutBeingUsedUp asserts R-046 as
// POST /setup/join reads it: the right token passes and stays, a wrong one
// or none is refused.
func TestR046_TheSetupTokenIsCheckedWithoutBeingUsedUp(t *testing.T) {
	ctx := context.Background()
	db := connected(t)
	users := state.NewUsers(db)
	require.ErrorIs(t, users.CheckSetupToken(ctx, secret.New("anything")), state.ErrSetupTokenWrong, "none made yet")

	token, _, err := users.EnsureSetupToken(ctx)
	require.NoError(t, err)
	require.ErrorIs(t, users.CheckSetupToken(ctx, secret.New("guess")), state.ErrSetupTokenWrong)
	require.NoError(t, users.CheckSetupToken(ctx, secret.New(token)))
	require.NoError(t, users.CheckSetupToken(ctx, secret.New(token)), "not used up")

	db.Close()
	require.Equal(t, errs.Internal, errs.CodeOf(users.CheckSetupToken(ctx, secret.New(token))))
	_, _, err = state.NewCluster(db, make([]byte, 32)).RecordFirstHost(ctx, state.Host{})
	require.Equal(t, errs.Internal, errs.CodeOf(err))
}
