//go:build integration

package state_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
)

func lastActivity(t *testing.T, db *state.DB, appID string) *time.Time {
	t.Helper()
	var at *time.Time
	err := db.QueryRow(context.Background(),
		`SELECT last_activity_at FROM app_activity WHERE app_id = $1`, appID).Scan(&at)
	if err != nil {
		return nil
	}
	return at
}

// TestR394_ADeployAndAStartAreActivity asserts R-394 at the database: the
// triggers of migration 69 record a deploy and a start whatever path wrote
// them, and a start clears a stop for inactivity and every notice (R-396).
func TestR394_ADeployAndAStartAreActivity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "idle-alice")
	apps := state.NewApps(db)
	activity := state.NewActivity(db)

	app, err := apps.Create(ctx, "idle notes", "idle-notes", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	assert.Nil(t, lastActivity(t, db, app.ID), "an app never used counts from its creation, with no row")

	rev, err := apps.CreateRevision(ctx, app.ID, minimalSpec(), spec.OriginManual, alice.ID)
	require.NoError(t, err)
	_, err = state.NewDeployments(db).Create(ctx, app.ID, rev.ID, "manual", alice.ID)
	require.NoError(t, err)
	deployed := lastActivity(t, db, app.ID)
	require.NotNil(t, deployed, "a deploy is activity")

	// Stopped for being idle, with a notice outstanding.
	require.NoError(t, apps.SetDesiredState(ctx, app.ID, state.StateRunning))
	stopped, err := activity.StopForIdle(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, stopped)
	now := time.Now()
	require.NoError(t, activity.SetIdleNotice(ctx, app.ID, state.IdleNoticeDelete, &now))
	got, _, err := apps.ByID(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, got.StoppedForIdle)
	require.Equal(t, state.StateStopped, got.DesiredState)

	// Somebody starts it.
	require.NoError(t, apps.SetDesiredState(ctx, app.ID, state.StateRunning))
	got, _, err = apps.ByID(ctx, app.ID)
	require.NoError(t, err)
	assert.False(t, got.StoppedForIdle, "starting it ends the stop for inactivity")
	status, err := activity.IdleStatus(ctx, app.ID)
	require.NoError(t, err)
	assert.Nil(t, status.DeleteNoticedAt, "and withdraws the notice")
	assert.False(t, lastActivity(t, db, app.ID).Before(*deployed), "and is activity")

	// An owner's stop is not Pando's.
	require.NoError(t, apps.SetDesiredState(ctx, app.ID, state.StateStopped))
	stopped, err = activity.StopForIdle(ctx, app.ID)
	require.NoError(t, err)
	assert.False(t, stopped, "an app its owner stopped is left as they left it")
}

// TestR394_ProxyActivityNeverMovesTheClockBack asserts R-394: replicas flush
// independently, and an older batch landing second keeps the newer time; an
// app deleted since its request does not fail the batch.
func TestR394_ProxyActivityNeverMovesTheClockBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "idle-bob")
	appID := seedApp(t, db, alice.ID)
	activity := state.NewActivity(db)

	newer := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, activity.RecordActivity(ctx, map[string]time.Time{appID: newer, "app_gone": newer}))
	require.NoError(t, activity.RecordActivity(ctx, map[string]time.Time{appID: newer.Add(-time.Hour)}))
	assert.True(t, lastActivity(t, db, appID).Equal(newer))
}

// TestR393_IdleCandidatesAreTheAppsOwedSomething asserts R-393 and R-397 at
// the query: the install's days apply unless the app sets its own, 0 turns
// it off, and an app idle less than the setting less the notice is not read.
func TestR393_IdleCandidatesAreTheAppsOwedSomething(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedUser(t, db, "idle-carol")
	activity := state.NewActivity(db)

	old, recent, optedOut, ownDays := seedApp(t, db, owner.ID), seedApp(t, db, owner.ID), seedApp(t, db, owner.ID), seedApp(t, db, owner.ID)
	ago := func(days int) time.Time { return time.Now().Add(-time.Duration(days) * 24 * time.Hour) }
	// Made long ago: an app counts from its creation when that is later than
	// its activity.
	_, err := db.Exec(ctx, `UPDATE apps SET created_at = $2 WHERE id = ANY($1)`,
		[]string{old, recent, optedOut, ownDays}, ago(500))
	require.NoError(t, err)
	require.NoError(t, activity.RecordActivity(ctx, map[string]time.Time{
		old: ago(25), recent: ago(2), optedOut: ago(400), ownDays: ago(25),
	}))
	zero, sixty := 0, 60
	require.NoError(t, activity.SetIdleSettings(ctx, optedOut, state.IdleSettings{StopDays: &zero}))
	require.NoError(t, activity.SetIdleSettings(ctx, ownDays, state.IdleSettings{StopDays: &sixty}))

	got, err := activity.IdleCandidates(ctx, state.IdleDefaults{StopDays: 30, NoticeDays: 7}, time.Now())
	require.NoError(t, err)
	ids := map[string]state.IdleApp{}
	for _, a := range got {
		ids[a.AppID] = a
	}
	require.Contains(t, ids, old, "idle 25 days of 30 is owed a notice")
	assert.Equal(t, 30, ids[old].StopDays)
	assert.Equal(t, "notes", ids[old].Name)
	assert.NotContains(t, ids, recent)
	assert.NotContains(t, ids, optedOut)
	assert.NotContains(t, ids, ownDays, "its own 60 days are not near")
}

// TestR244_TwoCreatesAtOnceCannotBothTakeTheLastPlace asserts R-244 at the
// database: the count and the insert share a transaction under a lock on the
// owner, so a person one short of the limit gets exactly one more app.
func TestR244_TwoCreatesAtOnceCannotBothTakeTheLastPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	dana := seedUser(t, db, "limit-dana")
	apps := state.NewApps(db)
	src := spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"}

	_, err := apps.CreateWithin(ctx, "limit one", "limit-one", dana.ID, dana.ID, src, 2)
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := []string{"limit two", "limit three", "limit four", "limit five"}[i]
			_, results[i] = apps.CreateWithin(ctx, name, name, dana.ID, dana.ID, src, 2)
		}(i)
	}
	wg.Wait()

	created, refused := 0, 0
	for _, err := range results {
		var over *state.AppLimitReached
		switch {
		case err == nil:
			created++
		case errors.As(err, &over):
			refused++
			assert.Equal(t, 2, over.Limit)
			assert.Equal(t, 2, over.Owned)
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, created)
	assert.Equal(t, 3, refused)
}

// TestR244_GroupLimitsReachThroughLinkedGroups asserts R-244 with R-079: a
// person's groups are read through effective_group_members, and only groups
// that set a value are returned.
func TestR244_GroupLimitsReachThroughLinkedGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	erin := seedUser(t, db, "limit-erin")
	groups := state.NewGroups(db)
	limits := state.NewAppLimits(db)

	finance, err := groups.Create(ctx, "Finance limit")
	require.NoError(t, err)
	unset, err := groups.Create(ctx, "Unset limit")
	require.NoError(t, err)
	require.NoError(t, groups.AddMember(ctx, finance.ID, erin.ID))
	require.NoError(t, groups.AddMember(ctx, unset.ID, erin.ID))
	five := 5
	require.NoError(t, limits.SetGroupMaxApps(ctx, finance.ID, &five))
	seedApp(t, db, erin.ID)

	facts, err := limits.Facts(ctx, erin.ID)
	require.NoError(t, err)
	assert.Nil(t, facts.UserMaxApps)
	assert.Equal(t, 1, facts.Owned)
	require.Len(t, facts.Groups, 1)
	assert.Equal(t, state.GroupLimit{GroupID: finance.ID, Name: "Finance limit", MaxApps: 5}, facts.Groups[0])

	two := 2
	require.NoError(t, limits.SetUserMaxApps(ctx, erin.ID, &two))
	facts, err = limits.Facts(ctx, erin.ID)
	require.NoError(t, err)
	require.NotNil(t, facts.UserMaxApps)
	assert.Equal(t, 2, *facts.UserMaxApps)

	require.Error(t, limits.SetGroupMaxApps(ctx, "grp_missing", &two))
}
