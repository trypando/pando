//go:build integration

package state_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// TestR421_AHostIsMarkedLostOnceAndEachFenceRecordedOnce asserts R-421's
// records: two marking a host lost at once, one of them did it; the first
// time and who said so are kept; and a fence is new exactly once.
func TestR421_AHostIsMarkedLostOnceAndEachFenceRecordedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, db := clusterStore(t)
	h, err := c.AddHost(ctx, state.Host{Name: "app-9", Roles: []string{"app"}, Address: "10.0.0.9:7443", CertSHA256: strings.Repeat("d", 64)})
	require.NoError(t, err)

	lost, err := c.LostHosts(ctx)
	require.NoError(t, err)
	require.Empty(t, lost)

	var newly atomic.Int32
	var wg sync.WaitGroup
	for _, by := range []string{"liveness", "usr_ada", "usr_bob"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := c.MarkHostLost(ctx, h.ID, by)
			require.NoError(t, err)
			if ok {
				newly.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), newly.Load())

	lost, err = c.LostHosts(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{h.ID: true}, lost)

	_, err = c.MarkHostLost(ctx, "host_nobody", "usr_ada")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	require.NoError(t, c.ClearHostLost(ctx, h.ID))
	lost, err = c.LostHosts(ctx)
	require.NoError(t, err)
	require.Empty(t, lost)
	var by *string
	require.NoError(t, db.QueryRow(ctx, `SELECT lost_by FROM cluster_hosts WHERE id = $1`, h.ID).Scan(&by))
	require.Nil(t, by)

	first, err := c.RecordHostFence(ctx, h.ID, "fnc_01HQ8ZZZZZZZZZZZZZZZZZZZZZ")
	require.NoError(t, err)
	require.True(t, first)
	again, err := c.RecordHostFence(ctx, h.ID, "fnc_01HQ8ZZZZZZZZZZZZZZZZZZZZZ")
	require.NoError(t, err)
	require.False(t, again)
	next, err := c.RecordHostFence(ctx, h.ID, "fnc_01HQ9ZZZZZZZZZZZZZZZZZZZZZ")
	require.NoError(t, err)
	require.True(t, next)

	_, err = c.RecordHostFence(ctx, h.ID, "not-a-fence")
	require.Error(t, err, "the column holds fence IDs only")
}
