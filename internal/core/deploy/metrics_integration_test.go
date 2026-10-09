//go:build integration

package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/telemetry"
	"github.com/trypando/pando/internal/telemetry/telemetrytest"
)

// TestR399_ADeployIsInPandosMetrics asserts R-399 for deploys: a deploy that
// ends is recorded with its outcome — here one that could not start, because
// its revision is gone. A deploy that succeeds is recorded the same way, at
// the same place (queue.run), and is covered end to end in test/acceptance.
func TestR399_ADeployIsInPandosMetrics(t *testing.T) {
	metrics := telemetrytest.Install(t, telemetry.Sources{})
	db, deps := queuedDeploys(t, "metrics-gone-rev")

	deployments := state.NewDeployments(db)
	q := &Queue{
		Deployments: deployments,
		Revisions: revisionsFunc(func(context.Context, string) (state.Revision, bool, error) {
			return state.Revision{}, false, nil
		}),
		Poll: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go q.Serve(ctx)

	require.Eventually(t, func() bool {
		dep, _, err := deployments.ByID(context.Background(), deps["metrics-gone-rev"].ID)
		return err == nil && dep.Status == state.DeployFailed
	}, 30*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		return metrics.Count("pando.deploy.duration", map[string]string{"pando.outcome": "failed"}) == 1
	}, 5*time.Second, 20*time.Millisecond)
	require.Zero(t, metrics.Count("pando.deploy.duration", map[string]string{"pando.outcome": "succeeded"}))
}
