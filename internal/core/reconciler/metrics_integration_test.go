//go:build integration

package reconciler_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/telemetry"
	"github.com/trypando/pando/internal/telemetry/telemetrytest"
)

// TestR399_AReconcileIsInPandosMetrics asserts R-399 for the reconciler: each
// reconcile of an app is recorded with how it ended — succeeded when Pando
// could look at the app, failed when its runtime could not be reached.
func TestR399_AReconcileIsInPandosMetrics(t *testing.T) {
	metrics := telemetrytest.Install(t, telemetry.Sources{})

	up := newHarness(t, state.StateRunning)
	up.runtime.setObserved(healthy())
	up.rec.Tick(context.Background())
	require.Equal(t, int64(1), metrics.Count("pando.reconcile.duration",
		map[string]string{"pando.outcome": "succeeded"}))

	down := newHarness(t, state.StateRunning)
	down.runtime.observeErr = errAdapterDown
	down.rec.Tick(context.Background())
	require.Equal(t, int64(1), metrics.Count("pando.reconcile.duration",
		map[string]string{"pando.outcome": "failed"}), "an app whose runtime cannot be reached was not reconciled")
}
