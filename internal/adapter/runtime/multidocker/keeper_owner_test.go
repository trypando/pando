package multidocker

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/errs"
)

// kept is labels with the ones an agent keeper puts on what it makes.
func kept(labels map[string]string) map[string]string {
	out := map[string]string{labelManaged: "true", labelRole: roleAgent}
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// TestR416_TheAgentKeeperNeverRemovesAContainerItDidNotMake asserts that the
// keeper replaces or removes only a container it made, by its labels: on a
// host that joined the cluster the container named pando-agent is that
// host's own Pando, which runs its agent (R-416), and a keeper reaching that
// Docker as if it were its own once force-removed it. It is refused with
// what to do, and nothing is removed, created or started.
func TestR416_TheAgentKeeperNeverRemovesAContainerItDidNotMake(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"no labels":                 nil,
		"Pando's own container":     {"com.docker.compose.service": "pando"},
		"managed, but another role": {labelManaged: "true", labelRole: "edge"},
		"the role, but not managed": {labelRole: roleAgent},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDaemon(t)
			f.on("GET /containers/pando-agent/json", respond(http.StatusOK, map[string]any{
				"Id": "c-pando", "Config": map[string]any{"Labels": labels}, "State": map[string]any{"Running": false},
			}))
			f.on("DELETE /containers/c-pando", noContent)
			k := newKeeper(t, f, testAgentImage)

			created, err := k.ensure(context.Background())
			require.False(t, created)
			require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
			require.Contains(t, errs.As(err).Message, "not made by Pando's Docker hosts runtime")
			require.Contains(t, errs.As(err).Message, "app-1")
			require.Contains(t, errs.As(err).Remedy, "agent://")
			require.Zero(t, f.called("DELETE"), "never removed")
			require.Zero(t, f.called("POST /containers/c-pando/start"), "never started")
			require.Zero(t, f.called("POST /containers/create"))
		})
	}
}

// TestR416_TheInstallMachinesAgentCarriesRuntimeCalls asserts that the agent
// the runtime keeps on the install's own machine is given that machine's
// runtime socket, so the cluster's other control hosts reach the machine's
// Docker through it as they reach a joined host's (R-416): mounted into the
// agent alone, with the socket's group, and never published — the agent
// still publishes only its own port. An agent kept without it is made again.
func TestR416_TheInstallMachinesAgentCarriesRuntimeCalls(t *testing.T) {
	f := newFakeDaemon(t)
	f.on("GET /networks", respond(http.StatusOK, []map[string]any{{"Id": "net-agent", "Name": agentNetwork}}))
	f.on("GET /images/"+testAgentImage+"/json", respond(http.StatusOK, map[string]any{"Id": "sha256:abc"}))
	f.on("POST /containers/create", respond(http.StatusCreated, map[string]any{"Id": "c-new"}))
	f.on("POST /containers/c-new/start", noContent)
	k := newKeeper(t, f, testAgentImage)
	sock := docker.OwnSocket{Source: "/run/user/1000/docker.sock", Group: "998"}
	k.socket = func(context.Context) (docker.OwnSocket, bool) { return sock, true }
	f.on("GET /containers/pando-agent/json", respond(http.StatusOK, map[string]any{
		"Id": "c-old", "Config": map[string]any{"Labels": kept(map[string]string{
			labelAgentDigest: k.digest(testAgentImage), labelAgentIssued: time.Now().UTC().Format(time.RFC3339)})},
		"State": map[string]any{"Running": true},
	}))
	f.on("DELETE /containers/c-old", noContent)

	created, err := k.ensure(context.Background())
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, 1, f.called("DELETE /containers/c-old"), "the agent kept without the socket is made again with it")

	var got struct {
		Env        []string
		HostConfig struct {
			Mounts       []struct{ Type, Source, Target string }
			GroupAdd     []string
			PortBindings map[string][]struct{ HostPort string }
		}
	}
	require.NoError(t, json.Unmarshal(f.body("POST /containers/create"), &got))
	require.Contains(t, got.Env, EnvAgentRuntimeSocket+"="+agentRuntimeSocket)
	require.Len(t, got.HostConfig.Mounts, 1)
	require.Equal(t, "bind", got.HostConfig.Mounts[0].Type)
	require.Equal(t, sock.Source, got.HostConfig.Mounts[0].Source)
	require.Equal(t, agentRuntimeSocket, got.HostConfig.Mounts[0].Target)
	require.Equal(t, []string{"998"}, got.HostConfig.GroupAdd)
	require.Len(t, got.HostConfig.PortBindings, 1, "only the agent's own port is published")
	require.NotEqual(t, k.digest(testAgentImage), k.digestWith(testAgentImage, sock))

	// Without a socket to give, the agent is made as before, so agents on
	// hosts an operator listed are not made again.
	require.Equal(t, k.digest(testAgentImage), k.digestWith(testAgentImage, docker.OwnSocket{}))
}
