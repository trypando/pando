//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// TestR212_ProvisionedServicesAreListedForTheAppAndForABackup asserts that a
// provisioned service is found both ways it is looked for: by its app, which
// is how a deploy reconnects it, and install-wide, which is what a backup
// counts and snapshots (R-131, R-212). An app with none lists none as an empty
// list rather than null, and a list that cannot be read says so.
func TestR212_ProvisionedServicesAreListedForTheAppAndForABackup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	appID := seedApp(t, db, alice.ID)
	other := seedApp(t, db, alice.ID)
	services := state.NewServices(db)

	for _, v := range []state.ServiceInstance{
		{ID: services.NewID(), AppID: appID, SlotKey: "db", SlotType: spec.SlotPostgres, AdapterRef: "svc_local", Handle: "pando-db", ConnectionRef: "DATABASE_URL"},
		{ID: services.NewID(), AppID: appID, SlotKey: "cache", SlotType: spec.SlotRedis, AdapterRef: "svc_local", Handle: "pando-cache", ConnectionRef: "REDIS_URL"},
	} {
		require.NoError(t, services.Record(ctx, v))
	}

	mine, err := services.ForApp(ctx, appID)
	require.NoError(t, err)
	require.Len(t, mine, 2)
	require.Equal(t, "cache", mine[0].SlotKey, "ordered by slot")
	require.Equal(t, spec.SlotRedis, mine[0].SlotType)
	require.Equal(t, "REDIS_URL", mine[0].ConnectionRef)
	require.Equal(t, "db", mine[1].SlotKey)

	none, err := services.ForApp(ctx, other)
	require.NoError(t, err)
	require.NotNil(t, none, "an app with no services lists [], not null")
	require.Empty(t, none)

	all, err := state.NewBundleSource(db).ServicesToSnapshot(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2, "a backup counts every provisioned service")
	for _, s := range all {
		require.Equal(t, appID, s.AppID)
		require.Equal(t, "svc_local", s.AdapterRef)
	}

	gone, cancel := context.WithCancel(ctx)
	cancel()
	_, err = services.ForApp(gone, appID)
	require.Error(t, err)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Contains(t, err.Error(), "Could not read this app's provisioned services.")
}
