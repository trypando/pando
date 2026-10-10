//go:build integration

package buildkit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

// TestR111_PandosOwnBuildKitStartsAndAnswers asserts R-111 against a real
// Docker daemon: the container Pando starts runs under the compiled-in profile
// and answers BuildKit's API on the address ensure returns. Names and the port
// are its own, so it never touches an installation's BuildKit on the same
// daemon, and everything it made is removed by name afterwards.
func TestR111_PandosOwnBuildKitStartsAndAnswers(t *testing.T) {
	ctx := context.Background()
	// One daemon for the adapter and the docker CLI the test inspects and
	// cleans up with: unset, the client takes the default socket and the CLI
	// its current context, which can be two daemons.
	if os.Getenv("DOCKER_HOST") == "" {
		t.Setenv("DOCKER_HOST", client.DefaultDockerHost)
	}
	dc, err := client.New(client.FromEnv)
	require.NoError(t, err)

	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	m := newManaged(dc)
	m.name, m.network, m.volume, m.port = "pando-test-buildkit-"+stamp, "pando-test-build-"+stamp, "pando-test-buildkit-cache-"+stamp, "21234"
	m.named = true
	m.self = "not-a-container-" + stamp // the test runs on the host
	t.Cleanup(func() {
		for _, args := range [][]string{{"rm", "-f", m.name}, {"network", "rm", m.network}, {"volume", "rm", m.volume}} {
			_ = exec.Command("docker", args...).Run()
		}
	})

	addr, err := m.ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, "tcp://127.0.0.1:21234", addr)

	out, err := exec.Command("docker", "inspect", "-f", "{{json .HostConfig.SecurityOpt}}", m.name).CombinedOutput()
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), `seccomp={\"defaultAction\":\"SCMP_ACT_ERRNO\"`, "the profile itself, inline")
	require.Contains(t, string(out), "Rootless BuildKit")

	bk, err := bkclient.New(ctx, addr)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		workers, err := bk.ListWorkers(c)
		return err == nil && len(workers) > 0
	}, 60*time.Second, time.Second, "BuildKit answers on %s", addr)

	again, err := m.ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, addr, again)
	out, err = exec.Command("docker", "ps", "-a", "-q", "-f", "name=^"+m.name+"$").CombinedOutput()
	require.NoError(t, err)
	require.Len(t, strings.Fields(string(out)), 1, "one container, found the second time")
}
