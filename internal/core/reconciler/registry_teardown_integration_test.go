//go:build integration

package reconciler_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// refusingRegistryImages is a registry that refuses every delete, as one
// whose token lacks delete permission does, and counts the attempts.
type refusingRegistryImages struct {
	mu       sync.Mutex
	attempts map[string]int
}

func (r *refusingRegistryImages) DeleteApp(_ context.Context, appID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts[appID]++
	return errs.New(errs.AdapterFailed, "The registry ghcr.io refused to delete a deleted app's image.").
		WithRemedy("Give the token delete:packages.")
}

// TestR224_ARegistryThatRefusesDeletesDoesNotHoldTheTeardown asserts that a
// registry cleanup that always fails no longer holds teardown: more deleted
// apps than one pass takes are all torn down, each registry cleanup is tried
// once, and each failure is on the teardown's audit event with what to do.
func TestR224_ARegistryThatRefusesDeletesDoesNotHoldTheTeardown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)

	var deleted []string
	for range reconciler.TeardownBatch + 5 {
		app, err := apps.Create(ctx, "gone-"+id.New(id.App)[4:14], id.New(id.App), owner, owner,
			spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
		require.NoError(t, err)
		require.NoError(t, apps.Archive(ctx, app.ID))
		deleted = append(deleted, app.ID)
	}

	images := &refusingRegistryImages{attempts: map[string]int{}}
	auditor := &recordingAuditor{}
	gc := &reconciler.GC{
		Apps: apps, Registry: noAdapters{}, Logger: zap.NewNop(), Auditor: auditor,
		RegistryImages: images,
	}
	gc.TearDownDeleted(ctx)
	gc.TearDownDeleted(ctx)
	gc.TearDownDeleted(ctx)

	for _, appID := range deleted {
		require.False(t, awaitingTeardown(t, apps, appID), appID+" is torn down")
		require.Equal(t, 1, images.attempts[appID], "the registry is tried once, not every pass")
	}
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	failures := 0
	for _, e := range auditor.events {
		if e.Action == "app.bundle.destroy" && e.Detail["registry_images_left"] == true {
			failures++
			require.Contains(t, e.Detail["registry_error"], "delete:packages")
		}
	}
	require.Equal(t, len(deleted), failures)
}
