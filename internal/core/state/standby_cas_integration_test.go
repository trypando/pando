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

// TestR151_APromotionIsWrittenAheadOnlyForAnAppThatHasNotFailed asserts the
// write-ahead's condition: a promotion read while the app ran is not written
// once the app has failed, so its standby is never started (R-151).
func TestR151_APromotionIsWrittenAheadOnlyForAnAppThatHasNotFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "standby-live-alice")
	apps := state.NewApps(db)
	standbys := state.NewStandbys(db)
	suffix := strings.ToLower(id.New(id.App)[4:14])
	app, err := apps.Create(ctx, "live-"+suffix, "live-"+suffix, alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	read := state.Standby{AppID: app.ID, Host: "o-" + suffix, Home: "h-" + suffix}
	ok, err := standbys.SaveIf(ctx, read, nil)
	require.NoError(t, err)
	require.True(t, ok)

	at := time.Now().UTC().Truncate(time.Microsecond)
	ahead := read
	ahead.Home, ahead.StaleHost, ahead.PromotedAt, ahead.PromotingAt = read.Host, read.Home, &at, &at
	_, err = standbys.SaveIfLive(ctx, ahead, nil)
	require.Error(t, err, "a write-ahead is made over the row it was read from")

	require.NoError(t, apps.SetState(ctx, app.ID, state.StateFailed))
	ok, err = standbys.SaveIfLive(ctx, ahead, &read)
	require.NoError(t, err)
	require.False(t, ok, "not written for a failed app")
	got, _, err := standbys.Get(ctx, app.ID)
	require.NoError(t, err)
	require.Nil(t, got.PromotingAt)

	require.NoError(t, apps.SetState(ctx, app.ID, state.StateRunning))
	ok, err = standbys.SaveIfLive(ctx, ahead, &read)
	require.NoError(t, err)
	require.True(t, ok)
}

// TestR421_ALostHostsAppsAreReadWithTheirPriorityAndDeletedAppsCopies
// asserts what the failover and a returning host read: each app homed on a
// host with its pinned spec's availability.priority, so no plan is built to
// order them (R-422), and a deleted app's copy left on a host, with its
// delete's choice about its storage (R-204), so it is removed when the host
// returns (R-419).
func TestR421_ALostHostsAppsAreReadWithTheirPriorityAndDeletedAppsCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "standby-priority-alice")
	apps := state.NewApps(db)
	standbys := state.NewStandbys(db)
	suffix := strings.ToLower(id.New(id.App)[4:14])
	home, left := "h-"+suffix, "l-"+suffix
	newApp := func(name string, priority int) string {
		app, err := apps.Create(ctx, name+"-"+suffix, name+"-"+suffix, alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
		require.NoError(t, err)
		if priority >= 0 {
			s := minimalSpec()
			s.AppID = app.ID
			s.Availability.Priority = priority
			rev, err := apps.CreateRevision(ctx, app.ID, s, spec.OriginEdited, alice.ID)
			require.NoError(t, err)
			require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateProposed, alice.ID))
		}
		return app.ID
	}
	high, unpinned := newApp("high", 7), newApp("unpinned", -1)
	for _, appID := range []string{high, unpinned} {
		require.NoError(t, standbys.Save(ctx, state.Standby{AppID: appID, Home: home, Host: left}))
	}
	homed, err := standbys.HomedOn(ctx, home)
	require.NoError(t, err)
	priority := map[string]int{}
	for _, sb := range homed {
		priority[sb.AppID] = sb.Priority
	}
	require.Equal(t, map[string]int{high: 7, unpinned: 0}, priority)

	kept, discarded := newApp("kept", -1), newApp("discarded", -1)
	for _, appID := range []string{kept, discarded} {
		require.NoError(t, standbys.Save(ctx, state.Standby{AppID: appID, Home: home, StaleHost: left}))
		require.NoError(t, apps.Archive(ctx, appID))
	}
	require.NoError(t, apps.DiscardStorage(ctx, discarded))
	leftOn, err := standbys.LeftOn(ctx, left)
	require.NoError(t, err)
	require.Len(t, leftOn, 2, "a deleted app's copy is still removed")
	fate, err := standbys.Fate(ctx, kept)
	require.NoError(t, err)
	require.Equal(t, state.AppFate{Deleted: true}, fate)
	fate, err = standbys.Fate(ctx, discarded)
	require.NoError(t, err)
	require.Equal(t, state.AppFate{Deleted: true, DiscardStorage: true}, fate)
	fate, err = standbys.Fate(ctx, high)
	require.NoError(t, err)
	require.Equal(t, state.AppFate{}, fate)
	homed, err = standbys.HomedOn(ctx, home)
	require.NoError(t, err)
	require.Len(t, homed, 2, "a deleted app is not failed over")
}

// TestR153_AStaleStandbyReadCannotPutBackAClearedStaleHost asserts that the
// compare-and-set covers every column a concurrent writer may change, not
// only the ones a promotion changes: a write from a reading taken before a
// stale copy was removed and its stale_host cleared is refused, so the copy
// is not put back on the record and removed twice (R-153, R-419). The row's
// version changes on every write, whoever makes it; a row forgotten
// meanwhile is not written back.
func TestR153_AStaleStandbyReadCannotPutBackAClearedStaleHost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "standby-version-alice")
	apps := state.NewApps(db)
	standbys := state.NewStandbys(db)
	suffix := strings.ToLower(id.New(id.App)[4:14])
	app, err := apps.Create(ctx, "ver-"+suffix, "ver-"+suffix, alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	home, left := "h-"+suffix, "l-"+suffix
	ok, err := standbys.SaveIf(ctx, state.Standby{AppID: app.ID, Home: home, StaleHost: left}, nil)
	require.NoError(t, err)
	require.True(t, ok)

	// Two leaders read the row; the first removes the stale copy.
	first, _, err := standbys.Get(ctx, app.ID)
	require.NoError(t, err)
	second, _, err := standbys.Get(ctx, app.ID)
	require.NoError(t, err)
	removed := first
	removed.StaleHost, removed.Host = "", left
	ok, err = standbys.SaveIf(ctx, removed, &first)
	require.NoError(t, err)
	require.True(t, ok)

	// The second, from its earlier reading, loses.
	shipped := second
	shipped.Copies = 3
	ok, err = standbys.SaveIf(ctx, shipped, &second)
	require.NoError(t, err)
	require.False(t, ok, "a write from before the stale host was cleared is refused")
	got, _, err := standbys.Get(ctx, app.ID)
	require.NoError(t, err)
	require.Empty(t, got.StaleHost)
	require.Equal(t, left, got.Host)

	// A write from outside SaveIf (a host's removal from the cluster)
	// changes what SaveIf compares too.
	_, err = db.Exec(ctx, `UPDATE app_standbys SET copies = 1 WHERE app_id = $1`, app.ID)
	require.NoError(t, err)
	ok, err = standbys.SaveIf(ctx, got, &got)
	require.NoError(t, err)
	require.False(t, ok)

	// Forgotten meanwhile: not written back.
	now, _, err := standbys.Get(ctx, app.ID)
	require.NoError(t, err)
	require.NoError(t, standbys.Delete(ctx, app.ID))
	ok, err = standbys.SaveIf(ctx, now, &now)
	require.NoError(t, err)
	require.False(t, ok)
	_, found, err := standbys.Get(ctx, app.ID)
	require.NoError(t, err)
	require.False(t, found)
}
