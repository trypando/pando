package buildkit

import (
	"context"
	"runtime"
	"sort"
	"sync"
	"time"

	bkclient "github.com/moby/buildkit/client"
)

// One BuildKit per app (R-117).
//
// Builds of different apps never share a daemon: a step shares its daemon's
// process namespace (rootless BuildKit cannot make another), its cache mounts
// and its network, so a daemon shared by two apps let one app's build signal
// the other's steps and read its cache mounts. Each app's builds get a daemon
// of their own, started when the first one needs it — a compose app's
// services share their app's — kept warm for idleWindow after the last one
// finishes, then removed with everything made for it.
//
// No new limit is set on how many run: builds are already bounded by the
// deploy queue (host policy's max_concurrent_deploys), and daemons — running
// and warm together — by the same number. Starting one that would pass it
// first removes the least recently used idle one. One with a build running is
// never removed for room.

// defaultIdleWindow is how long a daemon stays after its app's last build
// finishes. [P] Long enough that a deploy right after detection, or a fix and
// a redeploy, finds it warm; short enough that a few hundred apps deployed in
// a day leave nothing running. Its cold start is measured in design 03 §3.
const defaultIdleWindow = 10 * time.Minute

// defaultDaemonLimit is the bound when nothing has said what host policy's is:
// deploy.DefaultConcurrency's formula, one per CPU and at least two.
func defaultDaemonLimit() int {
	if n := runtime.NumCPU(); n > 2 {
		return n
	}
	return 2
}

// daemon is one app's BuildKit.
type daemon struct {
	app string

	// Filled in once ready is closed: the daemon, or why it could not start.
	inst *instance
	err  error

	ready chan struct{}

	users    int       // builds using it now
	lastUsed time.Time // when the last of them finished
	timer    stopper   // its idle window, while it has no users
	forget   bool      // the app was deleted while it was building
}

// instance is a running daemon as the launcher made it.
type instance struct {
	cli *bkclient.Client

	// container and network are what remove removes. Empty in tests.
	container, network string
}

// stopper is what time.AfterFunc returns, as the pool uses it.
type stopper interface{ Stop() bool }

// launcher starts and removes daemons. managed is the real one.
type launcher interface {
	launch(ctx context.Context, app string) (*instance, error)
	remove(ctx context.Context, inst *instance)
}

// daemons is this replica's BuildKits, one per app being built or recently
// built.
type daemons struct {
	launcher launcher

	// limit is how many daemons may run and wait warm together: host
	// policy's max_concurrent_deploys, read the way the deploy queue reads
	// it. Nil is defaultDaemonLimit.
	limit func(context.Context) int

	idle      time.Duration
	now       func() time.Time
	afterFunc func(time.Duration, func()) stopper

	// base is what removals run under: an idle window ends, or a build lets
	// a deleted app's daemon go, outside any request.
	base context.Context

	mu    sync.Mutex
	byApp map[string]*daemon
}

func newDaemons(l launcher) *daemons {
	return &daemons{
		launcher:  l,
		idle:      defaultIdleWindow,
		now:       time.Now,
		afterFunc: func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) },
		byApp:     map[string]*daemon{},
		base:      context.Background(),
	}
}

func (p *daemons) bound(ctx context.Context) int {
	if p.limit == nil {
		return defaultDaemonLimit()
	}
	if n := p.limit(ctx); n > 0 {
		return n
	}
	return defaultDaemonLimit()
}

// acquire returns the app's daemon, starting it if there is none, and counts
// the caller as using it until release.
func (p *daemons) acquire(ctx context.Context, app string) (*daemon, error) {
	limit := p.bound(ctx) // read before the lock: it may ask the database

	p.mu.Lock()
	if d, ok := p.byApp[app]; ok {
		d.users++
		if d.timer != nil {
			d.timer.Stop()
			d.timer = nil
		}
		p.mu.Unlock()
		return p.await(ctx, d)
	}
	victims := p.evictFor(limit)
	d := &daemon{app: app, users: 1, ready: make(chan struct{})}
	p.byApp[app] = d
	p.mu.Unlock()

	for _, v := range victims {
		p.launcher.remove(context.WithoutCancel(ctx), v.inst)
	}
	inst, err := p.launcher.launch(ctx, app)
	p.mu.Lock()
	d.inst, d.err = inst, err
	if err != nil && p.byApp[app] == d {
		// Not kept: the next build tries again.
		delete(p.byApp, app)
	}
	p.mu.Unlock()
	close(d.ready)
	if err != nil {
		p.release(d)
		return nil, err
	}
	return d, nil
}

// await waits for a daemon another build is starting.
func (p *daemons) await(ctx context.Context, d *daemon) (*daemon, error) {
	select {
	case <-d.ready:
	case <-ctx.Done():
		p.release(d)
		return nil, ctx.Err()
	}
	if d.err != nil {
		p.release(d)
		return nil, d.err
	}
	return d, nil
}

// evictFor removes from the pool the least recently used idle daemons, as
// many as starting one more takes to stay within limit, and returns them for
// the caller to remove outside the lock. Never one with a build running: when
// every daemon is busy, the new one starts anyway, since the builds are what
// the deploy queue bounds.
func (p *daemons) evictFor(limit int) []*daemon {
	over := len(p.byApp) + 1 - limit
	if over <= 0 {
		return nil
	}
	var idle []*daemon
	for _, d := range p.byApp {
		if d.users == 0 && d.inst != nil {
			idle = append(idle, d)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].lastUsed.Before(idle[j].lastUsed) })
	if len(idle) > over {
		idle = idle[:over]
	}
	for _, d := range idle {
		p.drop(d)
	}
	return idle
}

// drop takes d out of the pool. Called with the lock held.
func (p *daemons) drop(d *daemon) {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	delete(p.byApp, d.app)
}

// release ends one build's use of d. The last one out starts its idle window,
// or removes it at once if its app was deleted meanwhile.
func (p *daemons) release(d *daemon) {
	p.mu.Lock()
	d.users--
	d.lastUsed = p.now()
	if d.users > 0 || p.byApp[d.app] != d {
		p.mu.Unlock()
		return
	}
	if d.forget {
		p.drop(d)
		p.mu.Unlock()
		p.launcher.remove(p.base, d.inst)
		return
	}
	d.timer = p.afterFunc(p.idle, func() { p.expire(d) })
	p.mu.Unlock()
}

// expire removes d at the end of its idle window, unless a build took it up
// again first.
func (p *daemons) expire(d *daemon) {
	p.mu.Lock()
	if p.byApp[d.app] != d || d.users > 0 {
		p.mu.Unlock()
		return
	}
	p.drop(d)
	p.mu.Unlock()
	p.launcher.remove(p.base, d.inst)
}

// forgetApp removes a deleted app's daemon: now if it is idle, or when its
// build finishes.
func (p *daemons) forgetApp(ctx context.Context, app string) {
	p.mu.Lock()
	d, ok := p.byApp[app]
	if !ok {
		p.mu.Unlock()
		return
	}
	if d.users > 0 {
		d.forget = true
		p.mu.Unlock()
		return
	}
	p.drop(d)
	p.mu.Unlock()
	p.launcher.remove(context.WithoutCancel(ctx), d.inst)
}

// count is how many daemons the pool holds, running and warm.
func (p *daemons) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byApp)
}
