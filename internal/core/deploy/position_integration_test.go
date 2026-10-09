//go:build integration

package deploy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// TestR256_AQueuedDeploySaysHowManyAreAhead asserts issue #93's half of the
// bounded queue: a deploy that waits says where it is, counted in the order
// the queue takes them, and moves up as the ones ahead are taken.
func TestR256_AQueuedDeploySaysHowManyAreAhead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, deps := queuedDeploys(t, "first", "second", "third")
	store := state.NewDeployments(db)

	for name, want := range map[string]int{"first": 0, "second": 1, "third": 2} {
		ahead, waiting, err := store.QueuePosition(ctx, deps[name].ID)
		require.NoError(t, err)
		require.True(t, waiting, name)
		require.Equal(t, want, ahead, name)
	}

	claimed, err := store.Claim(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, deps["first"].ID, claimed[0].ID, "the queue takes the oldest first")

	_, waiting, err := store.QueuePosition(ctx, deps["first"].ID)
	require.NoError(t, err)
	require.False(t, waiting, "a claimed deploy is no longer in the queue")

	list := []state.Deployment{deps["first"], deps["second"], deps["third"]}
	for i := range list {
		list[i].Status = state.DeployPending
	}
	require.NoError(t, store.QueuePositions(ctx, list))
	require.Nil(t, list[0].QueuePosition, "taken, so it has no place in the queue")
	require.Equal(t, 0, *list[1].QueuePosition, "the second is next now")
	require.Equal(t, 1, *list[2].QueuePosition)

	_, waiting, err = store.QueuePosition(ctx, "dep_01HQ8NOSUCHDEPLOYMENT00")
	require.NoError(t, err)
	require.False(t, waiting, "an unknown deploy is not waiting")
}
