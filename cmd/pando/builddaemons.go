package main

import (
	"context"
	"sync/atomic"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	buildkitadapter "github.com/trypando/pando/internal/adapter/builder/buildkit"
	"github.com/trypando/pando/internal/core/deploy"
)

// limitBuildDaemons bounds the BuildKits Pando runs, one per app, by host
// policy's max_concurrent_deploys, read the way the deploy queue reads it:
// zero for the default, and on a failed read the last limit read (R-117).
func limitBuildDaemons(registry *adapterapi.Registry, read func(context.Context) (int, error)) {
	limit := daemonLimit(read)
	for _, ref := range registry.ByCategory(adapterapi.CategoryBuilder) {
		a, _ := registry.Get(ref)
		if bk, ok := a.(*buildkitadapter.Adapter); ok {
			bk.LimitDaemons(limit)
		}
	}
}

// daemonLimit is read as a limit that is safe to ask from builds starting at
// once.
func daemonLimit(read func(context.Context) (int, error)) func(context.Context) int {
	var last atomic.Int64
	last.Store(int64(deploy.DefaultConcurrency()))
	return func(ctx context.Context) int {
		n, err := read(ctx)
		switch {
		case err != nil:
			return int(last.Load())
		case n <= 0:
			n = deploy.DefaultConcurrency()
		}
		last.Store(int64(n))
		return n
	}
}
