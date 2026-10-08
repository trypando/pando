package capacity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/planner"
)

// sampled is a runtime that counts how often its usage is sampled.
type sampled struct {
	api.RuntimeAdapter
	usage   bool
	down    bool
	inUse   atomic.Int32
	reports atomic.Int32
}

func (s *sampled) Kind() string           { return "fake" }
func (s *sampled) Category() api.Category { return api.CategoryRuntime }
func (s *sampled) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{ReportsUsage: s.usage}, nil
}

func (s *sampled) Capacity(context.Context) (api.Capacity, error) {
	if s.down {
		return api.Capacity{}, errors.New("not answering")
	}
	n := s.reports.Add(1)
	return api.Capacity{TotalCPUMillis: 8000, TotalMemoryBytes: 16 << 30, RunningWorkloads: int(n)}, nil
}

func (s *sampled) InUse(context.Context) (api.InUse, error) {
	n := s.inUse.Add(1)
	return api.InUse{CPUMillis: int(n) * 100}, nil
}

// committed answers what is committed now, and can be changed between views.
type committed struct {
	mu    sync.Mutex
	cpu   int
	err   error
	calls int
}

func (c *committed) AllocatedOn(_ context.Context, ref, exclude string) (planner.Allocation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if exclude != "" {
		panic("the screen excludes no app")
	}
	return planner.Allocation{CPUMillis: c.cpu}, c.err
}

type fixture struct {
	snaps      *Snapshots
	clk        *clock.Fake
	live, down *sampled
	alloc      *committed
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	reg := api.NewRegistry()
	live, quiet, down := &sampled{usage: true}, &sampled{}, &sampled{down: true}
	require.NoError(t, reg.Register("rt_live", live))
	require.NoError(t, reg.Register("rt_quiet", quiet))
	require.NoError(t, reg.Register("rt_down", down))
	clk := clock.NewFake(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	alloc := &committed{cpu: 500}
	return fixture{
		snaps: &Snapshots{Registry: reg, Allocations: alloc, Clock: clk, Interval: 30 * time.Second, Idle: 5 * time.Minute},
		clk:   clk, live: live, down: down, alloc: alloc,
	}
}

func byRef(v View) map[string]Runtime {
	out := map[string]Runtime{}
	for _, r := range v.Runtimes {
		out[r.AdapterRef] = r
	}
	return out
}

// TestR245_CapacityIsServedFromABackgroundReadingAndSaysWhenItWasTaken
// asserts GET /capacity's reading after issue #72: the runtimes are sampled
// once and the sample shared by every view until the next refresh, each view
// says when its sample was taken, and what is committed is read on every view
// rather than from the sample (R-242) — so it agrees with a refused deploy.
func TestR245_CapacityIsServedFromABackgroundReadingAndSaysWhenItWasTaken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	taken := f.clk.Now()

	first, err := f.snaps.View(ctx)
	require.NoError(t, err)
	require.Equal(t, taken, first.TakenAt)
	require.Equal(t, 30*time.Second, first.Interval)
	runtimes := byRef(first)
	require.Len(t, runtimes, 3)
	require.True(t, runtimes["rt_live"].Reachable)
	require.Equal(t, 100, runtimes["rt_live"].InUse.CPUMillis)
	require.Nil(t, runtimes["rt_quiet"].InUse, "a runtime that does not report usage has none")
	require.False(t, runtimes["rt_down"].Reachable)
	require.Zero(t, runtimes["rt_down"].Allocated, "nothing is read for a runtime that did not answer")
	require.Equal(t, 500, runtimes["rt_live"].Allocated.CPUMillis)

	// Many views later, still one sample; the commitment is read each time.
	f.alloc.mu.Lock()
	f.alloc.cpu = 1500
	f.alloc.mu.Unlock()
	for range 10 {
		f.clk.Advance(time.Second)
		v, err := f.snaps.View(ctx)
		require.NoError(t, err)
		require.Equal(t, taken, v.TakenAt, "served from the reading")
		require.Equal(t, 1500, byRef(v)["rt_live"].Allocated.CPUMillis, "committed is as of the request")
	}
	require.EqualValues(t, 1, f.live.inUse.Load(), "sampled once for eleven views")

	// The background loop refreshes while somebody is looking.
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { f.snaps.Run(runCtx); close(done) }()
	require.Eventually(t, func() bool {
		f.clk.Advance(30 * time.Second)
		return f.live.inUse.Load() >= 2
	}, 5*time.Second, 10*time.Millisecond)
	v, err := f.snaps.View(ctx)
	require.NoError(t, err)
	require.True(t, v.TakenAt.After(taken), "a newer reading")

	// Nobody has looked for longer than Idle: the loop stops sampling.
	f.clk.Advance(10 * time.Minute)
	time.Sleep(50 * time.Millisecond) // a reading already under way finishes
	before := f.live.inUse.Load()
	for range 5 {
		f.clk.Advance(30 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, before, f.live.inUse.Load(), "no reading for a screen nobody has open")
	stop()
	<-done

	// And the next view, finding a reading that old, waits for a new one.
	f.clk.Advance(10 * time.Minute)
	v, err = f.snaps.View(ctx)
	require.NoError(t, err)
	require.Equal(t, f.clk.Now(), v.TakenAt)
}

// A view whose commitment cannot be read fails, as GET /capacity did before:
// a screen that showed nothing committed would say there is room there is not.
func TestACapacityViewFailsWhenTheCommitmentCannotBeRead(t *testing.T) {
	f := newFixture(t)
	f.alloc.err = errors.New("database gone")
	_, err := f.snaps.View(context.Background())
	require.Error(t, err)

	// Defaults when unset.
	s := &Snapshots{}
	require.Equal(t, DefaultInterval, s.interval())
	require.Equal(t, DefaultIdle, s.idle())
	require.NotNil(t, s.logger())
	require.False(t, s.now().IsZero())
}

// A request that gives up does not take the shared reading with it.
func TestACanceledViewLeavesTheReadingToTheOthers(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.snaps.View(ctx)
	if err != nil {
		require.ErrorIs(t, err, context.Canceled)
	}
	v, err := f.snaps.View(context.Background())
	require.NoError(t, err)
	require.Len(t, v.Runtimes, 3)
}
