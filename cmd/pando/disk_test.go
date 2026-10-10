package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
)

// usageRuntime answers Capabilities and Usage; anything else panics on the
// nil embedded interface.
type usageRuntime struct {
	adapterapi.RuntimeAdapter
	reports bool
	capsErr error
}

func (usageRuntime) Kind() string                  { return "fake" }
func (usageRuntime) Category() adapterapi.Category { return adapterapi.CategoryRuntime }
func (r usageRuntime) Capabilities(context.Context) (adapterapi.RuntimeCapabilities, error) {
	return adapterapi.RuntimeCapabilities{ReportsUsage: r.reports}, r.capsErr
}
func (usageRuntime) Usage(_ context.Context, ref adapterapi.BundleRef) (adapterapi.BundleUsage, error) {
	return adapterapi.BundleUsage{Workloads: []adapterapi.WorkloadUsage{{Workload: ref.BundleID, DiskBytes: 42}}}, nil
}

// TestR403_TheDiskPassReadsUsageFromTheAppsOwnRuntime asserts the wiring the
// leader's disk job runs with: usage from the runtime the app's spec names,
// unsupported for a runtime that does not report it or is not configured.
func TestR403_TheDiskPassReadsUsageFromTheAppsOwnRuntime(t *testing.T) {
	ctx := context.Background()
	registry := adapterapi.NewRegistry()
	require.NoError(t, registry.Register("rt_reports", usageRuntime{reports: true}))
	require.NoError(t, registry.Register("rt_silent", usageRuntime{}))
	require.NoError(t, registry.Register("rt_broken", usageRuntime{capsErr: errors.New("down")}))
	u := runtimeUsage{registry: registry}

	reading, supported, err := u.Usage(ctx, "rt_reports", "app_1")
	require.NoError(t, err)
	require.True(t, supported)
	require.Equal(t, int64(42), reading.Workloads[0].DiskBytes)
	require.Equal(t, "app_1", reading.Workloads[0].Workload, "the app's own bundle")

	_, supported, err = u.Usage(ctx, "rt_silent", "app_1")
	require.NoError(t, err)
	require.False(t, supported, "a runtime that does not report usage is never stopped for it")

	_, supported, err = u.Usage(ctx, "rt_gone", "app_1")
	require.NoError(t, err)
	require.False(t, supported)

	_, _, err = u.Usage(ctx, "rt_broken", "app_1")
	require.Error(t, err)

	p := diskPass(nil, registry, nil, nil, zap.NewNop())
	require.NotNil(t, p.Store)
	require.Equal(t, u, p.Usage)
}
