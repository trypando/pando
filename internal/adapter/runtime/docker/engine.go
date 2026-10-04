package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/errs"
)

// Where engines that speak Docker's API answer it differently.
//
// This adapter talks to anything that serves the Docker Engine API, and Podman
// does — but the same call can come back in a different shape. Each difference
// below was found by running this adapter's integration suite against rootless
// Podman 4.9, and each one had turned into a wrong answer somewhere: an app
// recreated on every pass, an image never cleaned up, an app with no health
// check reported unhealthy, a CPU reading near zero, and an app run outside
// the sandbox the adapter reported.

// familiarRef is an image reference in its short, familiar form, whichever
// engine spelled it.
//
// Docker reports an image by the name it was asked for ("alpine:3.20"); Podman
// reports it fully qualified ("docker.io/library/alpine:3.20"), and names a
// tag it made locally under "localhost/". Compared as written, the same image
// is two different ones.
func familiarRef(ref string) string {
	named, err := reference.ParseNormalizedNamed(strings.TrimPrefix(ref, "localhost/"))
	if err != nil {
		return ref
	}
	return reference.FamiliarString(named)
}

// sameImage reports whether two references name the same image, an absent tag
// meaning latest as it does to the engine.
func sameImage(a, b string) bool {
	return withTag(a) == withTag(b)
}

func withTag(ref string) string {
	named, err := reference.ParseNormalizedNamed(strings.TrimPrefix(ref, "localhost/"))
	if err != nil {
		return ref
	}
	return reference.FamiliarString(reference.TagNameOnly(named))
}

// reportsHealth reports whether a container has a health check to report on.
//
// Docker leaves Health out when there is no check. Podman includes it with an
// empty status. Read as a status, that empty string is "not healthy" — and an
// app with no health check is running, not perpetually degraded (R-221).
func reportsHealth(h *container.Health) bool {
	return h != nil && h.Status != ""
}

// isPodman reports whether the engine behind the API is Podman, which names
// itself among the version's components.
func isPodman(v client.ServerVersionResult) bool {
	for _, c := range v.Components {
		if strings.Contains(strings.ToLower(c.Name), "podman") {
			return true
		}
	}
	return false
}

// verifyRuntime checks that a container was created under the OCI runtime this
// adapter is configured for, and removes it if it was not.
//
// The class Capabilities reports rests on it (R-255), and an engine can accept
// the setting without applying it: Podman's Docker-compatible API takes a
// HostConfig.Runtime of "runsc", lists runsc among its runtimes — so
// HealthCheck passes — and starts the container under its default all the
// same. Checked on every container that runs app code, before it starts,
// because an app that ran once outside the sandbox the plan promised is the
// failure this setting exists to prevent.
func (a *Adapter) verifyRuntime(ctx context.Context, containerID, workload string) error {
	want := a.config.OCIRuntime
	if want == "" {
		return nil
	}
	res, err := a.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		_ = a.removeContainer(context.WithoutCancel(ctx), containerID)
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not check how %q was set up.", workload), err)
	}
	got := ""
	if res.Container.HostConfig != nil {
		got = res.Container.HostConfig.Runtime
	}
	if got == want {
		return nil
	}
	_ = a.removeContainer(context.WithoutCancel(ctx), containerID)
	return errs.Newf(errs.AdapterFailed,
		"Pando did not start %q: this runtime is set to run apps under %q, and the container engine created it under %q instead.",
		workload, want, got).
		WithRemedy("The engine accepted the container runtime setting without applying it, which Podman's " +
			"Docker-compatible API does. Pando will not run an app outside the sandbox it reported. Use Docker " +
			"with the runtime registered in /etc/docker/daemon.json, or clear the container runtime setting.")
}
