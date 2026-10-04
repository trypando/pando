//go:build integration

package docker_test

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/runtime/docker"
)

// inspected is the part of `docker inspect` the swap must carry over.
type inspected struct {
	Name   string
	Image  string
	Config struct {
		Env    []string
		Labels map[string]string
	}
	State struct {
		Running bool
	}
	Mounts []struct {
		Name        string
		Destination string
	}
	NetworkSettings struct {
		Networks map[string]json.RawMessage
	}
}

func inspectContainer(t *testing.T, name string) inspected {
	t.Helper()
	out, err := dockerCLI("inspect", name)
	require.NoError(t, err, out)
	var got []inspected
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	return got[0]
}

func imageID(t *testing.T, ref string) string {
	t.Helper()
	out, err := dockerCLI("image", "inspect", "-f", "{{.Id}}", ref)
	require.NoError(t, err, out)
	return strings.TrimSpace(out)
}

const serves = "RUN mkdir -p /www && echo ok > /www/readyz\nCMD [\"httpd\", \"-f\", \"-p\", \"8080\", \"-h\", \"/www\"]\n"

// TestR359_TheSwapKeepsPandosConfigurationAndPutsTheOldOneBack asserts the
// container half of R-359 against a real Docker daemon: the new container
// takes the old one's name, deployment settings, volume and networks — but not
// the old image's own environment — and a version that does not come up is
// discarded and the old container started again under its name.
func TestR359_TheSwapKeepsPandosConfigurationAndPutsTheOldOneBack(t *testing.T) {
	ctx := context.Background()
	stamp := strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	oldImage, goodImage, badImage := "pando-upg-old:"+stamp, "pando-upg-good:"+stamp, "pando-upg-bad:"+stamp
	buildTestImage(t, oldImage, "FROM busybox:1.37\nENV FROM_IMAGE=old\n"+serves)
	buildTestImage(t, goodImage, "FROM busybox:1.37\nENV FROM_IMAGE=new\n"+serves)
	buildTestImage(t, badImage, "FROM busybox:1.37\nENV FROM_IMAGE=bad\nCMD [\"sh\", \"-c\", \"echo migration 43 failed; exit 3\"]\n")

	net, vol := "pando-upg-net-"+stamp, "pando-upg-vol-"+stamp
	_, err := dockerCLI("network", "create", net)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = dockerCLI("network", "rm", net) })
	t.Cleanup(func() { _, _ = dockerCLI("volume", "rm", vol) })

	start := func(name string) string {
		t.Cleanup(func() {
			_, _ = dockerCLI("rm", "-f", name)
			_, _ = dockerCLI("rm", "-f", name+"-pre-upgrade")
		})
		out, err := dockerCLI("run", "-d", "--name", name, "--network", net, "-v", vol+":/data",
			"-e", "SET_BY_DEPLOY=yes", "--label", "com.docker.compose.service=pando", oldImage)
		require.NoError(t, err, out)
		return strings.TrimSpace(out)
	}

	r, err := docker.NewReplacer()
	require.NoError(t, err)

	t.Run("a version that comes up replaces the old one", func(t *testing.T) {
		name := "pando-upg-ok-" + stamp
		start(name)
		old, err := r.Inspect(ctx, name)
		require.NoError(t, err)
		require.Equal(t, name, old.Name)

		require.NoError(t, r.Stop(ctx, old))
		aside := inspectContainer(t, name+"-pre-upgrade")
		require.False(t, aside.State.Running, "stopped and moved aside, not removed")

		newID, err := r.Recreate(ctx, old, goodImage)
		require.NoError(t, err)
		got := inspectContainer(t, name)
		require.True(t, got.State.Running)
		require.Equal(t, imageID(t, goodImage), got.Image)
		require.Contains(t, got.Config.Env, "SET_BY_DEPLOY=yes", "the deployment's settings carry over")
		require.Contains(t, got.Config.Env, "FROM_IMAGE=new", "the new image's own environment applies")
		require.NotContains(t, got.Config.Env, "FROM_IMAGE=old", "the old image's does not")
		require.Equal(t, "pando", got.Config.Labels["com.docker.compose.service"], "Compose still finds its service")
		require.Contains(t, got.NetworkSettings.Networks, net)
		require.Len(t, got.Mounts, 1)
		require.Equal(t, vol, got.Mounts[0].Name)
		require.Equal(t, "/data", got.Mounts[0].Destination)

		if runtime.GOOS == "linux" {
			// Docker Desktop does not route to container addresses from the
			// host; the helper is on the same network, and CI is Linux.
			require.NoError(t, r.Ready(ctx, newID, "8080", 30*time.Second))
		}

		tag := "pando-upg-tag:" + stamp
		t.Cleanup(func() { _, _ = dockerCLI("rmi", tag) })
		require.NoError(t, r.Finish(ctx, old, goodImage, tag))
		require.Equal(t, imageID(t, goodImage), imageID(t, tag), "the moving tag names the new image")
		_, err = dockerCLI("inspect", name+"-pre-upgrade")
		require.Error(t, err, "the old container is gone")
	})

	t.Run("a version that exits is discarded and the old one started again", func(t *testing.T) {
		name := "pando-upg-bad-" + stamp
		start(name)
		old, err := r.Inspect(ctx, name)
		require.NoError(t, err)
		require.NoError(t, r.Stop(ctx, old))

		newID, err := r.Recreate(ctx, old, badImage)
		require.NoError(t, err)
		err = r.Ready(ctx, newID, "8080", 30*time.Second)
		require.ErrorContains(t, err, "exited with status 3")
		require.Contains(t, r.Logs(ctx, newID), "migration 43 failed", "why it failed is kept")

		r.Discard(ctx, newID)
		require.NoError(t, r.Restore(ctx, old))
		got := inspectContainer(t, name)
		require.True(t, got.State.Running)
		require.Equal(t, imageID(t, oldImage), got.Image)
		require.Contains(t, got.Config.Env, "FROM_IMAGE=old")
	})
}
