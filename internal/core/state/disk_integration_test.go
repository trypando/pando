//go:build integration

package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
)

// TestR403_AnAppStoppedForDiskIsStartedClear asserts R-403 at the database:
// the disk pass sees a running app with its pinned limit, a stop records why
// in the same write, starting the app clears it and both marks (migration
// 71), and an app its owner stopped is left as they left it.
func TestR403_AnAppStoppedForDiskIsStartedClear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "disk-alice")
	apps := state.NewApps(db)
	disk := state.NewDisk(db)

	app, err := apps.Create(ctx, "disk notes", "disk-notes", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	body := minimalSpec()
	body.Resources.DiskBytes = 3 << 30
	rev, err := apps.CreateRevision(ctx, app.ID, body, spec.OriginManual, alice.ID)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, alice.ID))
	require.NoError(t, apps.SetDesiredState(ctx, app.ID, state.StateRunning))

	candidate := func() *state.DiskApp {
		list, err := disk.DiskCandidates(ctx)
		require.NoError(t, err)
		for i := range list {
			if list[i].AppID == app.ID {
				return &list[i]
			}
		}
		return nil
	}
	c := candidate()
	require.NotNil(t, c, "a running app with a pinned spec is measured")
	assert.Equal(t, int64(3<<30), c.DiskBytes, "its limit from the pinned spec")
	assert.Equal(t, alice.ID, c.OwnerUserID)

	now := time.Now()
	require.NoError(t, disk.SetDiskMark(ctx, app.ID, state.DiskWarned, &now))
	require.NoError(t, disk.SetDiskMark(ctx, app.ID, state.DiskOver, &now))
	stopped, err := disk.StopForDisk(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, stopped)
	got, _, err := apps.ByID(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, got.StoppedForDisk)
	require.Equal(t, state.StateStopped, got.DesiredState)
	assert.Nil(t, candidate(), "a stopped app is not measured")

	require.NoError(t, apps.SetDesiredState(ctx, app.ID, state.StateRunning))
	got, _, err = apps.ByID(ctx, app.ID)
	require.NoError(t, err)
	assert.False(t, got.StoppedForDisk, "starting it ends the stop")
	c = candidate()
	require.NotNil(t, c)
	assert.Nil(t, c.WarnedAt, "and clears the warning")
	assert.Nil(t, c.OverAt, "and the reading over")

	require.NoError(t, apps.SetDesiredState(ctx, app.ID, state.StateStopped))
	stopped, err = disk.StopForDisk(ctx, app.ID)
	require.NoError(t, err)
	assert.False(t, stopped, "an app its owner stopped is left as they left it")
}
