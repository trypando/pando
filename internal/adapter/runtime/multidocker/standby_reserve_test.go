package multidocker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// TestR422_TheReservationCountsStandbysAlreadyPlaced asserts that the
// standbys already on the hosts, read back from what ApplyStandby wrote on
// them, keep their reservation: a second app on the same host does not get
// a standby where the first one's already takes the room kept for that
// host's loss.
func TestR422_TheReservationCountsStandbysAlreadyPlaced(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts() // free: control 2 GiB, app-1 20 GiB, app-2 10 GiB
	for _, b := range []string{"app_a", "app_b"} {
		h, err := a.Apply(ctx, plan(b, 6*gib))
		require.NoError(t, err)
		require.Equal(t, "app-1", h.Handle)
	}

	sb, err := a.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan("app_a", 6*gib)})
	require.NoError(t, err)
	require.Equal(t, "app-2", sb.Host)
	require.Equal(t, "app-1", sb.Home, "where the app runs, for core's record of what a lost host held")

	loads, err := a.listStandbys(ctx)
	require.NoError(t, err)
	require.Equal(t, []standbyLoad{{App: "app_a", Primary: "app-1", Host: "app-2",
		Need: amount{CPUMillis: 500, MemoryBytes: 6 * gib}}}, loads)

	// app-2 keeps 6 GiB of its 10 for app-1's loss, so 4 are left there and
	// 2 on control: app_b's 6 fit nowhere.
	_, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan("app_b", 6*gib)})
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "on app-2")
	require.Equal(t, []string{"app_a-standby"}, hosts["app-2"].stopped, "app_b's standby went nowhere")

	// A smaller one fits beside it.
	sb, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan("app_b", 3*gib)})
	require.NoError(t, err)
	require.Equal(t, "app-2", sb.Host)
	require.Empty(t, a.reservationNow(ctx).shortfalls(), "the loss of any one host still fits")
}

// TestR422_KeepingAStandbyWhereItIsNeverBreaksTheReservation asserts that a
// standby stays on its host while it fits there past what that host keeps
// for the app's host's loss, not counting itself, and moves when it no
// longer does.
func TestR422_KeepingAStandbyWhereItIsNeverBreaksTheReservation(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	for _, b := range []string{"app_a", "app_c"} {
		_, err := a.Apply(ctx, plan(b, 3*gib))
		require.NoError(t, err)
	}
	_, err := a.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan("app_a", 6*gib)})
	require.NoError(t, err)
	_, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan("app_c", 3*gib)})
	require.NoError(t, err)

	// Refreshed at 7 GiB: app-2 has 10, less app_c's 3, so it stays, though
	// control is now roomier.
	hosts["control"].free.MemoryBytes = 16 * gib
	sb, err := a.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan("app_a", 7*gib), Host: "app-2"})
	require.NoError(t, err)
	require.Equal(t, "app-2", sb.Host, "its own reservation is not counted against it")

	// At 8 GiB keeping it would leave app-1's loss short on app-2: it moves.
	sb, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan("app_a", 8*gib), Host: "app-2"})
	require.NoError(t, err)
	require.Equal(t, "control", sb.Host)
	require.Contains(t, hosts["app-2"].destroyed, "app_a-standby")
	require.Empty(t, a.reservationNow(ctx).shortfalls())

	// A preferred host that does not answer is not kept either.
	hosts["control"].down = true
	sb, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan("app_a", 6*gib), Host: "control"})
	require.NoError(t, err)
	require.Equal(t, "app-2", sb.Host)
}

// TestR422_APromotedStandbyStartsOnlyWhereItFits asserts that a standby
// placed past the reservation, under allow_failover_oversubscription, is
// not started on a host that no longer has room for it: refused before
// anything of the standby is touched, so the next app in priority order
// can start instead.
func TestR422_APromotedStandbyStartsOnlyWhereItFits(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, plan("app_a", 6*gib))
	require.NoError(t, err)
	_, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: plan("app_a", 6*gib)})
	require.NoError(t, err)

	hosts["app-2"].free.MemoryBytes = 4 * gib
	_, err = a.PromoteStandby(ctx, api.PromoteRequest{Bundle: plan("app_a", 6*gib), Host: "app-2"})
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	require.Equal(t, "This app's standby was not started on app-2: it needs 0.50 cores of CPU and 6.0 GB of memory, and app-2 has 8.00 cores of CPU and 4.0 GB of memory free.",
		errs.As(err).Message)
	require.NotContains(t, hosts["app-2"].destroyed, "app_a-standby", "the standby is left as it was")

	hosts["app-2"].down = true
	_, err = a.PromoteStandby(ctx, api.PromoteRequest{Bundle: plan("app_a", 6*gib), Host: "app-2"})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
}

// TestListStandbysFallsBackToPlacementAndSkipsSilentHosts covers a standby
// written without its primary, and a host that does not answer.
func TestListStandbysFallsBackToPlacementAndSkipsSilentHosts(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, plan("app_a", gib))
	require.NoError(t, err)
	p := standbyPlan(plan("app_a", gib), "")
	require.NoError(t, hosts["app-2"].ApplyStopped(ctx, p))
	loads, err := a.listStandbys(ctx)
	require.NoError(t, err)
	require.Len(t, loads, 1)
	require.Equal(t, "app-1", loads[0].Primary)

	hosts["app-2"].down = true
	loads, err = a.listStandbys(ctx)
	require.NoError(t, err)
	require.Empty(t, loads)

	// The Capacity rows say what each host keeps, from the hosts.
	hosts["app-2"].down = false
	rows := []map[string]any{{}, {}, {}}
	a.failoverDetails(ctx, rows)
	require.Equal(t, int64(gib), rows[2]["failover"].(map[string]any)["reserved_memory_bytes"])
}

// reservationNow is the reservation as the hosts report it now.
func (a *Adapter) reservationNow(ctx context.Context) reservation {
	loads, _ := a.standbyLoads(ctx)
	return reservation{hosts: a.readRooms(ctx), standbys: loads}
}
