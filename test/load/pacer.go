package main

import (
	"context"
	"math"
	"math/rand/v2"
	"time"
)

// Pacer releases requests at a fixed rate, open loop: the next request is due
// at its time whether or not the last one has answered. A closed loop — wait
// for an answer, then send — slows down when the server does, and so hides
// exactly the latency this harness exists to find.
type Pacer struct {
	Rate float64 // requests per second
	sent int64
}

// Due is how many requests should have been released after elapsed, minus
// the ones already released, and marks them released.
func (p *Pacer) Due(elapsed time.Duration) int {
	if p.Rate <= 0 || elapsed <= 0 {
		return 0
	}
	want := int64(math.Floor(elapsed.Seconds() * p.Rate))
	n := want - p.sent
	if n < 0 {
		return 0
	}
	p.sent = want
	return int(n)
}

// paced calls send at rate until ctx ends. A request that finds every worker
// busy is dropped and counted by dropped: the harness, not the server, ran
// out, and the report says so rather than quietly sending less.
func paced(ctx context.Context, rate float64, workers int, send func(context.Context), dropped func()) {
	if rate <= 0 {
		<-ctx.Done()
		return
	}
	jobs := make(chan struct{}, workers)
	for range workers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-jobs:
					send(ctx)
				}
			}
		}()
	}
	p := &Pacer{Rate: rate}
	start := time.Now()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			for range p.Due(time.Since(start)) {
				select {
				case jobs <- struct{}{}:
				default:
					dropped()
				}
			}
		}
	}
}

// jitter returns d scaled by a random factor in [1-f, 1+f], so a thousand
// pollers started together do not stay in lockstep.
func jitter(d time.Duration, f float64) time.Duration {
	return time.Duration(float64(d) * (1 - f + 2*f*rand.Float64()))
}

// every calls fn each interval until ctx ends, starting after a random part
// of one interval — the way a browser's polls are spread by when each tab
// was opened.
func every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	first := time.NewTimer(time.Duration(rand.Int64N(int64(interval))))
	select {
	case <-ctx.Done():
		first.Stop()
		return
	case <-first.C:
	}
	fn(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}
