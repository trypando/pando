//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// TestR421_TheAppsAHostHoldsAreReadFromTheirStandbys asserts migration
// 000089: an app's home, and the stale copy a failover left, are recorded
// with its standby, so the leader finds which apps a lost host held and
// which copies a returned host still holds; a deleted app is in neither.
// The no-room code and remedy are kept beside the message (R-422).
func TestR421_TheAppsAHostHoldsAreReadFromTheirStandbys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "standby-home-alice")
	apps := state.NewApps(db)
	standbys := state.NewStandbys(db)
	home, left := "h-"+id.New(id.App)[4:14], "l-"+id.New(id.App)[4:14]

	create := func(slug string) string {
		app, err := apps.Create(ctx, slug, slug+"-"+home, alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
		require.NoError(t, err)
		return app.ID
	}
	a, b, gone := create("home-a"), create("home-b"), create("home-gone")
	require.NoError(t, standbys.Save(ctx, state.Standby{AppID: a, Host: "other", Home: home,
		LastError: "No room.", LastErrorCode: "CAPACITY_WOULD_OVERSUBSCRIBE", LastErrorRemedy: "Add a host."}))
	require.NoError(t, standbys.Save(ctx, state.Standby{AppID: b, Home: "other", StaleHost: left}))
	require.NoError(t, standbys.Save(ctx, state.Standby{AppID: gone, Home: home, StaleHost: left}))
	require.NoError(t, apps.Archive(ctx, gone))

	homed, err := standbys.HomedOn(ctx, home)
	require.NoError(t, err)
	require.Len(t, homed, 1)
	require.Equal(t, a, homed[0].AppID)
	require.Equal(t, "CAPACITY_WOULD_OVERSUBSCRIBE", homed[0].LastErrorCode)
	require.Equal(t, "Add a host.", homed[0].LastErrorRemedy)

	stale, err := standbys.LeftOn(ctx, left)
	require.NoError(t, err)
	require.Len(t, stale, 1)
	require.Equal(t, b, stale[0].AppID)

	got, found, err := standbys.Get(ctx, b)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "other", got.Home)

	none, err := standbys.HomedOn(ctx, "nowhere-"+home)
	require.NoError(t, err)
	require.Empty(t, none)
}
