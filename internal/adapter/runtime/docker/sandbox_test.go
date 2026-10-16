package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR115_GVisorAndKataAreSandboxedAndNothingElseIs asserts the class each
// OCI runtime is reported as. R-115 names gVisor and Kata as `sandboxed`; any
// other runtime shares the host kernel, and claiming more for it would let a
// policy floor admit work it was set to exclude.
func TestR115_GVisorAndKataAreSandboxedAndNothingElseIs(t *testing.T) {
	for name, want := range map[string]spec.IsolationClass{
		"":                      spec.IsolationContainer,
		"runc":                  spec.IsolationContainer,
		"crun":                  spec.IsolationContainer,
		"nvidia":                spec.IsolationContainer,
		"runsc":                 spec.IsolationSandboxed,
		"runsc-debug":           spec.IsolationSandboxed,
		"kata":                  spec.IsolationSandboxed,
		"kata-fc":               spec.IsolationSandboxed,
		"io.containerd.kata.v2": spec.IsolationSandboxed,
		"runscx":                spec.IsolationContainer, // a prefix is not a match
	} {
		require.Equal(t, want, isolationOf(name), "%q", name)
	}
}

// TestR255_TheReportedClassIsTheConfiguredRuntimes asserts that the class,
// and what the trial run says it can see, follow the configured runtime.
func TestR255_TheReportedClassIsTheConfiguredRuntimes(t *testing.T) {
	ctx := context.Background()

	_, plain := newFakeDaemon(t, nil)
	caps, err := plain.Capabilities(ctx)
	require.NoError(t, err)
	require.Equal(t, spec.IsolationContainer, caps.IsolationClass)
	require.True(t, caps.SupportsPortObservation)
	require.True(t, caps.SupportsWriteObservation)

	_, sandboxed := newFakeDaemon(t, map[string]any{"oci_runtime": "runsc"})
	caps, err = sandboxed.Capabilities(ctx)
	require.NoError(t, err)
	require.Equal(t, spec.IsolationSandboxed, caps.IsolationClass)
	require.True(t, caps.SupportsTrialRun, "the trial still runs, inside the sandbox")
	require.False(t, caps.SupportsPortObservation,
		"R-097: a sidecar cannot see a sandbox's sockets, and reporting none would be a wrong answer")
	require.False(t, caps.SupportsWriteObservation)
}

// TestR255_ARuntimeDockerDoesNotHaveMakesTheAdapterUnavailable asserts that a
// sandboxed class is only ever claimed for a runtime the daemon has. The
// planner refuses to plan against an unhealthy adapter, so a floor of
// `sandboxed` cannot be met by a runtime that was configured but never
// installed — which Docker would otherwise refuse at the first container, after
// the plan had already said yes.
func TestR255_ARuntimeDockerDoesNotHaveMakesTheAdapterUnavailable(t *testing.T) {
	ctx := context.Background()
	info := func(runtimes ...string) http.HandlerFunc {
		m := map[string]any{}
		for _, r := range runtimes {
			m[r] = map[string]any{"path": r}
		}
		return respond(http.StatusOK, map[string]any{"Runtimes": m})
	}

	f, missing := newFakeDaemon(t, map[string]any{"oci_runtime": "runsc"})
	f.on("GET /info", info("runc"))
	err := missing.HealthCheck(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, `"runsc"`, "R-105: it names the runtime")
	require.Contains(t, errs.As(err).Remedy, "daemon.json", "R-105: it says where to fix it")

	f, present := newFakeDaemon(t, map[string]any{"oci_runtime": "runsc"})
	f.on("GET /info", info("runc", "runsc"))
	require.NoError(t, present.HealthCheck(ctx))

	f, plain := newFakeDaemon(t, nil)
	require.NoError(t, plain.HealthCheck(ctx))
	require.Zero(t, f.called("GET /info"), "no runtime configured, nothing to check")
}

// TestR255_PodmanCannotBeTheSandbox asserts that a sandboxed runtime on Podman
// is refused when the adapter is checked, which is before anything is planned.
// Podman lists runsc among its runtimes and does not apply it through this API,
// so the runtimes check alone would have passed.
func TestR255_PodmanCannotBeTheSandbox(t *testing.T) {
	ctx := context.Background()
	podman := respond(http.StatusOK, map[string]any{
		"Version": "4.9.3", "Components": []any{map[string]any{"Name": "Podman Engine", "Version": "4.9.3"}},
	})

	f, sandboxed := newFakeDaemon(t, map[string]any{"oci_runtime": "runsc"})
	f.on("GET /version", podman)
	f.on("GET /info", respond(http.StatusOK, map[string]any{"Runtimes": map[string]any{"runsc": map[string]any{}}}))
	err := sandboxed.HealthCheck(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "Podman")
	require.Contains(t, errs.As(err).Remedy, "Clear the container runtime setting")

	f, plain := newFakeDaemon(t, nil)
	f.on("GET /version", podman)
	require.NoError(t, plain.HealthCheck(ctx), "Podman under its own runtime is fine")
}

// createdRuntimes records the OCI runtime of every container the adapter asks
// the daemon to create, and inspects each as created under the runtime it asked
// for — as Docker does — unless applied says otherwise.
func createdRuntimes(f *fakeDaemon, applied ...string) *[]string {
	var mu sync.Mutex
	got := &[]string{}
	f.on("GET /containers/json", respond(http.StatusOK, []any{}))
	f.on("GET /images/*", respond(http.StatusOK, map[string]any{"Id": "sha256:abc", "Config": map[string]any{}}))
	f.on("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			HostConfig struct{ Runtime string }
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*got = append(*got, body.HostConfig.Runtime)
		mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"Id": "c1", "Warnings": []string{}})
	})
	f.on("GET /containers/c1/json", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		runtime := (*got)[len(*got)-1]
		mu.Unlock()
		if len(applied) > 0 {
			runtime = applied[0]
		}
		writeJSON(w, http.StatusOK, map[string]any{"Id": "c1", "HostConfig": map[string]any{"Runtime": runtime}})
	})
	f.on("DELETE /containers/c1", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	f.on("POST /containers/c1/start", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return got
}

// TestR255_AnEngineThatIgnoresTheRuntimeRunsNothing asserts that a container
// created under a runtime other than the configured one is removed before it
// starts, for a deploy and for a trial alike.
//
// Podman's Docker-compatible API does exactly this: it accepts "runsc",
// lists runsc among its runtimes so HealthCheck passes, and creates the
// container under its default ("oci"). Without the check, the adapter reported
// `sandboxed` and ran the app on the host's kernel.
func TestR255_AnEngineThatIgnoresTheRuntimeRunsNothing(t *testing.T) {
	ctx := context.Background()
	f, a := newFakeDaemon(t, map[string]any{"oci_runtime": "runsc"})
	createdRuntimes(f, "oci")

	plan := api.BundlePlan{BundleID: "app_01HQ8", Network: api.NetworkPlan{Private: true}}
	err := a.applyWorkload(ctx, plan, api.WorkloadPlan{Name: "web", Image: "nginx:1"}, "net1", nil)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, `"runsc"`, "R-105: it names what was asked for")
	require.Contains(t, errs.As(err).Message, `"oci"`, "and what the engine did instead")
	require.NotEmpty(t, errs.As(err).Remedy)

	_, err = a.startTrialContainer(ctx, api.TrialRequest{TrialID: "tr1", Image: "nginx:1"}, "net2")
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))

	require.Zero(t, f.called("POST /containers/c1/start"), "nothing ran outside the sandbox")
	require.Equal(t, 2, f.called("DELETE /containers/c1"), "and what was created is gone")
}

// TestR255_AppCodeRunsUnderTheConfiguredRuntime asserts that both places an
// app's own code runs — its deploy and the trial before it — use the runtime
// the class was reported for. A trial under runc on a sandboxed install would
// put the least trusted moment of an app's life outside the sandbox.
func TestR255_AppCodeRunsUnderTheConfiguredRuntime(t *testing.T) {
	ctx := context.Background()
	f, a := newFakeDaemon(t, map[string]any{"oci_runtime": "runsc"})
	got := createdRuntimes(f)

	plan := api.BundlePlan{BundleID: "app_01HQ8", Network: api.NetworkPlan{Private: true}}
	require.NoError(t, a.applyWorkload(ctx, plan, api.WorkloadPlan{Name: "web", Image: "nginx:1"}, "net1", nil))

	_, err := a.startTrialContainer(ctx, api.TrialRequest{TrialID: "tr1", Image: "nginx:1"}, "net2")
	require.NoError(t, err)

	require.Equal(t, []string{"runsc", "runsc"}, *got)

	// And with none configured, Docker's default: the field is left empty
	// rather than set to "runc", so a daemon whose default is something else
	// keeps it.
	f, plain := newFakeDaemon(t, nil)
	got = createdRuntimes(f)
	require.NoError(t, plain.applyWorkload(ctx, plan, api.WorkloadPlan{Name: "web", Image: "nginx:1"}, "net1", nil))
	require.Equal(t, []string{""}, *got)
}

// TestR255_ChangingTheRuntimeMovesAppsOnTheirNextDeploy asserts that a
// container under a runtime other than the configured one does not match its
// plan, however unchanged its image and environment are. The reported class
// changes the moment the setting does; without this, a redeploy after switching
// to runsc would have left the app on runc while the adapter called it
// sandboxed.
func TestR255_ChangingTheRuntimeMovesAppsOnTheirNextDeploy(t *testing.T) {
	w := api.WorkloadPlan{Name: "web", Image: "nginx:1"}

	matches := func(configured, running string) bool {
		t.Helper()
		config := map[string]any{}
		if configured != "" {
			config["oci_runtime"] = configured
		}
		f, a := newFakeDaemon(t, config)
		f.on("GET /containers/c1/json", respond(http.StatusOK, map[string]any{
			"Id":         "c1",
			"Config":     map[string]any{"Image": "nginx:1", "Labels": map[string]string{labelHardening: hardeningLevel}},
			"HostConfig": map[string]any{"Runtime": running, "PidsLimit": DefaultPidsLimit},
		}))
		ok, err := a.matchesPlan(context.Background(), "c1", w, "pando-app", nil)
		require.NoError(t, err)
		return ok
	}

	require.True(t, matches("", "runc"), "nothing changed")
	require.True(t, matches("", "crun"), "a daemon whose default is crun is not a reason to recreate")
	require.True(t, matches("runsc", "runsc"), "already in the sandbox")

	require.False(t, matches("runsc", "runc"), "switched on: into the sandbox on the next deploy")
	require.False(t, matches("", "runsc"), "switched off: out of it on the next deploy")
	require.False(t, matches("kata", "runsc"), "one sandbox for another is still a change")
}
