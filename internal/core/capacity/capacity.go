// Package capacity is what GET /capacity shows: each runtime's room, beside
// what Pando has committed to apps on it (R-242, R-243, R-245).
//
// The runtime's half is a reading taken in the background and shared by every
// request (issue #72). Sampling what is in use means reading stats for every
// running container — about a second, and work in proportion to how many apps
// there are — and doing it on every view meant an install with twenty
// thousand apps sampled twenty thousand containers each time somebody opened
// the screen. So a reading is taken every Interval while somebody is looking,
// and each response says when its reading was taken.
//
// The committed half is not part of the reading. It is read on every request
// from Allocations, the same sum the planner's capacity check reads at plan
// time, which is cheap (migration 60) — so what this screen calls committed and
// what a refused deploy says agree to the moment, never to the last reading.
package capacity

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/planner"
)

// Defaults [P]. Thirty seconds is old enough to spare the runtimes and young
// enough that a screen left open shows a host filling up while it does. A
// screen nobody has asked for in five minutes stops being refreshed.
const (
	DefaultInterval = 30 * time.Second
	DefaultIdle     = 5 * time.Minute
)

// Reading is one runtime's answer, as of a snapshot.
type Reading struct {
	AdapterRef string

	// Reachable is false when the runtime did not answer; nothing else in
	// the reading is then known.
	Reachable bool
	Capacity  api.Capacity

	// InUse is present only from a runtime that reports usage (R-245) and
	// answered for it.
	InUse *api.InUse
}

// Snapshot is every runtime's reading, taken together.
type Snapshot struct {
	TakenAt  time.Time
	Readings []Reading
}

// Runtime is one runtime's entry in a view: its reading, and what is
// committed on it now.
type Runtime struct {
	Reading
	Allocated planner.Allocation
}

// View is what GET /capacity shows.
type View struct {
	// TakenAt is when the runtimes were read, and Interval how often they
	// are read again while somebody is looking. Allocated is as of the
	// request.
	TakenAt  time.Time
	Interval time.Duration
	Runtimes []Runtime
}

// Snapshots keeps the latest reading of every runtime. The zero value is not
// usable: Registry and Allocations are required.
type Snapshots struct {
	Registry    *api.Registry
	Allocations planner.Allocations
	Clock       clock.Clock
	Logger      *zap.Logger

	// Interval is how often a reading is taken while somebody is looking,
	// and Idle how long after the last request the readings stop. Defaults
	// when zero.
	Interval time.Duration
	Idle     time.Duration

	group     singleflight.Group
	mu        sync.Mutex
	snap      *Snapshot
	lastAsked time.Time
}

// View returns the latest reading with what is committed now.
//
// A request that finds no reading, or one older than Idle — nobody has looked
// for a while, so nothing kept it fresh — waits for a new one, shared with any
// other request waiting at the same moment.
func (s *Snapshots) View(ctx context.Context) (View, error) {
	now := s.now()
	s.mu.Lock()
	s.lastAsked = now
	snap := s.snap
	s.mu.Unlock()

	if snap == nil || now.Sub(snap.TakenAt) > s.idle() {
		fresh, err := s.refresh(ctx)
		if err != nil {
			return View{}, err
		}
		snap = fresh
	}

	view := View{TakenAt: snap.TakenAt, Interval: s.interval(), Runtimes: make([]Runtime, 0, len(snap.Readings))}
	for _, r := range snap.Readings {
		entry := Runtime{Reading: r}
		if r.Reachable {
			allocated, err := s.Allocations.AllocatedOn(ctx, r.AdapterRef, "")
			if err != nil {
				return View{}, err
			}
			entry.Allocated = allocated
		}
		view.Runtimes = append(view.Runtimes, entry)
	}
	return view, nil
}

// Run takes a reading every Interval while somebody has asked within Idle,
// until ctx ends.
func (s *Snapshots) Run(ctx context.Context) {
	c := s.clock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.After(s.interval()):
		}
		s.mu.Lock()
		wanted := !s.lastAsked.IsZero() && s.now().Sub(s.lastAsked) <= s.idle()
		s.mu.Unlock()
		if !wanted {
			continue
		}
		if _, err := s.refresh(ctx); err != nil && ctx.Err() == nil {
			s.logger().Warn("could not read the runtimes' capacity", zap.Error(err))
		}
	}
}

// refresh reads every runtime, in parallel, and keeps the result. Callers at
// the same moment share one reading.
func (s *Snapshots) refresh(ctx context.Context) (*Snapshot, error) {
	ch := s.group.DoChan("refresh", func() (any, error) {
		// Detached from the request that started it: the others waiting on
		// it, and the snapshot, should not fail because that one went away.
		snap := s.read(context.WithoutCancel(ctx))
		s.mu.Lock()
		s.snap = snap
		s.mu.Unlock()
		return snap, nil
	})
	select {
	case res := <-ch:
		return res.Val.(*Snapshot), res.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// read asks every runtime what it has, and what is in use where it says. A
// runtime that does not answer is a reading saying so, not a failed snapshot.
func (s *Snapshots) read(ctx context.Context) *Snapshot {
	refs := s.Registry.ByCategory(api.CategoryRuntime)
	readings := make([]Reading, len(refs))
	present := make([]bool, len(refs))
	var wg sync.WaitGroup
	for i, ref := range refs {
		rt, ok := s.Registry.Runtime(ref)
		if !ok {
			continue
		}
		present[i] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			readings[i] = readOne(ctx, ref, rt)
		}()
	}
	wg.Wait()

	out := &Snapshot{TakenAt: s.now(), Readings: make([]Reading, 0, len(refs))}
	for i := range refs {
		if present[i] {
			out.Readings = append(out.Readings, readings[i])
		}
	}
	return out
}

func readOne(ctx context.Context, ref string, rt api.RuntimeAdapter) Reading {
	r := Reading{AdapterRef: ref}
	capacity, err := rt.Capacity(ctx)
	if err != nil {
		return r
	}
	r.Reachable, r.Capacity = true, capacity
	if caps, err := rt.Capabilities(ctx); err == nil && caps.ReportsUsage {
		if inUse, err := rt.InUse(ctx); err == nil {
			r.InUse = &inUse
		}
	}
	return r
}

func (s *Snapshots) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return DefaultInterval
}

func (s *Snapshots) idle() time.Duration {
	if s.Idle > 0 {
		return s.Idle
	}
	return DefaultIdle
}

func (s *Snapshots) clock() clock.Clock {
	if s.Clock == nil {
		return clock.System{}
	}
	return s.Clock
}

func (s *Snapshots) now() time.Time { return s.clock().Now().UTC() }

func (s *Snapshots) logger() *zap.Logger {
	if s.Logger == nil {
		return zap.NewNop()
	}
	return s.Logger
}
