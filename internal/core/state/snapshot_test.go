package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// The copy's operations say so when the database cannot be reached or its URL
// names nothing, rather than reporting success (R-359).
func TestTheDatabaseCopyReportsAnUnreachableDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	unreachable := "postgres://pando:pw@127.0.0.1:1/pando?connect_timeout=1"

	name, err := state.SnapshotName(unreachable)
	require.NoError(t, err)
	require.Equal(t, "pando"+state.SnapshotSuffix, name)

	require.ErrorContains(t, state.CanCopyDatabase(ctx, unreachable), "Could not reach Pando's database")
	require.ErrorContains(t, state.Snapshot(ctx, unreachable), "maintenance database")
	require.ErrorContains(t, state.RestoreSnapshot(ctx, unreachable), "maintenance database")
	require.ErrorContains(t, state.DropSnapshot(ctx, unreachable), "maintenance database")

	for _, bad := range []string{"postgres://pando:pw@127.0.0.1:1/", "::not a url"} {
		_, err := state.SnapshotName(bad)
		require.Error(t, err, bad)
		require.Error(t, state.Snapshot(ctx, bad), bad)
		require.Error(t, state.RestoreSnapshot(ctx, bad), bad)
		require.Error(t, state.DropSnapshot(ctx, bad), bad)
	}
}
