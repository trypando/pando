package deploy

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeClock is a hand-advanced clock so eviction is tested without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func storeWithClock() (*LogStore, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	store := NewLogStore()
	store.now = clock.now
	return store, clock
}

func (s *LogStore) held() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.streams)
}

func TestAFinishedLogWithNoFollowersIsDroppedAfterRetention(t *testing.T) {
	store, clock := storeWithClock()
	w := store.Writer("dep_done")
	_, _ = io.WriteString(w, "built\n")
	require.NoError(t, w.Close())

	clock.advance(LogRetention - time.Second)
	require.True(t, store.Has("dep_done"), "a deploy that just ended keeps its log for a while")
	backlog, _, cancel := store.Follow("dep_done")
	cancel()
	require.Equal(t, []string{"built"}, backlog)

	clock.advance(2 * time.Second)
	require.False(t, store.Has("dep_done"), "past retention, the log is reported gone")

	// Sweeps are amortized, at most one per quarter retention; the next write
	// after that drops it from the map.
	clock.advance(LogRetention / 4)
	store.Writer("dep_other")
	require.Equal(t, 1, store.held())
}

func TestAFollowedLogIsNotDropped(t *testing.T) {
	store, clock := storeWithClock()
	w := store.Writer("dep_watched")
	_, ch, cancel := store.Follow("dep_watched")
	defer cancel()

	clock.advance(10 * LogRetention)
	store.Writer("dep_other") // triggers a sweep
	require.True(t, store.Has("dep_watched"))

	_, _ = io.WriteString(w, "still here\n")
	require.Equal(t, "still here", <-ch)
}

func TestARunningLogIsNotDropped(t *testing.T) {
	store, clock := storeWithClock()
	w := store.Writer("dep_running")
	_, _ = io.WriteString(w, "step 1\n")

	clock.advance(10 * LogRetention)
	store.Writer("dep_other")
	require.True(t, store.Has("dep_running"), "a deploy still writing keeps its log however long it runs")

	backlog, _, cancel := store.Follow("dep_running")
	cancel()
	require.Equal(t, []string{"step 1"}, backlog)
}

func TestFollowingAnUnknownDeployDoesNotLeaveAnEntry(t *testing.T) {
	store, clock := storeWithClock()
	for i := range 100 {
		_, _, cancel := store.Follow(fmt.Sprintf("dep_unknown_%d", i))
		cancel()
	}
	clock.advance(LogRetention)
	require.False(t, store.Has("dep_unknown_0"))

	store.Writer("dep_real")
	require.Equal(t, 1, store.held(), "only the deploy that wrote is held")
}

// The retention clock starts when the last follower leaves, not when the
// stream was created: a follower that waited a long time for a deploy to
// start does not hand the deploy an already-expired stream.
func TestRetentionCountsFromTheLastFollowerLeaving(t *testing.T) {
	store, clock := storeWithClock()
	_, _, cancel := store.Follow("dep_late")
	clock.advance(2 * LogRetention)
	cancel()

	clock.advance(time.Second)
	require.True(t, store.Has("dep_late"))
}

func TestAnExpiredLogIsReplacedNotRevived(t *testing.T) {
	store, clock := storeWithClock()
	w := store.Writer("dep_again")
	_, _ = io.WriteString(w, "old\n")
	require.NoError(t, w.Close())

	clock.advance(LogRetention)
	backlog, ch, cancel := store.Follow("dep_again")
	defer cancel()
	require.Empty(t, backlog, "an expired log is not served")

	_, _ = io.WriteString(store.Writer("dep_again"), "new\n")
	require.Equal(t, "new", <-ch)
}
