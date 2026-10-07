package proxy

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var visitEpoch = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func (v *visits) size(t *testing.T) int {
	n := 0
	for i := range v.shards {
		s := &v.shards[i]
		s.mu.Lock()
		n += len(s.seen)
		require.LessOrEqual(t, len(s.order)-s.head, v.limit)
		s.mu.Unlock()
	}
	return n
}

func TestAVisitIsRememberedForTheWindowAndNoLonger(t *testing.T) {
	v := newVisits()
	v.remember("user:usr_1:app_1:vis_1", visitEpoch)

	require.True(t, v.remembered("user:usr_1:app_1:vis_1", visitEpoch.Add(UseWindow-time.Second)))
	require.False(t, v.remembered("user:usr_1:app_1:vis_1", visitEpoch.Add(UseWindow)))
	require.False(t, v.remembered("user:usr_1:app_1:vis_2", visitEpoch))
}

// Remembering a visit again after its window starts a new window; the stale
// queue entry for the old one must not forget the new one when it is popped.
func TestARememberedAgainVisitKeepsItsNewWindow(t *testing.T) {
	v := newVisitsLimit(visitShards * 4)
	v.remember("k", visitEpoch)
	later := visitEpoch.Add(UseWindow + time.Minute)
	v.remember("k", later)
	// Popping the expired first entry happens on the next remember in the shard.
	v.remember("k2", later)
	require.True(t, v.remembered("k", later.Add(time.Hour)))
}

// A full visit log drops what has expired, then the oldest of one shard — not
// everything — so a full log does not re-record every visit at once.
func TestAFullVisitLogDropsTheOldestNotEverything(t *testing.T) {
	const total = visitShards * 10
	v := newVisitsLimit(total)

	now := visitEpoch
	keys := make([]string, 0, 4*total)
	for i := range 4 * total {
		now = now.Add(time.Millisecond)
		key := fmt.Sprintf("token:tok_%d:app_1", i)
		keys = append(keys, key)
		v.remember(key, now)
	}
	require.LessOrEqual(t, v.size(t), total, "the log never holds more than its bound")

	// The most recent visits all survive; the oldest are the ones dropped.
	recent := 0
	for _, key := range keys[len(keys)-total/4:] {
		if v.remembered(key, now) {
			recent++
		}
	}
	require.Equal(t, total/4, recent, "fresh visits are not dropped en masse")
	require.False(t, v.remembered(keys[0], now), "the oldest visit is the one forgotten")
}

func TestExpiredVisitsAreDroppedBeforeLiveOnes(t *testing.T) {
	v := newVisitsLimit(visitShards * 50)
	// Fill with visits that will have expired.
	for i := range visitShards * 50 {
		v.remember(fmt.Sprintf("old_%d", i), visitEpoch)
	}
	later := visitEpoch.Add(UseWindow)
	for i := range visitShards {
		v.remember(fmt.Sprintf("new_%d", i), later)
	}
	for i := range visitShards {
		require.True(t, v.remembered(fmt.Sprintf("new_%d", i), later))
	}
	require.LessOrEqual(t, v.size(t), visitShards*50)
}

func TestTheVisitLogIsSafeUnderConcurrentUse(t *testing.T) {
	v := newVisitsLimit(visitShards * 8)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := visitEpoch
			for i := range 2000 {
				now = now.Add(time.Second)
				key := fmt.Sprintf("g%d:%d", g, i%300)
				if !v.remembered(key, now) {
					v.remember(key, now)
				}
				_, _ = v.admitAnonymous("app_1", now)
			}
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, v.size(t), visitShards*8)
}
