package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
)

// TestR416_EachControlHostTellsTheRuntimeWhichMachineItIs asserts the
// server's half of several control hosts sharing one host list (R-414,
// R-416): the install's first host is the entry marked control; a joined
// host is the entry of the name it joined with, reached on its agent's
// runtime socket when it runs apps, else on the environment's.
func TestR416_EachControlHostTellsTheRuntimeWhichMachineItIs(t *testing.T) {
	t.Setenv(multidocker.EnvAgentRuntimeSocket, "/run/user/1000/docker.sock")
	require.Equal(t, multidocker.Machine{}, runtimeMachine(hosts.Joined{}, false))

	both := hosts.Joined{Name: "h2", Roles: []string{state.HostRoleControl, state.HostRoleApp}}
	require.Equal(t, multidocker.Machine{Name: "h2", Endpoint: "unix:///run/user/1000/docker.sock"}, runtimeMachine(both, true))

	control := hosts.Joined{Name: "h4", Roles: []string{state.HostRoleControl}}
	require.Equal(t, multidocker.Machine{Name: "h4"}, runtimeMachine(control, true))

	// A socket that cannot be named leaves the environment's.
	t.Setenv(multidocker.EnvAgentRuntimeSocket, "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	require.Equal(t, multidocker.Machine{Name: "h2"}, runtimeMachine(both, true))

	// The runtime is built for the machine it is told.
	a := newAdapterOn("runtime", multidocker.Kind, nil, multidocker.Machine{Name: "h2"})
	require.IsType(t, &multidocker.Adapter{}, a)
}
