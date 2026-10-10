//go:build integration

package docker_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	dockeradapter "github.com/trypando/pando/internal/adapter/runtime/docker"
)

// TestR402_RootlessDockerAppliesTheLimitsPandoSets asserts R-402 on a real
// rootless daemon with the cpu and memory controllers delegated, which is the
// recommended Linux install (issue #130): the adapter knows it is rootless,
// claims limits, and a container given limits has them in its cgroup — not
// merely in its configuration.
//
// Only where PANDO_TEST_ROOTLESS=1 says DOCKER_HOST is such a daemon: the
// rootless job in ci.yml sets one up.
func TestR402_RootlessDockerAppliesTheLimitsPandoSets(t *testing.T) {
	if os.Getenv("PANDO_TEST_ROOTLESS") != "1" {
		t.Skip("needs a rootless Docker daemon with delegated cgroup controllers at DOCKER_HOST; set PANDO_TEST_ROOTLESS=1")
	}
	ctx := context.Background()
	// Not adapter(t), which skips when Docker cannot be reached: here the job
	// said a rootless daemon is there, so not reaching it is a failure, or an
	// adapter regression would leave the job green (CodeRabbit on #177).
	a := dockeradapter.New()
	require.NoError(t, a.Configure(ctx, nil))
	require.NoError(t, a.HealthCheck(ctx), "PANDO_TEST_ROOTLESS=1 says a rootless daemon is at DOCKER_HOST")

	capacity, err := a.Capacity(ctx)
	require.NoError(t, err)
	require.Equal(t, true, capacity.Details["rootless"], "the daemon at DOCKER_HOST is rootless")

	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.True(t, caps.SupportsResourceLimits, "delegated controllers: %s", caps.ResourceLimitsRemedy)

	// Half a core and 64 MiB, read back from inside: cpu.max is quota and
	// period, memory.max is bytes (cgroup v2).
	out, err := execCommand("docker", "run", "--rm", "--cpus", "0.5", "--memory", "64m", "busybox:1.37",
		"cat", "/sys/fs/cgroup/cpu.max", "/sys/fs/cgroup/memory.max")
	require.NoError(t, err, out)
	lines := strings.Fields(out)
	require.Equal(t, []string{"50000", "100000", "67108864"}, lines, "the limits are in the container's cgroup")
}
