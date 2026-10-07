package work

import (
	"context"
	"sync"
)

// Each calls fn for every item, at most n at a time, and returns when all have
// returned. For a leader job's pass over many apps — rolling backups, the
// auto-deploy poll — where one slow app must not hold up the rest and every
// app at once would swamp the host (issue #72).
//
// An item not yet started when ctx ends is skipped.
func Each[T any](ctx context.Context, n int, items []T, fn func(ctx context.Context, item T)) {
	if n < 1 {
		n = 1
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, item := range items {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(item T) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(ctx, item)
		}(item)
	}
	wg.Wait()
}
