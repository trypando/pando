package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/state"
)

// TestR142_TheWebhookAndThePollShareOneJob asserts the wiring R-142 depends
// on: the service a webhook reaches asks the very job the poll runs, and both
// read and write the same record of checks.
func TestR142_TheWebhookAndThePollShareOneJob(t *testing.T) {
	apps := state.NewApps(nil)
	job, service := wireAutoDeploy(autoDeployWiring{
		apps: apps, deployments: state.NewDeployments(nil), concurrency: 3, logger: zap.NewNop(),
	})

	require.Same(t, job, service.Checker, "a webhook checks through the poll's own job")
	require.Same(t, job.Checks, service.Checks, "one record of checks")
	require.Same(t, apps, job.Apps)
	require.Same(t, apps, service.Apps)
	require.Equal(t, 3, job.Concurrency)
	require.NotNil(t, job.Resolver)
	require.NotNil(t, service.Secrets)
	require.NotNil(t, service.Audit)
}
