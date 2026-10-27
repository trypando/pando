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

// TestR421_AControlHostRecordsWhereItAnswersPingsInTheDatabase asserts
// R-421's record of where a control host answers pings: its address and the
// pin that replaces the first host's placeholder.
func TestR421_AControlHostRecordsWhereItAnswersPingsInTheDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, _ := clusterStore(t)
	h, recorded, err := c.RecordFirstHost(ctx, state.Host{
		Name: "core-1", Roles: []string{"control", "app"}, Address: "10.0.0.1:7443", CertSHA256: strings.Repeat("0", 64),
	})
	require.NoError(t, err)
	require.True(t, recorded)

	pin := strings.Repeat("e", 64)
	require.NoError(t, c.SetHostPing(ctx, h.ID, "10.0.0.1:7444", pin))
	got, err := c.Host(ctx, h.ID)
	require.NoError(t, err)
	require.Equal(t, "10.0.0.1:7444", got.PingAddress)
	require.Equal(t, pin, got.PingCertSHA256)
	// R-416: the agent's address and pin, which the runtime uses, are kept.
	require.Equal(t, "10.0.0.1:7443", got.Address)
	require.Equal(t, strings.Repeat("0", 64), got.CertSHA256)

	// A renewed certificate becomes the agent's, the old kept beside it.
	renewed := strings.Repeat("f", 64)
	require.NoError(t, c.RotateHostCert(ctx, h.ID, renewed))
	require.NoError(t, c.RotateHostCert(ctx, h.ID, renewed), "again is no change")
	got, err = c.Host(ctx, h.ID)
	require.NoError(t, err)
	require.Equal(t, renewed+","+strings.Repeat("0", 64), got.Pins())
	require.True(t, got.HoldsCert(pin), "the ping pin is the host's too")
	require.Equal(t, errs.NotFound, errs.CodeOf(c.RotateHostCert(ctx, "host_nobody", renewed)))

	err = c.SetHostPing(ctx, "host_nobody", "10.0.0.1:7444", pin)
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	require.Error(t, c.SetHostPing(ctx, h.ID, "10.0.0.1:7444", "not-a-pin"), "the column holds fingerprints only")
}
