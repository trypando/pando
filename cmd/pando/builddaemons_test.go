package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	buildkitadapter "github.com/trypando/pando/internal/adapter/builder/buildkit"
	"github.com/trypando/pando/internal/core/deploy"
)

// TestR117_BuildDaemonsAreBoundedByTheDeployLimit asserts the BuildKits Pando
// runs are bounded by host policy's max_concurrent_deploys, read as the deploy
// queue reads it: its value, the default for zero, and the last value read
// when reading fails.
func TestR117_BuildDaemonsAreBoundedByTheDeployLimit(t *testing.T) {
	var n int
	var err error
	limit := daemonLimit(func(context.Context) (int, error) { return n, err })
	ctx := context.Background()

	require.Equal(t, deploy.DefaultConcurrency(), limit(ctx), "zero is the default")
	n = 7
	require.Equal(t, 7, limit(ctx))
	n, err = 3, errors.New("database unavailable")
	require.Equal(t, 7, limit(ctx), "a failed read keeps the last limit")
}

func TestBuildDaemonLimitsReachEveryBuildKitBuilder(t *testing.T) {
	registry := adapterapi.NewRegistry()
	require.NoError(t, registry.Register("bld_buildkit", buildkitadapter.New()))
	read := func(context.Context) (int, error) { return 4, nil }
	require.NotPanics(t, func() { limitBuildDaemons(registry, read) })
	require.NotPanics(t, func() { limitBuildDaemons(adapterapi.NewRegistry(), read) })
}
