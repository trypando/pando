//go:build integration

package cluster_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/cluster"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
)

// TestR256_TheLeaderRunsTheJobsAndStopsThemWhenItLosesTheLock asserts that
// the install-wide jobs run on the replica holding the leader lock, and are
// stopped — every one of them, before it tries again — when its connection
// goes and with it the lock (R-256: one leader for install-wide work).
func TestR256_TheLeaderRunsTheJobsAndStopsThemWhenItLosesTheLock(t *testing.T) {
	t.Parallel()
	db, ownerURL := statetest.Connect(t)

	var running, started, stopped atomic.Int32
	job := func(ctx context.Context) {
		started.Add(1)
		running.Add(1)
		<-ctx.Done()
		running.Add(-1)
		stopped.Add(1)
	}
	l := &cluster.Leader{
		Store:  state.NewReplicas(db),
		Logger: zap.NewNop(),
		Retry:  200 * time.Millisecond,
		Jobs:   []cluster.Job{{Name: "gc", Run: job}, {Name: "retention", Run: job}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()

	require.Eventually(t, func() bool { return l.Leading() && running.Load() == 2 }, 10*time.Second, 20*time.Millisecond,
		"the only replica leads and runs every job")

	// What Postgres sees of a lost pod: its backend ends.
	owner, err := pgx.Connect(context.Background(), ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	_, err = owner.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return stopped.Load() >= 2 }, 10*time.Second, 20*time.Millisecond,
		"a leader that loses the lock stops every job")

	// Nobody else holds the lock, so it leads again at its next attempt.
	require.Eventually(t, func() bool { return started.Load() >= 4 && l.Leading() }, 10*time.Second, 20*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the leader did not stop with its context")
	}
	require.False(t, l.Leading())
	require.Zero(t, running.Load(), "no job outlives the leader")

	// It resigned on the way out: another replica can lead at once.
	lead, ok, err := state.NewReplicas(db).TryLead(context.Background(), time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	lead.Resign(context.Background())
}
