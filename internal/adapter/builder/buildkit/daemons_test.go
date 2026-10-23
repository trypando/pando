package buildkit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testWait = 5 * time.Second
	testTick = 10 * time.Millisecond
)

// fakeLauncher starts daemons that are only names, and records what it
// started and removed.
type fakeLauncher struct {
	mu       sync.Mutex
	launched []string
	removed  []string
	fail     error
	gate     chan struct{} // when set, launch waits for it
}

func (l *fakeLauncher) launch(ctx context.Context, app string) (*instance, error) {
	if l.gate != nil {
		select {
		case <-l.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return nil, l.fail
	}
	l.launched = append(l.launched, app)
	return &instance{container: app}, nil
}

func (l *fakeLauncher) remove(_ context.Context, inst *instance) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if inst != nil {
		l.removed = append(l.removed, inst.container)
	}
}

func (l *fakeLauncher) state() (launched, removed []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.launched...), append([]string(nil), l.removed...)
}

// manualTimers runs idle windows when the test says, not when time passes.
type manualTimers struct {
	mu    sync.Mutex
	fires []*manualTimer
}

type manualTimer struct {
	f       func()
	stopped bool
}

func (t *manualTimer) Stop() bool { t.stopped = true; return true }

func (m *manualTimers) afterFunc(_ time.Duration, f func()) stopper {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &manualTimer{f: f}
	m.fires = append(m.fires, t)
	return t
}

// fire ends every idle window still open.
func (m *manualTimers) fire() {
	m.mu.Lock()
	fires := m.fires
	m.fires = nil
	m.mu.Unlock()
	for _, t := range fires {
		if !t.stopped {
			t.f()
		}
	}
}

// testPool is a pool on a fake launcher, its limit n, with a clock the test
// moves and idle windows it ends.
func testPool(n int) (*daemons, *fakeLauncher, *manualTimers, *time.Time) {
	l := &fakeLauncher{}
	timers := &manualTimers{}
	p := newDaemons(l)
	p.limit = func(context.Context) int { return n }
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	p.afterFunc = timers.afterFunc
	return p, l, timers, &now
}

// TestR117_DifferentAppsNeverShareADaemon asserts two apps' builds get two
// daemons, and two builds of one app — a compose app's services — share one.
func TestR117_DifferentAppsNeverShareADaemon(t *testing.T) {
	ctx := context.Background()
	p, l, _, _ := testPool(8)

	web, err := p.acquire(ctx, "app_1")
	require.NoError(t, err)
	worker, err := p.acquire(ctx, "app_1")
	require.NoError(t, err)
	other, err := p.acquire(ctx, "app_2")
	require.NoError(t, err)

	require.Same(t, web, worker, "a compose app's services share their app's daemon")
	require.NotSame(t, web, other)
	launched, _ := l.state()
	require.Equal(t, []string{"app_1", "app_2"}, launched)
}

// TestR117_AnIdleDaemonIsRemovedAfterItsWindow asserts a daemon outlives its
// last build by its idle window and no more — and that a build in that window
// takes it up again rather than starting another.
func TestR117_AnIdleDaemonIsRemovedAfterItsWindow(t *testing.T) {
	ctx := context.Background()
	p, l, timers, _ := testPool(8)

	d, err := p.acquire(ctx, "app_1")
	require.NoError(t, err)
	p.release(context.Background(), d)
	again, err := p.acquire(ctx, "app_1")
	require.NoError(t, err)
	require.Same(t, d, again, "warm: taken up again")
	timers.fire() // the first window was stopped when it was taken up
	require.Equal(t, 1, p.count())

	p.release(context.Background(), again)
	timers.fire()
	_, removed := l.state()
	require.Equal(t, []string{"app_1"}, removed)
	require.Zero(t, p.count(), "nothing is kept after the window")

	// The window ending after a build took the daemon up again removes
	// nothing.
	d, err = p.acquire(ctx, "app_2")
	require.NoError(t, err)
	p.release(context.Background(), d)
	p.mu.Lock()
	window := d.timer.(*manualTimer)
	p.mu.Unlock()
	_, err = p.acquire(ctx, "app_2")
	require.NoError(t, err)
	window.f()
	require.Equal(t, 1, p.count())
}

// TestR117_WarmDaemonsAreBoundedByTheDeployLimit asserts the bound on running
// and warm daemons together is the deploy limit: starting one past it removes
// the least recently used idle one first, and never one with a build running.
func TestR117_WarmDaemonsAreBoundedByTheDeployLimit(t *testing.T) {
	ctx := context.Background()
	p, l, _, now := testPool(3)

	a, _ := p.acquire(ctx, "app_a")
	b, _ := p.acquire(ctx, "app_b")
	c, _ := p.acquire(ctx, "app_c")
	*now = now.Add(time.Minute)
	p.release(context.Background(), b) // idle longest
	*now = now.Add(time.Minute)
	p.release(context.Background(), c)
	// a is still building.

	_, err := p.acquire(ctx, "app_d")
	require.NoError(t, err)
	_, removed := l.state()
	require.Equal(t, []string{"app_b"}, removed, "the least recently used idle one goes")
	require.Equal(t, 3, p.count())

	_, err = p.acquire(ctx, "app_e")
	require.NoError(t, err)
	_, removed = l.state()
	require.Equal(t, []string{"app_b", "app_c"}, removed)

	// Every daemon busy: the next starts anyway, since builds are what the
	// deploy queue bounds, and none is removed mid-build.
	_, err = p.acquire(ctx, "app_f")
	require.NoError(t, err)
	_, removed = l.state()
	require.Len(t, removed, 2)
	require.Equal(t, 4, p.count())
	require.Positive(t, a.users, "a's build was never interrupted")
}

func TestTheDaemonLimitFallsBackToTheDeployDefault(t *testing.T) {
	p := newDaemons(&fakeLauncher{})
	require.Equal(t, defaultDaemonLimit(), p.bound(context.Background()), "nothing said")
	p.limit = func(context.Context) int { return 0 }
	require.Equal(t, defaultDaemonLimit(), p.bound(context.Background()), "zero is the default, as for deploys")
	p.limit = func(context.Context) int { return 5 }
	require.Equal(t, 5, p.bound(context.Background()))
	require.GreaterOrEqual(t, defaultDaemonLimit(), 2)
}

// A daemon that could not start is not kept: the next build tries again, and
// a build that was waiting on it is told why.
func TestADaemonThatCouldNotStartIsNotKept(t *testing.T) {
	ctx := context.Background()
	p, l, _, _ := testPool(8)
	l.gate = make(chan struct{})
	l.fail = errors.New("no user namespaces")

	errsCh := make(chan error, 2)
	go func() { _, err := p.acquire(ctx, "app_1"); errsCh <- err }()
	require.Eventually(t, func() bool { return p.count() == 1 }, testWait, testTick)
	go func() { _, err := p.acquire(ctx, "app_1"); errsCh <- err }()
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		d := p.byApp["app_1"]
		return d != nil && d.users == 2
	}, testWait, testTick)
	close(l.gate)
	require.ErrorContains(t, <-errsCh, "no user namespaces")
	require.ErrorContains(t, <-errsCh, "no user namespaces")
	require.Zero(t, p.count())

	l.fail = nil
	_, err := p.acquire(ctx, "app_1")
	require.NoError(t, err)
}

// A build canceled while it waits for its app's daemon to start stops
// waiting; the daemon carries on for the build that started it.
func TestABuildCanceledWhileItsDaemonStartsStopsWaiting(t *testing.T) {
	p, l, _, _ := testPool(8)
	l.gate = make(chan struct{})
	started := make(chan error, 1)
	go func() { _, err := p.acquire(context.Background(), "app_1"); started <- err }()
	require.Eventually(t, func() bool { return p.count() == 1 }, testWait, testTick)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.acquire(ctx, "app_1")
	require.ErrorIs(t, err, context.Canceled)
	close(l.gate)
	require.NoError(t, <-started)
}

// TestR117_ADeletedAppsDaemonIsRemoved asserts a deleted app's daemon goes at
// once when idle, and when its build finishes when not.
func TestR117_ADeletedAppsDaemonIsRemoved(t *testing.T) {
	ctx := context.Background()
	p, l, _, _ := testPool(8)

	idle, _ := p.acquire(ctx, "app_1")
	p.release(context.Background(), idle)
	p.forgetApp(context.Background(), "app_1")
	_, removed := l.state()
	require.Equal(t, []string{"app_1"}, removed)

	busy, _ := p.acquire(ctx, "app_2")
	p.forgetApp(context.Background(), "app_2")
	_, removed = l.state()
	require.Len(t, removed, 1, "not mid-build")
	p.release(context.Background(), busy)
	_, removed = l.state()
	require.Equal(t, []string{"app_1", "app_2"}, removed)
	require.Zero(t, p.count())

	p.forgetApp(context.Background(), "app_never_built")
}
