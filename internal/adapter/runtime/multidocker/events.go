package multidocker

import (
	"context"
	"sync"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// defaultWatchRetry is how long a host's event stream waits before it is
// opened again after failing, by consecutive failures, capped at the last.
var defaultWatchRetry = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}

// WatchBundles follows every host's event stream at once, one stream per host
// (O-52), and reports what happened without acting on it (R-148).
//
// A host whose stream drops is reconnected here, with backoff, while every
// other host's stream carries on: one host restarting its daemon must not
// make the reconciler look at every app on every host. When a host's stream
// opens again after a gap, BundleEventMissed says that something on it may
// have been missed. BundleEventWatching is sent once every host's first
// attempt has opened or failed — a host that is down at the start is caught
// up with BundleEventMissed when it comes back. It returns only when ctx ends.
func (a *Adapter) WatchBundles(ctx context.Context, sink func(api.BundleEvent)) error {
	if a.control == nil {
		return errs.New(errs.AdapterUnavailable, "The Docker hosts runtime has not been set up.")
	}

	var mu sync.Mutex
	waiting := len(a.hosts)
	watching := false
	send := func(ev api.BundleEvent) {
		mu.Lock()
		defer mu.Unlock()
		// Before every host has been tried, the Watching that follows calls
		// for a look at everything anyway.
		if watching {
			sink(ev)
		}
	}
	tried := func() {
		mu.Lock()
		defer mu.Unlock()
		waiting--
		if waiting == 0 {
			watching = true
			sink(api.BundleEvent{Kind: api.BundleEventWatching})
		}
	}

	var wg sync.WaitGroup
	for _, h := range a.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.followHost(ctx, h, send, tried)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// followHost keeps one host's event stream open until ctx ends.
func (a *Adapter) followHost(ctx context.Context, h *host, send func(api.BundleEvent), tried func()) {
	first := true
	failures := 0
	for {
		_ = h.rt.WatchBundles(ctx, func(ev api.BundleEvent) {
			if ev.Kind != api.BundleEventWatching {
				send(ev)
				return
			}
			failures = 0
			if first {
				first = false
				tried()
				return
			}
			// Open again after a gap: whatever happened on this host
			// meanwhile was not seen.
			send(api.BundleEvent{Kind: api.BundleEventMissed})
		})
		if ctx.Err() != nil {
			return
		}
		if first {
			first = false
			tried()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.watchRetryAfter(failures)):
		}
		failures++
	}
}

func (a *Adapter) watchRetryAfter(failures int) time.Duration {
	schedule := a.watchRetry
	if len(schedule) == 0 {
		schedule = defaultWatchRetry
	}
	if failures >= len(schedule) {
		failures = len(schedule) - 1
	}
	return schedule[failures]
}
