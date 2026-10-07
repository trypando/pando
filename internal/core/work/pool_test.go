package work

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// queue is an in-memory stand-in for a table of queued work: Claim takes from
// the front, as the SKIP LOCKED query takes the oldest.
type queue struct {
	mu       sync.Mutex
	waiting  []int
	released []int
}

func (q *queue) claim(_ context.Context, n int) ([]int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n > len(q.waiting) {
		n = len(q.waiting)
	}
	out := append([]int(nil), q.waiting[:n]...)
	q.waiting = q.waiting[n:]
	return out, nil
}

func (q *queue) release(_ context.Context, items []int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.released = append(q.released, items...)
}

// TestR256_TheDeployQueueRunsNoMoreThanItsLimit asserts the bound O-32 is
// for: a replica asked for forty deploys at once runs its limit at a time,
// and runs every one.
func TestR256_TheDeployQueueRunsNoMoreThanItsLimit(t *testing.T) {
	t.Parallel()
	q := &queue{}
	for i := 0; i < 40; i++ {
		q.waiting = append(q.waiting, i)
	}

	var running, most atomic.Int32
	var mu sync.Mutex
	var ran []int
	pool := &Pool[int]{
		Name:  "test",
		Limit: 3,
		Poll:  5 * time.Millisecond,
		Claim: q.claim,
		Run: func(_ context.Context, item int) {
			now := running.Add(1)
			for {
				prev := most.Load()
				if now <= prev || most.CompareAndSwap(prev, now) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			running.Add(-1)
			mu.Lock()
			ran = append(ran, item)
			mu.Unlock()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pool.Serve(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ran) == 40
	}, 10*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	require.LessOrEqual(t, most.Load(), int32(3), "never more than the limit at once")
	require.Equal(t, int32(3), most.Load(), "and the limit is used")
	sort.Ints(ran)
	for i, item := range ran {
		require.Equal(t, i, item, "each item ran exactly once")
	}
}

// TestR256_AStoppingReplicaHandsBackWhatItWasRunning asserts that a pool told
// to stop cancels what it is running and releases it, so another replica takes
// it at once rather than after this one is taken for dead.
func TestR256_AStoppingReplicaHandsBackWhatItWasRunning(t *testing.T) {
	t.Parallel()
	q := &queue{waiting: []int{1, 2, 3, 4}}
	started := make(chan int, 4)
	pool := &Pool[int]{
		Limit: 2,
		Poll:  time.Hour,
		Claim: q.claim,
		Run: func(ctx context.Context, item int) {
			started <- item
			<-ctx.Done() // a long deploy, honoring cancellation
		},
		Release: q.release,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pool.Serve(ctx)
		close(done)
	}()
	<-started
	<-started
	require.Equal(t, 2, pool.Running())

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pool did not stop")
	}

	sort.Ints(q.released)
	require.Equal(t, []int{1, 2}, q.released, "the two it was running go back")
	require.Equal(t, []int{3, 4}, q.waiting, "and the two it never took are still queued")
}

// TestR256_KickRunsQueuedWorkWithoutWaitingForAPoll asserts that work queued
// by this replica starts at once, not at the next poll.
func TestR256_KickRunsQueuedWorkWithoutWaitingForAPoll(t *testing.T) {
	t.Parallel()
	q := &queue{}
	ran := make(chan int, 1)
	pool := &Pool[int]{Limit: 1, Poll: time.Hour, Claim: q.claim, Run: func(_ context.Context, item int) { ran <- item }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Serve(ctx)

	time.Sleep(20 * time.Millisecond) // past the pool's first look
	q.mu.Lock()
	q.waiting = append(q.waiting, 7)
	q.mu.Unlock()
	pool.Kick()

	select {
	case item := <-ran:
		require.Equal(t, 7, item)
	case <-time.After(5 * time.Second):
		t.Fatal("kicked work did not run")
	}
}

// TestR211_EachRunsAtMostNAtOnce asserts the bound on a leader job's pass —
// rolling backups, the auto-deploy poll — and that every item is visited.
func TestR211_EachRunsAtMostNAtOnce(t *testing.T) {
	t.Parallel()
	items := make([]int, 25)
	for i := range items {
		items[i] = i
	}
	var running, most, visited atomic.Int32
	Each(context.Background(), 4, items, func(context.Context, int) {
		now := running.Add(1)
		for {
			prev := most.Load()
			if now <= prev || most.CompareAndSwap(prev, now) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		running.Add(-1)
		visited.Add(1)
	})
	require.Equal(t, int32(25), visited.Load())
	require.LessOrEqual(t, most.Load(), int32(4))
}
