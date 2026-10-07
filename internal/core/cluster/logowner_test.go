package cluster

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// deploys is a deployment that stays queued until claimedAfter Waiting calls,
// then is run by runner.
type deploys struct {
	mu           sync.Mutex
	polls        int
	claimedAfter int
	runner       string
	waitErr      error
}

func (d *deploys) Waiting(context.Context, string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.polls++
	if d.waitErr != nil {
		return false, d.waitErr
	}
	return d.polls <= d.claimedAfter, nil
}

func (d *deploys) Runner(context.Context, string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.polls <= d.claimedAfter {
		return "", nil // queued: nobody runs it yet
	}
	return d.runner, nil
}

type replicaSet map[string]state.Replica

func (r replicaSet) ByID(_ context.Context, id string) (state.Replica, bool, error) {
	rep, ok := r[id]
	return rep, ok, nil
}

// TestR256_ADeploysLiveLogIsFoundOnTheReplicaThatClaimsIt asserts where a
// deploy's live log is looked for (issue #72, O-32): a queued deploy is waited
// on until a replica claims it, and the answer is that replica's address —
// or this replica, or nobody, when no other live replica holds it.
func TestR256_ADeploysLiveLogIsFoundOnTheReplicaThatClaimsIt(t *testing.T) {
	t.Parallel()
	stopped := time.Now()
	replicas := replicaSet{
		"rep_b":    {ID: "rep_b", AdvertiseURL: "http://10.0.0.2:8080"},
		"rep_gone": {ID: "rep_gone", AdvertiseURL: "http://10.0.0.3:8080", StoppedAt: &stopped},
	}
	ctx := context.Background()

	t.Run("claimed after a wait, by another replica", func(t *testing.T) {
		t.Parallel()
		d := &deploys{claimedAfter: 1, runner: "rep_b"}
		where, err := LogOwner{Self: "rep_a", Deployments: d, Replicas: replicas}.Where(ctx, "dep_1")
		require.NoError(t, err)
		require.Equal(t, "http://10.0.0.2:8080", where)
		require.Equal(t, 2, d.polls, "it waited for the claim")
	})

	t.Run("run here", func(t *testing.T) {
		t.Parallel()
		where, err := LogOwner{Self: "rep_a", Deployments: &deploys{runner: "rep_a"}, Replicas: replicas}.Where(ctx, "dep_1")
		require.NoError(t, err)
		require.Empty(t, where)
	})

	t.Run("run by a replica that has stopped", func(t *testing.T) {
		t.Parallel()
		where, err := LogOwner{Self: "rep_a", Deployments: &deploys{runner: "rep_gone"}, Replicas: replicas}.Where(ctx, "dep_1")
		require.NoError(t, err)
		require.Empty(t, where, "a stopped replica's log is not followed")
	})

	t.Run("run by a replica nobody knows", func(t *testing.T) {
		t.Parallel()
		where, err := LogOwner{Self: "rep_a", Deployments: &deploys{runner: "rep_x"}, Replicas: replicas}.Where(ctx, "dep_1")
		require.NoError(t, err)
		require.Empty(t, where)
	})

	t.Run("still queued when the wait runs out", func(t *testing.T) {
		t.Parallel()
		d := &deploys{claimedAfter: 1 << 30, runner: "rep_b"}
		start := time.Now()
		where, err := LogOwner{Self: "rep_a", Deployments: d, Replicas: replicas, ClaimWait: 100 * time.Millisecond}.
			Where(ctx, "dep_1")
		require.NoError(t, err)
		require.Empty(t, where, "nobody holds the log of a deploy nobody has claimed")
		require.Less(t, time.Since(start), 5*time.Second, "the wait is bounded")
	})

	t.Run("the request ends while it waits", func(t *testing.T) {
		t.Parallel()
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		where, err := LogOwner{Self: "rep_a", Deployments: &deploys{claimedAfter: 1 << 30}, Replicas: replicas}.
			Where(cctx, "dep_1")
		require.NoError(t, err)
		require.Empty(t, where)
	})

	t.Run("whether it is queued cannot be read", func(t *testing.T) {
		t.Parallel()
		d := &deploys{waitErr: errors.New("database is gone"), runner: "rep_b"}
		where, err := LogOwner{Self: "rep_a", Deployments: d, Replicas: replicas}.Where(ctx, "dep_1")
		require.NoError(t, err)
		require.Equal(t, "http://10.0.0.2:8080", where, "it asks who runs it rather than waiting")
	})
}
