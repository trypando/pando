//go:build integration

package docker_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// buildTestImage builds a tiny image from dockerfile under name, removing it
// when the test ends. Everything it creates is named by the caller with a test-
// prefix, so nothing else on the host is touched.
func buildTestImage(t *testing.T, name, dockerfile string, labels ...string) {
	t.Helper()
	args := []string{"build", "-q", "-t", name}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	build := exec.Command("docker", append(args, "-")...)
	build.Stdin = strings.NewReader(dockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build the test image: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = dockerCLI("rmi", name) })
}

// TestR224_DeletingAnAppRemovesEveryImageBuiltForIt asserts R-224. A rebuild
// moves the tag and leaves the previous image untagged, so the images built
// for an app are found by the builder's label and all of them go with it
// (issue #55).
func TestR224_DeletingAnAppRemovesEveryImageBuiltForIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	stamp := time.Now().Format("150405.000")
	id := "test-built-" + strings.ReplaceAll(stamp, ".", "")
	other := id + "-other"

	current, previous, kept := "test-built-"+id+":current", "test-built-"+id+":previous", "test-built-"+id+":kept"
	buildTestImage(t, current, "FROM alpine:3.20\nLABEL test.generation=2\n", api.ImageLabelBundle+"="+id)
	buildTestImage(t, previous, "FROM alpine:3.20\nLABEL test.generation=1\n", api.ImageLabelBundle+"="+id)
	buildTestImage(t, kept, "FROM alpine:3.20\nLABEL test.generation=3\n", api.ImageLabelBundle+"="+other)

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: id}, api.DestroyOptions{}))

	for _, img := range []string{current, previous} {
		_, err := dockerCLI("image", "inspect", img)
		require.Error(t, err, "%s was built for the deleted app", img)
	}
	_, err := dockerCLI("image", "inspect", kept)
	require.NoError(t, err, "an image built for another app is not this deletion's to remove")
}

// Vaultwarden declares VOLUME /data and refuses to start without storage
// there. A trial gives each declared path throwaway storage, so the app starts
// the way it will when deployed with a volume (issue #55).
func TestATrialMountsThrowawayStorageWhereTheImageDeclaresAVolume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	img := "test-trial-volume-" + time.Now().Format("150405") + ":latest"
	buildTestImage(t, img, "FROM alpine:3.20\nVOLUME /data\n")

	result, err := a.Trial(ctx, api.TrialRequest{
		TrialID: trialID(t),
		Image:   img,
		Command: []string{"sh", "-c", "grep ' /data ' /proc/mounts; exit 3"},
		Timeout: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"/data"}, result.ImageVolumes)
	require.Contains(t, result.Log, "tmpfs", "the declared path was throwaway storage, not an anonymous volume")
}

// TestR025_StartupReclaimsOnlyNetworksNoContainerBelongsTo asserts R-025
// against a real daemon. A stopped container is not an endpoint, so a stopped
// app's network looks empty; removing it left the app unable to start again
// (issue #55). Only an owned network no container belongs to is reclaimed.
func TestR025_StartupReclaimsOnlyNetworksNoContainerBelongsTo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	now := time.Now()
	prefix := "test-reclaim-" + now.Format("150405")
	empty, stopped, theirs := prefix+"-empty", prefix+"-stopped", prefix+"-theirs"

	third := 100 + now.Nanosecond()%100
	for i, bundle := range []string{empty, stopped, theirs} {
		name := bundle
		subnet := fmt.Sprintf("10.231.%d.%d/28", third, i*16)
		if out, err := dockerCLI("network", "create", "--subnet", subnet,
			"--label", "io.pando.managed=true", "--label", "io.pando.bundle="+bundle, name); err != nil {
			t.Skipf("could not create a test network: %v %s", err, out)
		}
		t.Cleanup(func() { _, _ = dockerCLI("network", "rm", name) })
	}

	holder := stopped + "-web"
	if out, err := dockerCLI("create", "--name", holder, "--network", stopped,
		"--label", "io.pando.bundle="+stopped, "alpine:3.20", "true"); err != nil {
		t.Fatalf("could not create the stopped container: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = dockerCLI("rm", "-f", holder) })

	mine := func(bundle string) bool { return bundle == empty || bundle == stopped }
	n, err := a.ReclaimNetworks(ctx, mine)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	_, err = dockerCLI("network", "inspect", empty)
	require.Error(t, err, "nothing belongs to it: reclaimed")
	_, err = dockerCLI("network", "inspect", stopped)
	require.NoError(t, err, "a stopped app's network is kept so the app can start again")
	_, err = dockerCLI("network", "inspect", theirs)
	require.NoError(t, err, "another install's network is left alone")
}
