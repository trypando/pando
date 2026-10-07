//go:build integration

package state_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

func registerReplica(t *testing.T, r *state.Replicas, name string) string {
	t.Helper()
	repID := id.New(id.Replica)
	require.NoError(t, r.Register(context.Background(), state.Replica{
		ID: repID, Hostname: name, AdvertiseURL: "http://" + name + ":8080", Version: "test",
		AssertionKID: "kid-" + name, AssertionKey: make([]byte, 32),
	}))
	return repID
}

// age moves a replica's clock back: as if it last heartbeated, and stopped if
// stopped, ago in the past. The database's own clock is the only one replicas
// share, so that is the one moved.
func age(t *testing.T, db *state.DB, repID string, ago time.Duration, stopped bool) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		UPDATE pando_replicas SET
			started_at = now() - make_interval(secs => $2) - interval '1 second',
			heartbeat_at = now() - make_interval(secs => $2),
			stopped_at = CASE WHEN $3 THEN now() - make_interval(secs => $2) END
		WHERE id = $1`, repID, ago.Seconds(), stopped)
	require.NoError(t, err)
}

func replicaIDs(reps []state.Replica) []string {
	out := make([]string, 0, len(reps))
	for _, r := range reps {
		out = append(out, r.ID)
	}
	return out
}

// TestR256_TheInstallKnowsWhichReplicasAreAlive asserts what the replicas
// table answers (R-256): which replicas are live, whose signing keys are
// still published, and which are history to prune.
func TestR256_TheInstallKnowsWhichReplicasAreAlive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	replicas := state.NewReplicas(db)

	live := registerReplica(t, replicas, "live")
	silent := registerReplica(t, replicas, "silent")          // went quiet a minute ago
	stoppedRecently := registerReplica(t, replicas, "recent") // stopped a minute ago
	longGone := registerReplica(t, replicas, "gone")          // stopped two days ago
	age(t, db, silent, time.Minute, false)
	age(t, db, stoppedRecently, time.Minute, true)
	age(t, db, longGone, 48*time.Hour, true)

	got, err := replicas.Live(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{live}, replicaIDs(got), "only a replica that heartbeated recently is live")
	require.Equal(t, "http://live:8080", got[0].AdvertiseURL)
	require.Nil(t, got[0].StoppedAt)

	rep, found, err := replicas.ByID(ctx, stoppedRecently)
	require.NoError(t, err)
	require.True(t, found, "a stopped replica can still be read")
	require.NotNil(t, rep.StoppedAt)
	require.Equal(t, "kid-recent", rep.AssertionKID)
	_, found, err = replicas.ByID(ctx, id.New(id.Replica))
	require.NoError(t, err)
	require.False(t, found)

	// An assertion lives five minutes: a replica gone a minute may have
	// signed one still in use; one gone two days has not.
	keys, err := replicas.VerificationKeys(ctx, 5*time.Minute)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{live, silent, stoppedRecently}, replicaIDs(keys))

	n, err := replicas.Prune(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "only the replica gone longer than a day is forgotten")
	_, found, err = replicas.ByID(ctx, longGone)
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = replicas.ByID(ctx, silent)
	require.NoError(t, err)
	require.True(t, found)
}

// TestR256_ARestartAskedOfOneReplicaReachesEveryReplica asserts that POST
// /restart's signal is seen by every replica that started before it, and not
// by one that started after (R-256).
func TestR256_ARestartAskedOfOneReplicaReachesEveryReplica(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	replicas := state.NewReplicas(db)

	before := registerReplica(t, replicas, "before")
	age(t, db, before, time.Second, false) // started a second ago

	pending, err := replicas.RestartPending(ctx, before)
	require.NoError(t, err)
	require.False(t, pending, "nothing asked for a restart yet")

	require.NoError(t, replicas.RequestRestart(ctx))
	pending, err = replicas.RestartPending(ctx, before)
	require.NoError(t, err)
	require.True(t, pending, "a replica that started before the request restarts")

	// The replica that comes back is a new one, started after the request.
	time.Sleep(10 * time.Millisecond)
	after := registerReplica(t, replicas, "after")
	pending, err = replicas.RestartPending(ctx, after)
	require.NoError(t, err)
	require.False(t, pending, "a replica started after the request does not restart again")

	pending, err = replicas.RestartPending(ctx, id.New(id.Replica))
	require.NoError(t, err)
	require.False(t, pending, "an unknown replica has nothing pending")
}

// TestR256_ExclusiveWorkRunsOnOneReplicaAtATime asserts that work done under
// Exclusive is serialized across connections — as replicas starting at once
// are — and that the lock is released afterwards, even when the work fails.
func TestR256_ExclusiveWorkRunsOnOneReplicaAtATime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	const key int64 = 0x7465737401

	var (
		mu      sync.Mutex
		inside  int
		maxSeen int
		wg      sync.WaitGroup
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, db.Exclusive(ctx, key, func() error {
				mu.Lock()
				inside++
				if inside > maxSeen {
					maxSeen = inside
				}
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			}))
		}()
	}
	wg.Wait()
	require.Equal(t, 1, maxSeen, "never two at once")

	boom := errors.New("boom")
	require.ErrorIs(t, db.Exclusive(ctx, key, func() error { return boom }), boom)

	// Released: another session takes it without waiting.
	conn, err := db.Acquire(ctx)
	require.NoError(t, err)
	var free bool
	require.NoError(t, conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&free))
	require.True(t, free, "the lock is given back after a failure too")
	_, err = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key)
	require.NoError(t, err)
	conn.Release()

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, db.Exclusive(canceled, key, func() error { return nil }),
		"a lock that cannot be taken runs nothing")
}

// TestR075a_OldPasscodeFailuresAreForgotten asserts that failures outside
// R-075a's window no longer count, and are pruned.
func TestR075a_OldPasscodeFailuresAreForgotten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	failures := state.NewPasscodeFailures(db)
	stale := id.New(id.App) + "|198.51.100.1"
	fresh := id.New(id.App) + "|198.51.100.2"

	require.NoError(t, failures.Record(ctx, stale, time.Hour))
	require.NoError(t, failures.Record(ctx, fresh, time.Hour))
	_, err := db.Exec(ctx, `UPDATE passcode_failures SET failed_at = now() - interval '2 hours' WHERE key = $1`, stale)
	require.NoError(t, err)

	n, err := failures.Recent(ctx, stale, time.Hour)
	require.NoError(t, err)
	require.Zero(t, n, "a failure outside the window does not count")

	require.NoError(t, failures.Prune(ctx, time.Hour))
	var rows int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM passcode_failures WHERE key = $1`, stale).Scan(&rows))
	require.Zero(t, rows, "pruned")
	n, err = failures.Recent(ctx, fresh, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n, "a recent failure survives the prune")
}
