//go:build integration

package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
)

// TestR419_AStandbyIsRecordedUntilItsDeletedAppsStandbyIsRemoved asserts the
// standby store (migration 000087): a live app is a candidate with its
// standby, a soft-deleted one stays a candidate while it has a row, and the
// row is gone once forgotten.
func TestR419_AStandbyIsRecordedUntilItsDeletedAppsStandbyIsRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "standby-alice")
	apps := state.NewApps(db)
	standbys := state.NewStandbys(db)

	app, err := apps.Create(ctx, "standby notes", "standby-notes", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	rev, err := apps.CreateRevision(ctx, app.ID, minimalSpec(), spec.OriginManual, alice.ID)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, alice.ID))

	find := func() *state.StandbyApp {
		list, err := standbys.Candidates(ctx)
		require.NoError(t, err)
		for i := range list {
			if list[i].AppID == app.ID {
				return &list[i]
			}
		}
		return nil
	}
	c := find()
	require.NotNil(t, c)
	require.Nil(t, c.Standby)
	require.Equal(t, rev.ID, c.PinnedSpecID)

	shipped := time.Now().UTC().Truncate(time.Microsecond)
	want := state.Standby{AppID: app.ID, RuntimeRef: "rt_hosts", Host: "app-2", SpecID: rev.ID, Volumes: 1, Copies: 3,
		ShippedAt: &shipped, Unreplicated: []string{"postgres"}, StaleHost: "app-1"}
	require.NoError(t, standbys.Save(ctx, want))
	got, found, err := standbys.Get(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want.Host, got.Host)
	require.Equal(t, want.Unreplicated, got.Unreplicated)
	require.True(t, shipped.Equal(*got.ShippedAt))
	require.Equal(t, "rt_hosts", find().Standby.RuntimeRef)

	st, found, err := standbys.AppState(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, state.StateRunning, st)

	// ForApp reads what the reconciler would, for the standby's plan.
	r, found, err := state.NewReconciles(db).ForApp(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, rev.ID, r.PinnedSpecID)

	require.NoError(t, apps.Archive(ctx, app.ID))
	c = find()
	require.NotNil(t, c, "a deleted app with a standby is still visited, to remove it")
	require.True(t, c.Deleted)
	_, found, err = standbys.AppState(ctx, app.ID)
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = state.NewReconciles(db).ForApp(ctx, app.ID)
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, standbys.Delete(ctx, app.ID))
	require.Nil(t, find())
	_, found, err = standbys.Get(ctx, app.ID)
	require.NoError(t, err)
	require.False(t, found)
}
