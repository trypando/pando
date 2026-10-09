// Package work runs work held in Postgres on a bounded number of goroutines
// (issue #72, O-32).
//
// The work itself — a deploy, a detection — is a row. A replica claims rows
// with FOR UPDATE SKIP LOCKED when it has room and runs each on a goroutine of
// its own, never more at once than its limit; every replica does the same, so
// the install's throughput grows with its replicas and none of them takes on
// more than it can run. A row nobody has claimed waits for the first replica
// with room, which is what lets queued work survive a restart.
//
// This is not scheduling (R-010): nothing here decides where an app runs. It
// decides which of Pando's own processes does a piece of Pando's own work.
package work

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Pool claims and runs items of type T.
type Pool[T any] struct {
	// Name is what the pool is called in log lines.
	Name string

	// Limit is the most items this replica runs at once. At least one.
	Limit int

	// LimitFunc, when set, is read instead of Limit every time the pool looks
	// for work, so the limit can change while the pool runs (issue #93). A
	// lower limit takes effect as running items finish: nothing is stopped.
	LimitFunc func(ctx context.Context) int

	// Poll is how often the pool looks for work when it was not told there is
	// some (Kick). Work queued on another replica is found this way. Two
	// seconds when zero.
	Poll time.Duration

	// Claim takes up to n items for this replica.
	Claim func(ctx context.Context, n int) ([]T, error)

	// Run runs one item to completion. Its context ends when the pool stops,
	// and Run must return soon after: a deploy's adapters honor cancellation.
	Run func(ctx context.Context, item T)

	// Release puts back items whose Run was still going when the pool
	// stopped, so another replica takes them at once rather than after this
	// one is taken for dead. An item that finished in the meantime must be
	// left alone; the stores' Release only touches work still in flight.
	Release func(ctx context.Context, items []T)

	// Drain is how long a stopping pool waits for its items to return before
	// releasing them anyway. Ten seconds when zero.
	Drain time.Duration

	Logger *zap.Logger

	once    sync.Once
	kick    chan struct{}
	mu      sync.Mutex
	running map[int]T
	next    int

	// stopped is what returned from Run after the pool's context ended: not
	// finished, as far as the pool can tell, and so released.
	stopped []T
}

func (p *Pool[T]) init() {
	p.once.Do(func() {
		p.kick = make(chan struct{}, 1)
		p.running = map[int]T{}
	})
}

// Kick tells the pool there may be work, so it claims now rather than at its
// next poll. Never blocks.
func (p *Pool[T]) Kick() {
	p.init()
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Running is how many items this replica is running now.
func (p *Pool[T]) Running() int {
	p.init()
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.running)
}

func (p *Pool[T]) limit(ctx context.Context) int {
	n := p.Limit
	if p.LimitFunc != nil {
		n = p.LimitFunc(ctx)
	}
	if n < 1 {
		return 1
	}
	return n
}

func (p *Pool[T]) logger() *zap.Logger {
	if p.Logger == nil {
		return zap.NewNop()
	}
	return p.Logger
}

// Serve claims and runs work until ctx ends, then stops what is running,
// waits up to Drain for it, and releases what did not finish.
func (p *Pool[T]) Serve(ctx context.Context) {
	p.init()
	poll := p.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	done := make(chan struct{}, p.limit(ctx))
	var wg sync.WaitGroup

	for {
		p.fill(ctx, &wg, done)

		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			p.stop(ctx, &wg)
			return
		case <-p.kick:
		case <-done:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// fill claims as many items as there is room for and starts each.
func (p *Pool[T]) fill(ctx context.Context, wg *sync.WaitGroup, done chan<- struct{}) {
	for ctx.Err() == nil {
		free := p.limit(ctx) - p.Running()
		if free <= 0 {
			return
		}
		items, err := p.Claim(ctx, free)
		if err != nil {
			if ctx.Err() == nil {
				p.logger().Warn("could not take work from the queue", zap.String("queue", p.Name), zap.Error(err))
			}
			return
		}
		if len(items) == 0 {
			return
		}
		for _, item := range items {
			p.mu.Lock()
			key := p.next
			p.next++
			p.running[key] = item
			p.mu.Unlock()

			wg.Add(1)
			go func(key int, item T) {
				defer wg.Done()
				defer func() {
					p.mu.Lock()
					delete(p.running, key)
					if ctx.Err() != nil {
						p.stopped = append(p.stopped, item)
					}
					p.mu.Unlock()
					select {
					case done <- struct{}{}:
					default:
					}
				}()
				p.Run(ctx, item)
			}(key, item)
		}
		if len(items) < free {
			return
		}
	}
}

// stop waits for running items, then releases every item that was running
// when the pool was told to stop: those whose Run returned after that, and
// those still running when the wait ran out.
//
// A Run that returned because its context ended did not finish its item, and
// the stores' Release leaves alone an item that finished anyway.
func (p *Pool[T]) stop(ctx context.Context, wg *sync.WaitGroup) {
	drain := p.Drain
	if drain <= 0 {
		drain = 10 * time.Second
	}
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(drain):
	}

	p.mu.Lock()
	items := append([]T(nil), p.stopped...)
	for _, item := range p.running {
		items = append(items, item)
	}
	p.stopped = nil
	p.mu.Unlock()
	if p.Release != nil && len(items) > 0 {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		p.Release(ctx, items)
	}
}
