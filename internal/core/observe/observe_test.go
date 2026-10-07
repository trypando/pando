package observe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
)

// fakeRuntime counts Observe and Usage calls. Only those two are implemented;
// anything else panics on the nil embedded interface.
type fakeRuntime struct {
	api.RuntimeAdapter

	observes atomic.Int32
	usages   atomic.Int32

	mu      sync.Mutex
	gate    chan struct{} // when set, Observe waits on it
	entered chan struct{} // when set, Observe signals on entry
	err     error
	lastCtx context.Context
}

func (f *fakeRuntime) Observe(ctx context.Context, ref api.BundleRef) (api.ObservedBundle, error) {
	n := f.observes.Add(1)
	f.mu.Lock()
	gate, entered, err := f.gate, f.entered, f.err
	f.lastCtx = ctx
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return api.ObservedBundle{}, ctx.Err()
		}
	}
	if err != nil {
		return api.ObservedBundle{}, err
	}
	return api.ObservedBundle{Workloads: []api.ObservedWorkload{{Name: ref.BundleID, RestartCount: int(n)}}}, nil
}

func (f *fakeRuntime) Usage(_ context.Context, ref api.BundleRef) (api.BundleUsage, error) {
	n := f.usages.Add(1)
	return api.BundleUsage{Workloads: []api.WorkloadUsage{{Workload: ref.BundleID, CPUMillis: int(n)}}}, nil
}

func newCache() (*Cache, *clock.Fake) {
	clk := clock.NewFake(time.Time{})
	return New(WithClock(clk)), clk
}

func TestConcurrentObservesWithinTTLShareOneCall(t *testing.T) {
	c, _ := newCache()
	rt := &fakeRuntime{gate: make(chan struct{})}

	const callers = 64
	var wg sync.WaitGroup
	results := make([]api.ObservedBundle, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = c.Observe(context.Background(), "docker", rt, "app_1")
		}()
	}
	// Whether a caller joins the flight or arrives after it and reads the
	// stored answer, the clock has not moved, so the runtime is asked once.
	close(rt.gate)
	wg.Wait()

	if got := rt.observes.Load(); got != 1 {
		t.Fatalf("runtime Observe called %d times; want 1", got)
	}
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i].Workloads[0].Name != "app_1" {
			t.Fatalf("caller %d got %+v", i, results[i])
		}
	}
}

func TestObservationIsAskedAgainAfterTTL(t *testing.T) {
	c, clk := newCache()
	rt := &fakeRuntime{}
	ctx := context.Background()

	for range 3 {
		if _, err := c.Observe(ctx, "docker", rt, "app_1"); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(DefaultObserveTTL - time.Millisecond)
	_, _ = c.Observe(ctx, "docker", rt, "app_1")
	if got := rt.observes.Load(); got != 1 {
		t.Fatalf("within TTL: %d calls; want 1", got)
	}
	clk.Advance(time.Millisecond)
	got, _ := c.Observe(ctx, "docker", rt, "app_1")
	if n := rt.observes.Load(); n != 2 {
		t.Fatalf("after TTL: %d calls; want 2", n)
	}
	if got.Workloads[0].RestartCount != 2 {
		t.Fatalf("served the old answer after TTL: %+v", got)
	}
}

func TestUsageIsSharedForItsOwnLongerTTL(t *testing.T) {
	c, clk := newCache()
	rt := &fakeRuntime{}
	ctx := context.Background()

	_, _ = c.Usage(ctx, "docker", rt, "app_1")
	clk.Advance(DefaultObserveTTL + time.Second) // past the status TTL, inside usage's
	_, _ = c.Usage(ctx, "docker", rt, "app_1")
	if n := rt.usages.Load(); n != 1 {
		t.Fatalf("usage within its TTL: %d calls; want 1", n)
	}
	// A status poll is a different answer and does not reuse the reading.
	_, _ = c.Observe(ctx, "docker", rt, "app_1")
	if n := rt.observes.Load(); n != 1 {
		t.Fatalf("observe: %d calls; want 1", n)
	}
	clk.Advance(DefaultUsageTTL)
	_, _ = c.Usage(ctx, "docker", rt, "app_1")
	if n := rt.usages.Load(); n != 2 {
		t.Fatalf("usage after its TTL: %d calls; want 2", n)
	}
}

func TestKeyIncludesAppAndRuntime(t *testing.T) {
	c, _ := newCache()
	rt := &fakeRuntime{}
	ctx := context.Background()
	_, _ = c.Observe(ctx, "docker", rt, "app_1")
	_, _ = c.Observe(ctx, "docker", rt, "app_2")
	_, _ = c.Observe(ctx, "other", rt, "app_1")
	if n := rt.observes.Load(); n != 3 {
		t.Fatalf("%d calls; want 3, one per (runtime, app)", n)
	}
}

func TestFailedObservationIsNotCached(t *testing.T) {
	c, _ := newCache()
	rt := &fakeRuntime{err: errors.New("docker is not answering")}
	ctx := context.Background()

	if _, err := c.Observe(ctx, "docker", rt, "app_1"); err == nil {
		t.Fatal("want the runtime's error")
	}
	rt.mu.Lock()
	rt.err = nil
	rt.mu.Unlock()
	got, err := c.Observe(ctx, "docker", rt, "app_1")
	if err != nil {
		t.Fatalf("failure was cached: %v", err)
	}
	if n := rt.observes.Load(); n != 2 || got.Workloads[0].Name != "app_1" {
		t.Fatalf("%d calls, %+v; want a second call that succeeds", n, got)
	}
}

func TestOneCallerGoingAwayDoesNotCancelTheSharedCall(t *testing.T) {
	c, _ := newCache()
	rt := &fakeRuntime{gate: make(chan struct{}), entered: make(chan struct{}, 1)}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := c.Observe(leaderCtx, "docker", rt, "app_1")
		leaderErr <- err
	}()
	<-rt.entered // the shared call is running, started by the leader

	other := make(chan error, 1)
	go func() {
		_, err := c.Observe(context.Background(), "docker", rt, "app_1")
		other <- err
	}()

	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: %v; want context.Canceled", err)
	}
	rt.mu.Lock()
	callCtx := rt.lastCtx
	rt.mu.Unlock()
	if callCtx.Err() != nil {
		t.Fatal("the shared call's context was canceled with the leader's")
	}
	close(rt.gate)
	if err := <-other; err != nil {
		t.Fatalf("the other waiter failed: %v", err)
	}
	if n := rt.observes.Load(); n != 1 {
		t.Fatalf("%d calls; want 1", n)
	}
}

func TestForgetMakesTheNextRequestAskAgain(t *testing.T) {
	c, _ := newCache()
	rt := &fakeRuntime{}
	ctx := context.Background()

	_, _ = c.Observe(ctx, "docker", rt, "app_1")
	_, _ = c.Usage(ctx, "docker", rt, "app_1")
	_, _ = c.Observe(ctx, "docker", rt, "app_2")
	c.Forget("app_1")
	_, _ = c.Observe(ctx, "docker", rt, "app_1")
	_, _ = c.Usage(ctx, "docker", rt, "app_1")
	_, _ = c.Observe(ctx, "docker", rt, "app_2")
	if o, u := rt.observes.Load(), rt.usages.Load(); o != 3 || u != 2 {
		t.Fatalf("observes %d usages %d; want 3 and 2 (app_2 still cached)", o, u)
	}
}

func TestAnAnswerAskedForBeforeForgetIsNotStored(t *testing.T) {
	c, clk := newCache()
	rt := &fakeRuntime{gate: make(chan struct{}), entered: make(chan struct{}, 1)}

	done := make(chan struct{})
	go func() {
		_, _ = c.Observe(context.Background(), "docker", rt, "app_1")
		close(done)
	}()
	<-rt.entered
	clk.Advance(time.Millisecond)
	c.Forget("app_1") // e.g. the app was stopped while the poll was in flight
	close(rt.gate)
	<-done

	rt.mu.Lock()
	rt.gate, rt.entered = nil, nil
	rt.mu.Unlock()
	_, _ = c.Observe(context.Background(), "docker", rt, "app_1")
	if n := rt.observes.Load(); n != 2 {
		t.Fatalf("%d calls; want 2 — the pre-Forget answer must not be reused", n)
	}
}

func TestExpiredEntriesAreSwept(t *testing.T) {
	c, clk := newCache()
	rt := &fakeRuntime{}
	ctx := context.Background()
	for _, id := range []string{"app_1", "app_2", "app_3"} {
		_, _ = c.Observe(ctx, "docker", rt, id)
	}
	if c.Len() != 3 {
		t.Fatalf("held %d; want 3", c.Len())
	}
	clk.Advance(sweepEvery + time.Second)
	_, _ = c.Observe(ctx, "docker", rt, "app_4")
	if c.Len() != 1 {
		t.Fatalf("held %d after a sweep; want only the fresh one", c.Len())
	}
}

func TestNilCacheAsksTheRuntimeEveryTime(t *testing.T) {
	var c *Cache
	rt := &fakeRuntime{}
	_, _ = c.Observe(context.Background(), "docker", rt, "app_1")
	_, _ = c.Observe(context.Background(), "docker", rt, "app_1")
	c.Forget("app_1")
	if n := rt.observes.Load(); n != 2 {
		t.Fatalf("%d calls; want 2", n)
	}
}
