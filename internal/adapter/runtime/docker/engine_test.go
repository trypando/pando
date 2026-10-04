package docker

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
)

// An image is the same image however the engine spells it. Podman reports the
// fully qualified name and a local tag under localhost/; compared as written,
// every Apply on Podman recreated a container that matched its plan.
func TestAnImageIsTheSameImageInEitherEnginesSpelling(t *testing.T) {
	for _, pair := range [][2]string{
		{"alpine:3.20", "docker.io/library/alpine:3.20"},
		{"nginx", "docker.io/library/nginx:latest"},
		{"nginx:latest", "nginx"},
		{"pando-app:abc", "localhost/pando-app:abc"},
		{"ghcr.io/acme/web:1", "ghcr.io/acme/web:1"},
		{"user/app:2", "docker.io/user/app:2"},
	} {
		require.True(t, sameImage(pair[0], pair[1]), "%s = %s", pair[0], pair[1])
	}
	for _, pair := range [][2]string{
		{"alpine:3.20", "alpine:3.21"},
		{"nginx", "nginx:1"},
		{"ghcr.io/acme/web:1", "docker.io/acme/web:1"},
	} {
		require.False(t, sameImage(pair[0], pair[1]), "%s ≠ %s", pair[0], pair[1])
	}

	require.Equal(t, "busybox:1.36", familiarRef("docker.io/library/busybox:1.36"))
	require.Equal(t, "pando-pulled/ab12:app", familiarRef("localhost/pando-pulled/ab12:app"))
	require.Equal(t, "busybox:1.36", familiarRef("busybox:1.36"), "Docker's own spelling is left as it is")
}

// Pando's ownership tags are recognized in Podman's spelling, so an image
// Pando fetched is released when its last app goes (R-224).
func TestR224_OwnershipTagsAreReadInEitherEnginesSpelling(t *testing.T) {
	repo := ownedRepo("busybox:1.36")
	require.Equal(t, repo, ownedRepo("docker.io/library/busybox:1.36"))

	refs, fetched, claimed := unclaimedRefs([]string{
		"docker.io/library/busybox:1.36",
		"localhost/" + repo + ":" + pulledMarker,
	}, repo)
	require.True(t, fetched)
	require.False(t, claimed)
	require.Equal(t, []string{"busybox:1.36"}, refs)

	_, _, claimed = unclaimedRefs([]string{"localhost/" + repo + ":app-01hq8"}, repo)
	require.True(t, claimed, "an app's tag in Podman's spelling is still a claim")
}

// TestR111_ABuiltImageIsNamedAsEitherEngineLoadedIt asserts that the image a
// build hands the runtime is read back by the name the engine recorded, in
// Docker's and Podman's answers alike. Podman ends the line without Docker's
// escaped newline, and the text-cutting parser returned `…:latest"}`.
func TestR111_ABuiltImageIsNamedAsEitherEngineLoadedIt(t *testing.T) {
	for engine, body := range map[string]string{
		"docker": `{"stream":"Loaded image: pando/app-01hq8:latest\n"}` + "\n",
		"podman": `{"stream":"Loaded image: docker.io/pando/app-01hq8:latest"}`,
		"text":   "Loaded image: pando/app-01hq8:latest\n",
	} {
		require.Equal(t, "app-01hq8:latest", strings.TrimPrefix(familiarRef(parseLoadedRef(body)), "pando/"), engine)
	}
	require.Equal(t, "", parseLoadedRef(`{"stream":"Loaded image ID: sha256:abc\n"}`),
		"an untagged load has no reference to run")
}

// An empty health status is no health check, not a failing one (R-221).
func TestR221_AnEmptyHealthStatusIsNoHealthCheck(t *testing.T) {
	require.False(t, reportsHealth(nil))
	require.False(t, reportsHealth(&container.Health{Status: ""}), "Podman's answer for no health check")
	require.True(t, reportsHealth(&container.Health{Status: "starting"}))
	require.True(t, reportsHealth(&container.Health{Status: "unhealthy"}))
}

// TestR245_CPUIsSampledTwiceWhenTheEngineDoesNotDiff asserts that a reading
// with no previous sample is diffed against a second one taken here, not
// against nothing. Podman's one-shot stats carry no previous sample, and a loop
// pinned at half a core read as a few thousandths of one.
func TestR245_CPUIsSampledTwiceWhenTheEngineDoesNotDiff(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /containers/c1/json", respond(http.StatusOK, map[string]any{
		"Id": "c1", "State": map[string]any{"Running": true}, "HostConfig": map[string]any{},
	}))

	// Two readings a second apart by the engine's clock, with half a core's
	// worth of CPU in between — and a system counter that moved by about the
	// same, as Podman's does. Divided by that counter, this read as all four
	// cores; the answer has to come from the wall time instead.
	start := time.Now().UTC()
	readings := []map[string]any{
		{"read": start.Format(time.RFC3339Nano), "cpu_stats": map[string]any{
			"cpu_usage": map[string]any{"total_usage": 10_000_000_000}, "system_cpu_usage": 20_000_000_000, "online_cpus": 4}},
		{"read": start.Add(time.Second).Format(time.RFC3339Nano), "cpu_stats": map[string]any{
			"cpu_usage": map[string]any{"total_usage": 10_500_000_000}, "system_cpu_usage": 20_510_000_000, "online_cpus": 4}},
	}
	served := 0
	f.on("GET /containers/c1/stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, readings[min(served, len(readings)-1)])
		served++
	})

	u := a.workloadUsage(context.Background(), "c1", "web")
	require.Equal(t, 2, served, "a second sample was taken")
	require.Equal(t, 500, u.CPUMillis, "half a core, not the average since boot")
}
