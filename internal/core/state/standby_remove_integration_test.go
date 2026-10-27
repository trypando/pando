//go:build integration

package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// TestR419_RemovingAHostLeavesNoStandbyNamingIt asserts R-414 with R-419:
// removing a host clears it from every app's standby row, in the same
// transaction, so nothing ships to it, promotes from it, removes a copy from
// it or holds an app for it. A standby on it is placed again by the next
// pass; a promotion away from it goes on.
func TestR419_RemovingAHostLeavesNoStandbyNamingIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, db := clusterStore(t)
	alice := seedUser(t, db, "standby-remove-alice")
	apps := state.NewApps(db)
	standbys := state.NewStandbys(db)

	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	suffix := strings.ToLower(id.New(id.App)[4:14])
	gone, kept := "gone-"+suffix, "kept-"+suffix
	h, err := c.AddHost(ctx, state.Host{Name: gone, Roles: []string{"app"}, Address: "10.0.0.9:7443",
		CertSHA256: strings.Repeat("d", 64), JoinedWith: issued.Token.ID})
	require.NoError(t, err)

	newApp := func(name string) string {
		app, err := apps.Create(ctx, name+"-"+suffix, name+"-"+suffix, alice.ID, alice.ID,
			spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
		require.NoError(t, err)
		return app.ID
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	rows := map[string]state.Standby{
		"standby": {Host: gone, Home: kept, Copies: 3, ShippedAt: &at, SpecID: ""},
		"home":    {Host: kept, Home: gone, Copies: 2},
		"stale":   {Host: gone, Home: kept, StaleHost: gone},
		// A promotion away from the host, under way: it goes on.
		"leaving": {Host: kept, Home: kept, StaleHost: gone, PromotingAt: &at, PromotedAt: &at},
		// A promotion onto the host, under way: it cannot finish.
		"arriving": {Host: gone, Home: gone, StaleHost: kept, PromotingAt: &at, PromotedAt: &at},
		"other":    {Host: kept, Home: "elsewhere-" + suffix, Copies: 1},
	}
	ids := map[string]string{}
	for name, sb := range rows {
		sb.AppID = newApp(name)
		ids[name] = sb.AppID
		require.NoError(t, standbys.Save(ctx, sb))
	}

	_, err = c.RemoveHost(ctx, h.ID)
	require.NoError(t, err)

	get := func(name string) state.Standby {
		sb, found, err := standbys.Get(ctx, ids[name])
		require.NoError(t, err)
		require.True(t, found, name)
		return sb
	}
	for name := range rows {
		sb := get(name)
		require.NotEqual(t, gone, sb.Host, name)
		require.NotEqual(t, gone, sb.Home, name)
		require.NotEqual(t, gone, sb.StaleHost, name)
	}
	sb := get("standby")
	require.Empty(t, sb.Host, "placed again by the next pass")
	require.Zero(t, sb.Copies)
	require.Nil(t, sb.ShippedAt)
	require.Equal(t, kept, sb.Home)

	require.Equal(t, kept, get("home").Host, "its standby is kept")
	require.NotNil(t, get("leaving").PromotingAt, "a promotion away from it goes on")
	require.Nil(t, get("arriving").PromotingAt, "a promotion onto it is over")
	require.Equal(t, "elsewhere-"+suffix, get("other").Home, "rows that do not name it are untouched")
	require.Equal(t, 1, get("other").Copies)

	for _, name := range []string{"standby", "home"} {
		hold, err := standbys.Held(ctx, ids[name])
		require.NoError(t, err)
		require.False(t, hold.HomeLost || hold.HomeFenced || hold.Promoting, name)
	}
	left, err := standbys.LeftOn(ctx, gone)
	require.NoError(t, err)
	require.Empty(t, left)
	homed, err := standbys.HomedOn(ctx, gone)
	require.NoError(t, err)
	require.Empty(t, homed)
}
