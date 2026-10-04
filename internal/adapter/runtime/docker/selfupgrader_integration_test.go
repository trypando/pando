//go:build integration

package docker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	dockeradapter "github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/secret"
)

// TestR355_TheRuntimeKnowsPandosOwnContainerAndStartsTheHelperBesideIt
// asserts the adapter half of R-355 and R-359: Self reports how Pando's
// container was started, so the upgrade can tell a moving tag from a pin;
// StartHelper runs Pando's own image with Pando's mounts and networks and the
// database URL in its environment; RemoveHelpers clears finished ones.
//
// "Pando" here is a long-running busybox container named by proxy_container,
// which is how the adapter is told its own container when it is not running
// in one.
func TestR355_TheRuntimeKnowsPandosOwnContainerAndStartsTheHelperBesideIt(t *testing.T) {
	ctx := context.Background()
	stamp := strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	name, net, vol := "pando-self-"+stamp, "pando-self-net-"+stamp, "pando-self-vol-"+stamp

	_, err := dockerCLI("network", "create", net)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = dockerCLI("network", "rm", net) })
	t.Cleanup(func() { _, _ = dockerCLI("volume", "rm", vol) })
	out, err := dockerCLI("run", "-d", "--name", name, "--network", net, "-v", vol+":/data", "busybox:1.37", "sleep", "300")
	require.NoError(t, err, out)
	t.Cleanup(func() {
		_, _ = dockerCLI("rm", "-f", name)
		_, _ = dockerCLI("rm", "-f", name+"-upgrade")
	})

	a := dockeradapter.New()
	cfg, _ := json.Marshal(map[string]string{"proxy_container": name})
	require.NoError(t, a.Configure(ctx, cfg))
	if err := a.HealthCheck(ctx); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}

	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.True(t, caps.SupportsSelfUpgrade, "Pando's container is one this daemon runs")

	self, err := a.Self(ctx)
	require.NoError(t, err)
	require.Equal(t, "busybox:1.37", self.Image, "the reference as the deployment named it")

	require.NoError(t, a.PullImage(ctx, "busybox:1.37"))
	require.Error(t, a.PullImage(ctx, "pando-no-such-image-"+stamp+":1"), "a missing image fails before Pando stops")

	id, err := a.StartHelper(ctx, api.HelperSpec{
		Args: []string{"sh", "-c", `test -d /data && echo "url=$UPGRADE_DATABASE_URL"`},
		Env:  map[string]secret.Value{"UPGRADE_DATABASE_URL": secret.New("postgres://pando:pw@postgres/pando")},
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	var logs string
	require.Eventually(t, func() bool {
		logs, _ = dockerCLI("logs", name+"-upgrade")
		return strings.Contains(logs, "url=")
	}, 30*time.Second, 500*time.Millisecond)
	require.Contains(t, logs, "url=postgres://pando:pw@postgres/pando", "the helper has Pando's volume and the URL")

	helper := inspectContainer(t, name+"-upgrade")
	require.Contains(t, helper.NetworkSettings.Networks, net, "on Pando's network, to reach Postgres by name")
	inspectArgs, err := dockerCLI("inspect", "-f", "{{json .Args}}", name+"-upgrade")
	require.NoError(t, err)
	require.NotContains(t, inspectArgs, "pw@", "the URL is in the environment, never the command line")

	require.Eventually(t, func() bool {
		state, _ := dockerCLI("inspect", "-f", "{{.State.Running}}", name+"-upgrade")
		return state == "false"
	}, 30*time.Second, 500*time.Millisecond)
	require.NoError(t, a.RemoveHelpers(ctx))
	_, err = dockerCLI("inspect", name+"-upgrade")
	require.Error(t, err, "a finished helper is removed")

	// Not in a container: nothing to replace, said plainly.
	elsewhere := dockeradapter.New()
	cfg, _ = json.Marshal(map[string]string{"proxy_container": "pando-not-a-container-" + stamp})
	require.NoError(t, elsewhere.Configure(ctx, cfg))
	_, err = elsewhere.Self(ctx)
	require.ErrorContains(t, err, "not running in a container")
	_, err = elsewhere.StartHelper(ctx, api.HelperSpec{Args: []string{"true"}})
	require.Error(t, err)
	caps, err = elsewhere.Capabilities(ctx)
	require.NoError(t, err)
	require.False(t, caps.SupportsSelfUpgrade)
}
