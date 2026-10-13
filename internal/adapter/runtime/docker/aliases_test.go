package docker

import (
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR096_AContainerMissingAPlannedAliasIsNotReused asserts R-096: a
// container is reused only when it answers to every name the plan gives it. A
// redeploy that added a compose container_name alias kept the old container,
// and what addressed the service by that name stopped resolving it.
func TestR096_AContainerMissingAPlannedAliasIsNotReused(t *testing.T) {
	const id = "3f1c9a2b7d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4"
	w := api.WorkloadPlan{Name: "db", Aliases: []string{"app_db"}}

	// Docker adds the short container ID as an alias of its own; it is not a mismatch.
	require.True(t, hasAliases(&network.EndpointSettings{Aliases: []string{"db", "app_db", id[:12]}}, id, w))
	require.False(t, hasAliases(&network.EndpointSettings{Aliases: []string{"db", id[:12]}}, id, w),
		"the container_name alias the plan added is missing")
	require.False(t, hasAliases(nil, id, w))
}
