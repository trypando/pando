//go:build integration

package state_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/tokenkey"
	"github.com/trypando/pando/internal/id"
)

// TestR153_AStandbyRowIsWrittenOnlyOverWhatWasRead asserts migration 000091's
// standby half: a write made from a row read before a promotion is refused,
// so the promoted home is never put back (R-153); a row whose promotion is
// under way is not forgotten, is listed for the host it is leaving, and holds
// the app from the reconciler, as a lost or fenced home does (R-421).
func TestR153_AStandbyRowIsWrittenOnlyOverWhatWasRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "standby-cas-alice")
	apps := state.NewApps(db)
	standbys := state.NewStandbys(db)
	cluster := state.NewCluster(db, bytes.Repeat([]byte{7}, tokenkey.Size))
	suffix := strings.ToLower(id.New(id.App)[4:14])
	home, other := "h-"+suffix, "o-"+suffix
	app, err := apps.Create(ctx, "cas-"+suffix, "cas-"+suffix, alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)

	first := state.Standby{AppID: app.ID, Host: other, Home: home}
	ok, err := standbys.SaveIf(ctx, first, nil)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = standbys.SaveIf(ctx, first, nil)
	require.NoError(t, err)
	require.False(t, ok, "nil prev writes only where there is no row")

	read, _, err := standbys.Get(ctx, app.ID)
	require.NoError(t, err)

	// A promotion, written ahead.
	at := time.Now().UTC().Truncate(time.Microsecond)
	ahead := read
	ahead.Home, ahead.StaleHost, ahead.PromotedAt, ahead.PromotingAt = other, home, &at, &at
	ok, err = standbys.SaveIf(ctx, ahead, &read)
	require.NoError(t, err)
	require.True(t, ok)

	// The pass's write, from its earlier reading, loses.
	pass := read
	pass.Copies = 2
	ok, err = standbys.SaveIf(ctx, pass, &read)
	require.NoError(t, err)
	require.False(t, ok)
	got, _, err := standbys.Get(ctx, app.ID)
	require.NoError(t, err)
	require.Equal(t, other, got.Home)
	require.NotNil(t, got.PromotingAt)

	// Under way: listed for the host it leaves, not forgotten, and holding.
	homed, err := standbys.HomedOn(ctx, home)
	require.NoError(t, err)
	require.Len(t, homed, 1)
	homed, err = standbys.HomedOn(ctx, other)
	require.NoError(t, err)
	require.Empty(t, homed, "not homed on the new host until it is done")
	require.NoError(t, standbys.Delete(ctx, app.ID))
	_, found, err := standbys.Get(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, found)
	hold, err := standbys.Held(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, hold.Promoting)

	// Done: written over the row as written ahead.
	done := got
	done.PromotingAt, done.Host = nil, ""
	ok, err = standbys.SaveIf(ctx, done, &got)
	require.NoError(t, err)
	require.True(t, ok)
	hold, err = standbys.Held(ctx, app.ID)
	require.NoError(t, err)
	require.Equal(t, state.Hold{Home: other}, hold, "a home that is not a cluster host holds nothing")

	// A lost or fenced home holds the app, and the pass is told it is lost.
	h, err := cluster.AddHost(ctx, state.Host{Name: other, Roles: []string{"app"}, Address: "10.0.0.6:7443", CertSHA256: strings.Repeat("0", 64)})
	require.NoError(t, err)
	changed, err := cluster.SetHostFenced(ctx, h.ID, true)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = cluster.SetHostFenced(ctx, h.ID, true)
	require.NoError(t, err)
	require.False(t, changed, "unchanged is not written again")
	hold, err = standbys.Held(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, hold.HomeFenced)
	_, err = cluster.SetHostFenced(ctx, h.ID, false)
	require.NoError(t, err)
	_, err = cluster.MarkHostLost(ctx, h.ID, "liveness")
	require.NoError(t, err)
	hold, err = standbys.Held(ctx, app.ID)
	require.NoError(t, err)
	require.Equal(t, state.Hold{Home: other, HomeLost: true}, hold)
	candidates, err := standbys.Candidates(ctx)
	require.NoError(t, err)
	var mine *state.StandbyApp
	for i := range candidates {
		if candidates[i].AppID == app.ID {
			mine = &candidates[i]
		}
	}
	require.NotNil(t, mine)
	require.True(t, mine.HomeLost)

	none, err := standbys.Held(ctx, "app_nobody")
	require.NoError(t, err)
	require.Equal(t, state.Hold{}, none)
}
