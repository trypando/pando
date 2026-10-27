package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/core/hosts"
)

// TestR153_AJoinedHostFencesTheRuntimeItsAppsRunOn asserts R-153 with R-421:
// a joined control host with the app role fences through its agent's
// rootless socket, the runtime its apps run on, not whatever DOCKER_HOST
// names; the install's own host fences the runtime Pando reads from its
// environment.
func TestR153_AJoinedHostFencesTheRuntimeItsAppsRunOn(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/other.sock")
	t.Setenv(multidocker.EnvAgentRuntimeSocket, "/run/user/1000/docker.sock")

	cli, err := fenceClient(hosts.Joined{HostID: "host_1", Roles: []string{"control", "app"}}, true)
	require.NoError(t, err)
	require.Equal(t, "unix:///run/user/1000/docker.sock", cli.DaemonHost())

	cli, err = fenceClient(hosts.Joined{HostID: "host_2", Roles: []string{"control"}}, true)
	require.NoError(t, err)
	require.Equal(t, "unix:///var/run/other.sock", cli.DaemonHost(), "a control-only host runs no apps of its own")

	cli, err = fenceClient(hosts.Joined{}, false)
	require.NoError(t, err)
	require.Equal(t, "unix:///var/run/other.sock", cli.DaemonHost())

	t.Setenv(multidocker.EnvAgentRuntimeSocket, "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	_, err = fenceClient(hosts.Joined{HostID: "host_1", Roles: []string{"app", "control"}}, true)
	require.Error(t, err, "no socket known: the host's pings are refused rather than answered")
}
