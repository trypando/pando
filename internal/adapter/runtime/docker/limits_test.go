package docker

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// TestR240_DockerClaimsLimitsOnlyWhereItAppliesThem asserts the adapter asks
// the daemon before claiming SupportsResourceLimits (issue #130). Rootless
// Docker without the cpu controller delegated accepts --cpus and drops it; the
// adapter says so, and says how to delegate it.
func TestR240_DockerClaimsLimitsOnlyWhereItAppliesThem(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		info     map[string]any
		enforced bool
		remedy   []string
	}{
		{
			name: "rootless without the cpu controller",
			info: map[string]any{"SecurityOptions": []string{"name=seccomp,profile=builtin", "name=rootless", "name=cgroupns"},
				"CgroupVersion": "2", "CpuCfsQuota": false, "CpuCfsPeriod": false, "MemoryLimit": true},
			remedy: []string{"rootless", "cpu cgroup controller", "CPU limits", "Delegate=cpu cpuset io memory pids", "systemctl --user restart docker"},
		},
		{
			name:   "rootful without memory limits",
			info:   map[string]any{"CpuCfsQuota": true, "CpuCfsPeriod": true, "MemoryLimit": false},
			remedy: []string{"cannot apply memory limits", "docker info"},
		},
		{
			// Rootless on cgroup v1: the flags say yes, the driver manages
			// nothing, and Docker ignores the limits.
			name: "rootless without a cgroup driver",
			info: map[string]any{"SecurityOptions": []string{"name=rootless"}, "CgroupVersion": "1", "CgroupDriver": "none",
				"CpuCfsQuota": true, "CpuCfsPeriod": true, "MemoryLimit": true},
			remedy: []string{"Cgroup Driver: none", "cgroup v2", "systemd.unified_cgroup_hierarchy=1"},
		},
		{
			name:     "both applied",
			info:     map[string]any{"SecurityOptions": []string{"name=rootless"}, "CpuCfsQuota": true, "CpuCfsPeriod": true, "MemoryLimit": true},
			enforced: true,
		},
	}
	for _, c := range cases {
		f, a := newFakeDaemon(t, nil)
		f.on("GET /info", respond(http.StatusOK, c.info))
		caps, err := a.Capabilities(ctx)
		require.NoError(t, err, c.name)
		require.Equal(t, c.enforced, caps.SupportsResourceLimits, c.name)
		if c.enforced {
			require.Empty(t, caps.ResourceLimitsRemedy, c.name)
		}
		for _, want := range c.remedy {
			require.Contains(t, caps.ResourceLimitsRemedy, want, c.name)
		}
	}

	// A daemon that cannot say whether it applies limits, or which engine it
	// is, is unavailable, never a yes: the health check only pings, so a
	// failure here must not let a deploy through (CodeRabbit on #177).
	for _, route := range []string{"GET /info", "GET /version"} {
		f, a := newFakeDaemon(t, nil)
		f.on(route, respond(http.StatusInternalServerError, map[string]string{"message": "boom"}))
		_, err := a.Capabilities(ctx)
		require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err), route)
	}
}

// TestR402_DockerReportsWhetherItIsRootless asserts the install shows whether
// the runtime Pando drives runs as root (issue #130): Capacity's details,
// shown under Installation, Capacity, carry it with the limit support.
func TestR402_DockerReportsWhetherItIsRootless(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /info", respond(http.StatusOK, map[string]any{"NCPU": 4, "MemTotal": 8 << 30,
		"SecurityOptions": []string{"name=rootless"}, "CgroupVersion": "2", "CgroupDriver": "systemd",
		"CpuCfsQuota": true, "CpuCfsPeriod": true, "MemoryLimit": true}))
	f.on("GET /containers/json", respond(http.StatusOK, []any{}))
	capacity, err := a.Capacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, true, capacity.Details["rootless"])
	require.Equal(t, "2", capacity.Details["cgroup_version"])
	require.Equal(t, "systemd", capacity.Details["cgroup_driver"])
	require.Equal(t, true, capacity.Details["cpu_limits"])
	require.Equal(t, true, capacity.Details["memory_limits"])
}

// podmanDaemon is a fake daemon that says it is Podman, with Podman 4.9's
// Docker-compatible info: CpuCfsQuota false whatever is delegated.
func podmanDaemon(t *testing.T) (*fakeDaemon, *Adapter) {
	t.Helper()
	f, a := newFakeDaemon(t, nil)
	f.on("GET /version", respond(http.StatusOK, map[string]any{
		"Version": "4.9.3", "Components": []any{map[string]any{"Name": "Podman Engine", "Version": "4.9.3"}},
	}))
	f.on("GET /info", respond(http.StatusOK, map[string]any{"SecurityOptions": []string{"name=rootless"},
		"CpuCfsQuota": false, "CpuCfsPeriod": false, "MemoryLimit": true}))
	return f, a
}

func libpodInfoBody(version string, rootless bool, controllers ...string) map[string]any {
	return map[string]any{"host": map[string]any{
		"cgroupManager": "systemd", "cgroupVersion": version, "cgroupControllers": controllers,
		"security": map[string]any{"rootless": rootless},
	}}
}

// TestR240_PodmanLimitSupportIsReadFromLibpod asserts that on Podman the
// adapter reads limit support from Podman's own info, /libpod/info, whose
// cgroupControllers lists what systemd delegated, and not from its
// Docker-compatible info, which says no CPU limits either way (issue #179).
func TestR240_PodmanLimitSupportIsReadFromLibpod(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		info     map[string]any
		enforced bool
		remedy   []string
	}{
		{
			name:     "rootless with the controllers delegated",
			info:     libpodInfoBody("v2", true, "cpuset", "cpu", "io", "memory", "pids"),
			enforced: true,
		},
		{
			name:   "rootless with systemd's default delegation",
			info:   libpodInfoBody("v2", true, "memory", "pids"),
			remedy: []string{"Podman is running rootless", "cpu cgroup controller", "CPU limits", "Delegate=cpu cpuset io memory pids", "podman info"},
		},
		{
			name:   "rootless on cgroup v1",
			info:   libpodInfoBody("v1", true, "cpu", "memory", "pids"),
			remedy: []string{"cgroup v1", "systemd.unified_cgroup_hierarchy=1"},
		},
		{
			name:     "rootful",
			info:     libpodInfoBody("v2", false, "cpu", "memory"),
			enforced: true,
		},
	}
	for _, c := range cases {
		f, a := podmanDaemon(t)
		f.on("GET /libpod/info", respond(http.StatusOK, c.info))
		caps, err := a.Capabilities(ctx)
		require.NoError(t, err, c.name)
		require.Equal(t, c.enforced, caps.SupportsResourceLimits, c.name)
		if c.enforced {
			require.Empty(t, caps.ResourceLimitsRemedy, c.name)
		}
		for _, want := range c.remedy {
			require.Contains(t, caps.ResourceLimitsRemedy, want, c.name)
		}
		require.NotContains(t, caps.ResourceLimitsRemedy, "Docker", c.name)
	}

	// Podman that cannot say is unavailable, as Docker is, never a yes.
	f, a := podmanDaemon(t)
	f.on("GET /libpod/info", respond(http.StatusInternalServerError, map[string]string{"message": "boom"}))
	_, err := a.Capabilities(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))

	// Capacity's details carry Podman's answer too.
	f, a = podmanDaemon(t)
	f.on("GET /libpod/info", respond(http.StatusOK, libpodInfoBody("v2", true, "cpu", "memory", "pids")))
	f.on("GET /containers/json", respond(http.StatusOK, []any{}))
	capacity, err := a.Capacity(ctx)
	require.NoError(t, err)
	require.Equal(t, true, capacity.Details["rootless"])
	require.Equal(t, "2", capacity.Details["cgroup_version"])
	require.Equal(t, true, capacity.Details["cpu_limits"])
	require.Equal(t, true, capacity.Details["memory_limits"])
}
