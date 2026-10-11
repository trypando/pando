//go:build integration

package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// TestR404_TheWaitIsUntilTheOldestFailureLeavesTheWindow asserts what a
// refusal over a limit tells someone (R-404): the time until the oldest
// failure in the window ages out of it, which is when one more attempt is
// allowed — and nothing to wait for when there are no failures.
func TestR404_TheWaitIsUntilTheOldestFailureLeavesTheWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	failures := state.NewFailedAttempts(db)
	key := "signin|user|" + id.New(id.User)

	wait, err := failures.Wait(ctx, key, 15*time.Minute)
	require.NoError(t, err)
	require.Zero(t, wait)

	require.NoError(t, failures.Record(ctx, key, 15*time.Minute))
	require.NoError(t, failures.Record(ctx, key, 15*time.Minute))
	_, err = db.Exec(ctx, `
		UPDATE failed_attempts SET failed_at = now() - interval '10 minutes'
		WHERE ctid = (SELECT ctid FROM failed_attempts WHERE key = $1 LIMIT 1)`, key)
	require.NoError(t, err)

	wait, err = failures.Wait(ctx, key, 15*time.Minute)
	require.NoError(t, err)
	require.InDelta(t, (5 * time.Minute).Seconds(), wait.Seconds(), 5, "measured from the oldest failure")
}
