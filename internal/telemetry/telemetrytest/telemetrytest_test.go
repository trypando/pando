package telemetrytest_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/telemetry"
	"github.com/trypando/pando/internal/telemetry/telemetrytest"
)

// Count reads what was recorded by name and attributes, and a metric never
// recorded counts zero rather than failing the test that asked.
func TestCountMatchesOnTheAttributesNamed(t *testing.T) {
	metrics := telemetrytest.Install(t, telemetry.Sources{})
	ctx := context.Background()

	require.Zero(t, metrics.Count("pando.deploy.duration", nil))

	telemetry.Deploy(ctx, telemetry.Succeeded, time.Second)
	telemetry.Deploy(ctx, telemetry.Failed, time.Second)
	telemetry.ProxyRequest(ctx, false, "anonymous")
	telemetry.ProxyRequest(ctx, false, "anonymous")

	require.Equal(t, int64(2), metrics.Count("pando.deploy.duration", nil))
	require.Equal(t, int64(1), metrics.Count("pando.deploy.duration", map[string]string{"pando.outcome": "failed"}))
	require.Equal(t, int64(2), metrics.Count("pando.proxy.requests", map[string]string{"pando.proxy.decision": "denied"}))
	require.Zero(t, metrics.Count("pando.proxy.requests", map[string]string{"pando.proxy.decision": "allowed"}))
}
