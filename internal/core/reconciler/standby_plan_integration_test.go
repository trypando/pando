//go:build integration

package reconciler_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// TestR419_AStandbyIsBuiltFromWhatTheReconcilerWouldStart asserts that the
// plan a standby is made and promoted from is the reconciler's own: the
// pinned spec's shape, with the pull credential and the environment once
// completed.
func TestR419_AStandbyIsBuiltFromWhatTheReconcilerWouldStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, state.StateRunning)
	h.rec.BuiltImageAuth = func(context.Context, *spec.AppSpec, string) *api.RegistryAuth {
		return &api.RegistryAuth{Registry: "registry.internal:5000"}
	}

	plan, s, err := h.rec.Shape(ctx, h.appID)
	require.NoError(t, err)
	require.Equal(t, h.appID, plan.BundleID)
	require.NotEmpty(t, plan.Workloads)
	require.Nil(t, plan.Workloads[0].PullAuth, "the shape resolves nothing")

	require.NoError(t, h.rec.Complete(ctx, &plan, s))
	require.NotNil(t, plan.Workloads[0].PullAuth)

	_, _, err = h.rec.Shape(ctx, "app_missing")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
}
