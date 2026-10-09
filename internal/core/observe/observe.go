// Package observe shares what a runtime reports about an app between the
// requests that ask for it at the same moment (issue #72).
//
// The console asks for an app's status every few seconds and for its usage a
// little less often, from every tab that has the app open. Each status is a
// container listing and an inspect per container; each usage reading samples
// container stats for about a second. With thousands of people looking at the
// same apps, asking the runtime once per request drives it harder than the
// apps it is running. So each replica asks once per app per short window and
// hands the answer to everyone who asked in it.
//
// Only what the runtime reports is kept here. Whether the person asking may see
// it is decided per request, before the cache is consulted, and never stored
// (R-274): a cache of decisions would keep answering after a grant was revoked.
//
// The reconciler does not use this. It needs the runtime's answer now, not one
// a status poll fetched two seconds ago (design 05).
package observe

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
)

// Defaults. Short enough that a person who just started or stopped an app sees
// it within a poll or two even on a replica that did not handle the request.
const (
	DefaultObserveTTL  = 2 * time.Second
	DefaultUsageTTL    = 5 * time.Second
	DefaultCallTimeout = 15 * time.Second

	// DefaultRuntimeTTL is how long what a runtime says about itself — its
	// capabilities and its capacity — is reused for a page that shows it
	// [P]. These change when an adapter is reconfigured or a host is added,
	// not between two polls of the same app.
	DefaultRuntimeTTL = 30 * time.Second

	// sweepEvery is how often expired entries are dropped wholesale, on top of
	// being dropped when they are next asked for. Apps nobody looks at again
	// would otherwise stay in memory until the process ends.
	sweepEvery = 30 * time.Second
)

// Cache is one replica's short-lived copy of runtime observations. The zero
// value is not usable; use New. A nil *Cache is: every call goes straight to
// the runtime.
//
// Values handed out are shared between callers and must be treated as
// read-only.
type Cache struct {
	clock       clock.Clock
	observeTTL  time.Duration
	usageTTL    time.Duration
	runtimeTTL  time.Duration
	callTimeout time.Duration

	group singleflight.Group

	mu        sync.Mutex
	entries   map[key]entry
	forgotten map[key]time.Time // when Forget last ran for the key
	// flightKeys are the keys with a call to the runtime in flight, so Forget
	// can detach them from later callers.
	flightKeys map[key]struct{}
	lastSweep  time.Time
}

// Option configures a Cache.
type Option func(*Cache)

// WithClock sets the time source, for tests.
func WithClock(c clock.Clock) Option { return func(o *Cache) { o.clock = c } }

// WithRuntimeTTL sets how long a runtime's capabilities and capacity are
// reused.
func WithRuntimeTTL(d time.Duration) Option { return func(o *Cache) { o.runtimeTTL = d } }

// New returns a Cache with the default TTLs.
func New(opts ...Option) *Cache {
	c := &Cache{
		clock:       clock.System{},
		observeTTL:  DefaultObserveTTL,
		usageTTL:    DefaultUsageTTL,
		runtimeTTL:  DefaultRuntimeTTL,
		callTimeout: DefaultCallTimeout,
		entries:     map[key]entry{},
		forgotten:   map[key]time.Time{},
		flightKeys:  map[key]struct{}{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type kind uint8

const (
	kindObserve kind = iota + 1
	kindUsage
	kindCapabilities
	kindCapacity
)

// key names one answer: the same app on two runtimes is two answers.
type key struct {
	kind    kind
	runtime string // the spec's runtime adapter ref
	appID   string
}

func (k key) String() string {
	prefix := "observe"
	switch k.kind {
	case kindUsage:
		prefix = "usage"
	case kindCapabilities:
		prefix = "capabilities"
	case kindCapacity:
		prefix = "capacity"
	}
	return prefix + "\x00" + k.runtime + "\x00" + k.appID
}

type entry struct {
	value   any
	expires time.Time
}

// Observe returns what runtime reports for the app's bundle, asking it at most
// once per TTL on this replica. runtimeRef is the adapter ref the app's spec
// names, and is part of the key.
func (c *Cache) Observe(ctx context.Context, runtimeRef string, runtime api.RuntimeAdapter, appID string) (api.ObservedBundle, error) {
	ref := api.BundleRef{BundleID: appID}
	if c == nil {
		return runtime.Observe(ctx, ref)
	}
	return load(ctx, c, key{kindObserve, runtimeRef, appID}, c.observeTTL,
		func(ctx context.Context) (api.ObservedBundle, error) { return runtime.Observe(ctx, ref) })
}

// Usage returns the runtime's usage reading for the app's bundle, sampling at
// most once per TTL on this replica.
func (c *Cache) Usage(ctx context.Context, runtimeRef string, runtime api.RuntimeAdapter, appID string) (api.BundleUsage, error) {
	ref := api.BundleRef{BundleID: appID}
	if c == nil {
		return runtime.Usage(ctx, ref)
	}
	return load(ctx, c, key{kindUsage, runtimeRef, appID}, c.usageTTL,
		func(ctx context.Context) (api.BundleUsage, error) { return runtime.Usage(ctx, ref) })
}

// Capabilities returns what the runtime says it can do, asking it at most
// once per runtime TTL on this replica. Shown with a reading, not used to plan
// or enforce: the planner asks the runtime itself.
func (c *Cache) Capabilities(ctx context.Context, runtimeRef string, runtime api.RuntimeAdapter) (api.RuntimeCapabilities, error) {
	if c == nil {
		return runtime.Capabilities(ctx)
	}
	return load(ctx, c, key{kind: kindCapabilities, runtime: runtimeRef}, c.runtimeTTL, runtime.Capabilities)
}

// Capacity returns the runtime's totals, asking it at most once per runtime
// TTL on this replica. For display, like Capabilities: the capacity check at
// plan time asks the runtime itself (R-242).
func (c *Cache) Capacity(ctx context.Context, runtimeRef string, runtime api.RuntimeAdapter) (api.Capacity, error) {
	if c == nil {
		return runtime.Capacity(ctx)
	}
	return load(ctx, c, key{kind: kindCapacity, runtime: runtimeRef}, c.runtimeTTL, runtime.Capacity)
}

// Forget drops what this replica holds for the app, so the next request asks
// the runtime. Called after an action that changes what is running. A call
// already in flight when Forget runs is not stored when it returns.
func (c *Cache) Forget(appID string) {
	if c == nil {
		return
	}
	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.appID == appID {
			delete(c.entries, k)
		}
	}
	for _, kd := range []kind{kindObserve, kindUsage} {
		// Runtime refs are not known here; the in-flight guard is keyed per
		// app, with runtime left empty.
		k := key{kind: kd, appID: appID}
		c.forgotten[k] = now
	}
	// New callers must not join a flight that began before the action.
	c.forgetFlights(appID)
}

// forgetFlights detaches in-flight calls for the app from new callers.
// singleflight has no prefix forget, so this walks the keys it can name: those
// entries the cache has seen for the app. Called with c.mu held.
func (c *Cache) forgetFlights(appID string) {
	for k := range c.flightKeys {
		if k.appID == appID {
			c.group.Forget(k.String())
			delete(c.flightKeys, k)
		}
	}
}

func load[T any](ctx context.Context, c *Cache, k key, ttl time.Duration, fetch func(context.Context) (T, error)) (T, error) {
	var zero T
	now := c.clock.Now()

	c.mu.Lock()
	if e, ok := c.entries[k]; ok {
		if now.Before(e.expires) {
			c.mu.Unlock()
			return e.value.(T), nil
		}
		delete(c.entries, k)
	}
	c.sweepLocked(now)
	c.flightKeys[k] = struct{}{}
	c.mu.Unlock()

	ch := c.group.DoChan(k.String(), func() (any, error) {
		started := c.clock.Now()
		defer func() {
			c.mu.Lock()
			delete(c.flightKeys, k)
			c.mu.Unlock()
		}()
		// Looked again: a flight that ended between this caller's miss above
		// and its joining here has stored an answer, and asking the runtime a
		// second time for it is the call this cache exists to save.
		c.mu.Lock()
		if e, ok := c.entries[k]; ok && started.Before(e.expires) {
			c.mu.Unlock()
			return e.value, nil
		}
		c.mu.Unlock()
		// Detached from the caller who happened to start it: the others
		// waiting on it should not fail because that one went away. Values
		// (the logger, the request ID) are kept.
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.callTimeout)
		defer cancel()
		v, err := fetch(callCtx)
		if err != nil {
			// Not stored: the next request asks again. Callers waiting at the
			// same moment still share the one failure.
			return nil, err
		}
		c.store(k, v, started, ttl)
		return v, nil
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return zero, res.Err
		}
		return res.Val.(T), nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func (c *Cache) store(k key, v any, started time.Time, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.forgotten[key{kind: k.kind, appID: k.appID}]; ok && !started.After(at) {
		return // asked before an action that changed the app; stale on arrival
	}
	c.entries[k] = entry{value: v, expires: c.clock.Now().Add(ttl)}
}

// sweepLocked drops expired entries and Forget markers no flight can still be
// older than. Called with c.mu held.
func (c *Cache) sweepLocked(now time.Time) {
	if now.Sub(c.lastSweep) < sweepEvery {
		return
	}
	c.lastSweep = now
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	for k, at := range c.forgotten {
		if now.Sub(at) > c.callTimeout {
			delete(c.forgotten, k)
		}
	}
}

// Len reports how many answers are held, for tests.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
