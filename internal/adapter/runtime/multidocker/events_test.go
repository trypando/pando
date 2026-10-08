package multidocker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// hostStreams scripts each fake host's event stream: every WatchBundles call
// takes the next step for that host.
type hostStreams struct {
	mu    sync.Mutex
	steps map[*fakeHost][]func(ctx context.Context, sink func(api.BundleEvent)) error
	calls map[*fakeHost]int
}

var streams = &hostStreams{
	steps: map[*fakeHost][]func(context.Context, func(api.BundleEvent)) error{},
	calls: map[*fakeHost]int{},
}

func (f *fakeHost) WatchBundles(ctx context.Context, sink func(api.BundleEvent)) error {
	streams.mu.Lock()
	n := streams.calls[f]
	streams.calls[f]++
	steps := streams.steps[f]
	streams.mu.Unlock()
	if n >= len(steps) {
		<-ctx.Done()
		return ctx.Err()
	}
	return steps[n](ctx, sink)
}

func script(h *fakeHost, steps ...func(context.Context, func(api.BundleEvent)) error) {
	streams.mu.Lock()
	defer streams.mu.Unlock()
	streams.steps[h] = steps
}

// opens says Watching, sends events, then stays open until released.
func opens(release <-chan struct{}, events ...api.BundleEvent) func(context.Context, func(api.BundleEvent)) error {
	return func(ctx context.Context, sink func(api.BundleEvent)) error {
		sink(api.BundleEvent{Kind: api.BundleEventWatching})
		for _, e := range events {
			sink(e)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return errDown
		}
	}
}

func refuses(context.Context, func(api.BundleEvent)) error { return errDown }

// TestEveryHostIsFollowedAndOneHostsGapIsReportedAsMissed asserts the
// multi-host stream (O-52): Watching once every host has been tried, each
// host's events passed on, and a host whose stream drops — or that was down
// at the start — reconnected on its own with a Missed when it is back, while
// the other hosts' streams carry on.
func TestEveryHostIsFollowedAndOneHostsGapIsReportedAsMissed(t *testing.T) {
	control, app1, app2 := &fakeHost{}, &fakeHost{}, &fakeHost{}
	a := newTestAdapter(t, map[string]*fakeHost{"control": control, "app-1": app1, "app-2": app2}, "control", "app-1", "app-2")
	a.watchRetry = []time.Duration{time.Millisecond}

	drop := make(chan struct{})
	script(control, opens(nil))
	script(app1,
		opens(drop, api.BundleEvent{Kind: api.BundleEventExited, BundleID: "app_a"}),
		refuses,
		opens(nil, api.BundleEvent{Kind: api.BundleEventStarted, BundleID: "app_a"}))
	// Down at the start, back on the second try.
	script(app2, refuses, opens(nil))

	var mu sync.Mutex
	var got []api.BundleEvent
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		done <- a.WatchBundles(ctx, func(e api.BundleEvent) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, e)
		})
	}()
	kinds := func() []api.BundleEventKind {
		mu.Lock()
		defer mu.Unlock()
		out := make([]api.BundleEventKind, 0, len(got))
		for _, e := range got {
			out = append(out, e.Kind)
		}
		return out
	}

	require.Eventually(t, func() bool { return len(kinds()) > 0 && kinds()[0] == api.BundleEventWatching },
		5*time.Second, time.Millisecond, "Watching once every host has been tried")
	// app-2 coming back is a gap on app-2.
	require.Eventually(t, func() bool { return count(kinds(), api.BundleEventMissed) == 1 }, 5*time.Second, time.Millisecond)

	close(drop)
	require.Eventually(t, func() bool {
		k := kinds()
		return count(k, api.BundleEventMissed) == 2 && count(k, api.BundleEventStarted) == 1
	}, 5*time.Second, time.Millisecond, "app-1 reconnects after failing once, and says it may have missed something")

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	streams.mu.Lock()
	require.Equal(t, 1, streams.calls[control], "the control host's stream was never reopened")
	streams.mu.Unlock()
}

func count(kinds []api.BundleEventKind, k api.BundleEventKind) int {
	n := 0
	for _, x := range kinds {
		if x == k {
			n++
		}
	}
	return n
}

func TestWatchBundlesNeedsHosts(t *testing.T) {
	require.Error(t, New().WatchBundles(context.Background(), func(api.BundleEvent) {}))
	require.Equal(t, time.Second, (&Adapter{}).watchRetryAfter(0))
	require.Equal(t, 30*time.Second, (&Adapter{}).watchRetryAfter(99))
}
