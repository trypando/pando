package reconciler

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state"
)

// Event-driven reconciliation (O-52).
//
// Visiting every app every fifteen seconds costs an Observe per app per tick:
// at 20,000 apps that is over a thousand runtime calls a second and a pass
// that never finishes. Most of those visits find an app exactly as it was. So
// a runtime that can report what happens to its workloads
// (RuntimeCapabilities.SupportsBundleEvents) is followed instead, and an app
// it reports on is visited at once; an app found as it should be is then left
// for a slow sweep, which is the backstop for anything an event stream could
// not report. An app that is changing — degraded, backing off, just
// corrected, just recovered — keeps the fast cadence, and so does every app
// on a runtime without the capability, or whose stream is not open.
//
// None of this reaches a failed app. Nudge, like Claim, acts only on
// reconcilableStates, so an event for an app Pando gave up on changes nothing
// (R-151).

// Tunables, [P] (design 05 §2.3).
const (
	// DefaultSettledRevisit is how long a settled app on a runtime whose
	// events are followed waits for its next visit: the sweep that catches
	// anything an event did not report. Twenty times the fast cadence, which
	// takes 20,000 settled apps from about 1,300 Observe calls a second to
	// about 67.
	DefaultSettledRevisit = 5 * time.Minute

	// nudgeRetry is how soon an event for an app another pass holds is tried
	// again. The pass may have looked at the app before the event, so the
	// app is made due once it lets go.
	nudgeRetry = 2 * time.Second

	// recheckEvents is how long a runtime that does not report events waits
	// before it is asked again: it may be reconfigured into one that does.
	recheckEvents = 5 * time.Minute

	// eventBuffer is how many events wait to be turned into nudges. A burst
	// beyond it — a host restarting every container on it — is handled as a
	// catch-up of the whole runtime instead.
	eventBuffer = 4096
)

// watchRetry is how long a runtime's event stream waits to be opened again
// after it fails, by consecutive failures, capped at the last.
var watchRetry = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}

// eventState is what Run's event watchers share with Tick.
type eventState struct {
	mu sync.Mutex
	// live is every runtime whose event stream is open now. Only an app on
	// one of these may be left for the slow sweep.
	live map[string]bool
	// following is every runtime with a watcher running, and how to stop it.
	following map[string]context.CancelFunc

	// incoming carries app IDs from the watchers to nudgeLoop.
	incoming chan string
	// wake asks Run for a pass now. One slot: a pass already asked for
	// covers every event since.
	wake chan struct{}
}

func (r *Reconciler) events() *eventState {
	r.eventsOnce.Do(func() {
		r.ev = &eventState{
			live:      map[string]bool{},
			following: map[string]context.CancelFunc{},
			incoming:  make(chan string, eventBuffer),
			wake:      make(chan struct{}, 1),
		}
	})
	return r.ev
}

func (e *eventState) setLive(ref string, live bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if live {
		e.live[ref] = true
	} else {
		delete(e.live, ref)
	}
}

func (e *eventState) isLive(ref string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.live[ref]
}

func (e *eventState) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// settledRevisit is SettledRevisit or its default.
func (r *Reconciler) settledRevisit() time.Duration {
	if r.SettledRevisit > 0 {
		return r.SettledRevisit
	}
	return DefaultSettledRevisit
}

// release lets go of an app after a visit, choosing when it is next due.
//
// A settled app on a runtime whose events this replica is following waits for
// the slow sweep; anything else is due again after MinRevisit, as before. The
// stream being open here is what makes the wait safe: every replica follows
// every runtime, so while this replica's stream is open whatever happens to
// the app reaches it, and when the stream reopens after a gap the runtime's
// waiting apps are made due (CatchUp).
func (r *Reconciler) release(ctx context.Context, lease *state.Lease, app state.Reconcilable, seen visit) error {
	visited := app.ClaimedAt
	extra := r.settledRevisit() - r.MinRevisit
	if seen.settled && extra > 0 && r.events().isLive(seen.runtime) {
		return lease.ReleaseSettled(ctx, app.ID, visited, extra)
	}
	return lease.Release(ctx, app.ID, &visited)
}

func (r *Reconciler) clock() clock.Clock {
	if r.Clock == nil {
		return clock.System{}
	}
	return r.Clock
}

// followRuntimes starts a watcher for every runtime in Runtimes that lacks
// one, and stops the watcher of any runtime no longer there.
func (r *Reconciler) followRuntimes(ctx context.Context) {
	if r.Runtimes == nil {
		return
	}
	ev := r.events()
	want := map[string]bool{}
	for _, ref := range r.Runtimes() {
		want[ref] = true
	}

	ev.mu.Lock()
	defer ev.mu.Unlock()
	for ref, stop := range ev.following {
		if !want[ref] {
			stop()
			delete(ev.following, ref)
			delete(ev.live, ref)
		}
	}
	for ref := range want {
		if _, ok := ev.following[ref]; ok {
			continue
		}
		watchCtx, stop := context.WithCancel(ctx)
		ev.following[ref] = stop
		go r.follow(watchCtx, ref)
	}
}

// follow keeps one runtime's event stream open for as long as ctx lives,
// reopening it with backoff when it drops.
func (r *Reconciler) follow(ctx context.Context, ref string) {
	ev := r.events()
	failures := 0
	for {
		wait := r.followOnce(ctx, ref, &failures)
		ev.setLive(ref, false)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-r.clock().After(wait):
		}
	}
}

// followOnce opens the runtime's stream once and follows it until it ends,
// returning how long to wait before the next attempt.
func (r *Reconciler) followOnce(ctx context.Context, ref string, failures *int) time.Duration {
	runtime, ok := r.Registry.Runtime(ref)
	if !ok {
		return recheckEvents
	}
	caps, err := runtime.Capabilities(ctx)
	if err == nil && !caps.SupportsBundleEvents {
		// Today's cadence for every app on it (R-254: asked, not assumed).
		return recheckEvents
	}
	if err == nil {
		err = runtime.WatchBundles(ctx, func(e api.BundleEvent) { r.onEvent(ctx, ref, e, failures) })
	}
	if ctx.Err() != nil {
		return 0
	}
	wait := watchRetry[min(*failures, len(watchRetry)-1)]
	*failures++
	r.Logger.Warn("lost the runtime's event stream; its apps are visited on every pass until it is back",
		zap.String("runtime", ref), zap.Duration("retry_in", wait), zap.Error(err))
	return wait
}

// onEvent handles one event from a runtime's stream.
func (r *Reconciler) onEvent(ctx context.Context, ref string, e api.BundleEvent, failures *int) {
	ev := r.events()
	switch e.Kind {
	case api.BundleEventWatching:
		*failures = 0
		ev.setLive(ref, true)
		r.catchUp(ctx, ref)
	case api.BundleEventMissed:
		r.catchUp(ctx, ref)
	default:
		if e.BundleID == "" {
			return
		}
		select {
		case ev.incoming <- e.BundleID:
		default:
			// More at once than can be nudged one by one: look at the whole
			// runtime instead, which covers this app too.
			r.catchUp(ctx, ref)
		}
	}
}

// catchUp makes every app on the runtime that is waiting for the slow sweep
// due now, and asks for a pass.
func (r *Reconciler) catchUp(ctx context.Context, ref string) {
	n, err := r.Reconciles.CatchUp(ctx, ref)
	if err != nil {
		r.Logger.Warn("could not catch up on the runtime's apps", zap.String("runtime", ref), zap.Error(err))
		return
	}
	if n > 0 {
		r.Logger.Info("catching up on apps whose events may have been missed",
			zap.String("runtime", ref), zap.Int64("apps", n))
	}
	r.events().poke()
}

// nudgeLoop turns events into due apps, one database write per app, and asks
// for a pass when one is due.
func (r *Reconciler) nudgeLoop(ctx context.Context) {
	ev := r.events()
	// pending is every app held by a pass when its event arrived, and when it
	// was first tried.
	pending := map[string]time.Time{}
	giveUp := 2 * r.leaseDuration()
	for {
		var retry <-chan time.Time
		if len(pending) > 0 {
			retry = r.clock().After(nudgeRetry)
		}
		select {
		case <-ctx.Done():
			return
		case appID := <-ev.incoming:
			r.nudge(ctx, appID, pending)
		case <-retry:
			now := r.clock().Now()
			for appID, first := range pending {
				if now.Sub(first) > giveUp {
					// Held this long, its holder is long past the event, or
					// gone and the lease has run out.
					delete(pending, appID)
					continue
				}
				r.nudge(ctx, appID, pending)
			}
		}
	}
}

func (r *Reconciler) nudge(ctx context.Context, appID string, pending map[string]time.Time) {
	got, err := r.Reconciles.Nudge(ctx, appID)
	switch {
	case err != nil:
		r.Logger.Warn("could not mark an app for reconciliation after an event",
			zap.String("app_id", appID), zap.Error(err))
		r.keepPending(appID, pending)
	case got.Due:
		delete(pending, appID)
		r.events().poke()
	case got.Held:
		r.keepPending(appID, pending)
	default:
		// Not an app the loop acts on — failed (R-151), deleted, never
		// deployed, or not Pando's at all.
		delete(pending, appID)
	}
}

func (r *Reconciler) keepPending(appID string, pending map[string]time.Time) {
	if _, ok := pending[appID]; !ok {
		pending[appID] = r.clock().Now()
	}
}
