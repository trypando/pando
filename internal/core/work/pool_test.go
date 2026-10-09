package work

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

// TestR256_AQueueThatCannotBeReadIsTriedAgainAtTheNextPoll asserts that a
// failed claim — the database briefly unreachable — is logged and does not
// stop the pool, and that a pool with no limit set still runs one at a time.
func TestR256_AQueueThatCannotBeReadIsTriedAgainAtTheNextPoll(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zap.WarnLevel)
	var claims, running, most atomic.Int32
	ran := make(chan int, 3)
	pool := &Pool[int]{
		Name: "flaky",
		Poll: 5 * time.Millisecond,
		Claim: func(_ context.Context, n int) ([]int, error) {
			switch claims.Add(1) {
			case 1:
				return nil, errors.New("connection refused")
			case 2:
				return []int{1, 2, 3}[:n], nil
			case 3:
				return []int{2, 3}[:n], nil
			case 4:
				return []int{3}[:n], nil
			}
			return nil, nil
		},
		Run: func(_ context.Context, item int) {
			now := running.Add(1)
			if now > most.Load() {
				most.Store(now)
			}
			time.Sleep(time.Millisecond)
			running.Add(-1)
			ran <- item
		},
		Logger: zap.New(core),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Serve(ctx)

	for want := 1; want <= 3; want++ {
		select {
		case item := <-ran:
			require.Equal(t, want, item)
		case <-time.After(5 * time.Second):
			t.Fatal("the pool stopped claiming after a failed claim")
		}
	}
	require.Equal(t, int32(1), most.Load(), "no limit set means one at a time")
	warned := logs.FilterMessage("could not take work from the queue").All()
	require.Len(t, warned, 1)
	require.Equal(t, "flaky", warned[0].ContextMap()["queue"])
}

// TestR256_AStoppingPoolReleasesWorkThatOutlastsTheDrain asserts that a
// stopping pool does not wait for good on work that ignores cancellation: once
// Drain has passed, what is still running is released anyway.
func TestR256_AStoppingPoolReleasesWorkThatOutlastsTheDrain(t *testing.T) {
	t.Parallel()
	q := &queue{waiting: []int{9}}
	started, unblock := make(chan struct{}), make(chan struct{})
	defer close(unblock)
	pool := &Pool[int]{
		Claim:   q.claim,
		Run:     func(context.Context, int) { close(started); <-unblock },
		Release: q.release,
		Drain:   20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pool.Serve(ctx)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pool waited past its drain")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	require.Equal(t, []int{9}, q.released, "still running at the drain, so handed back")
}

// TestR211_EachSkipsWhatItHadNotStartedWhenStopped asserts that a pass whose
// context ends starts nothing more, and that a bound below one is one.
func TestR211_EachSkipsWhatItHadNotStartedWhenStopped(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var visited, running, most atomic.Int32
	Each(ctx, 0, []int{1, 2, 3, 4, 5}, func(context.Context, int) {
		now := running.Add(1)
		if now > most.Load() {
			most.Store(now)
		}
		if visited.Add(1) == 2 {
			cancel()
			// Still holding the only slot, so the pass sees the context end
			// rather than a free slot.
			time.Sleep(10 * time.Millisecond)
		}
		running.Add(-1)
	})
	require.Equal(t, int32(2), visited.Load(), "nothing after the context ended")
	require.Equal(t, int32(1), most.Load(), "a bound of zero runs one at a time")
}

// TestR256_TheLimitIsReadWhileThePoolRuns asserts issue #93's change to the
// bound: the deploy limit is host policy, read each time the pool looks for
// work, so raising it lets more run without a restart and lowering it stops
// new claims until enough have finished.
func TestR256_TheLimitIsReadWhileThePoolRuns(t *testing.T) {
	t.Parallel()
	q := &queue{}
	for i := 0; i < 10; i++ {
		q.waiting = append(q.waiting, i)
	}

	var limit, running atomic.Int32
	limit.Store(1)
	gate := make(chan struct{})
	pool := &Pool[int]{
		Name:      "test",
		Poll:      5 * time.Millisecond,
		LimitFunc: func(context.Context) int { return int(limit.Load()) },
		Claim:     q.claim,
		Run: func(ctx context.Context, _ int) {
			running.Add(1)
			defer running.Add(-1)
			select {
			case <-gate:
			case <-ctx.Done():
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Serve(ctx)

	require.Eventually(t, func() bool { return running.Load() == 1 }, time.Second, time.Millisecond)
	require.Never(t, func() bool { return running.Load() > 1 }, 30*time.Millisecond, time.Millisecond,
		"one at a time while the limit is one")

	limit.Store(4)
	pool.Kick()
	require.Eventually(t, func() bool { return running.Load() == 4 }, time.Second, time.Millisecond,
		"raising the limit takes effect without restarting the pool")

	limit.Store(2)
	gate <- struct{}{} // one finishes: three left running, over the new limit of two
	require.Eventually(t, func() bool { return running.Load() == 3 }, time.Second, time.Millisecond)
	require.Never(t, func() bool { return running.Load() > 3 }, 30*time.Millisecond, time.Millisecond,
		"lowering it stops new claims; nothing running is stopped")
	close(gate)
}

// A limit below one is one: a policy read of zero must not stop the queue.
func TestALimitBelowOneRunsOne(t *testing.T) {
	p := &Pool[int]{LimitFunc: func(context.Context) int { return 0 }}
	require.Equal(t, 1, p.limit(context.Background()))
	p = &Pool[int]{Limit: -3}
	require.Equal(t, 1, p.limit(context.Background()))
}
