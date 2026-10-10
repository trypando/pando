package docker

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
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

	// Podman's Docker-compatible info says CpuCfsQuota false with the cpu
	// controller delegated (Podman 4.9 in the podman CI run), so its answer is
	// not taken as one: Podman keeps the support it had.
	f, pod := newFakeDaemon(t, nil)
	f.on("GET /version", respond(http.StatusOK, map[string]any{
		"Version": "4.9.3", "Components": []any{map[string]any{"Name": "Podman Engine", "Version": "4.9.3"}},
	}))
	f.on("GET /info", respond(http.StatusOK, map[string]any{"SecurityOptions": []string{"name=rootless"},
		"CpuCfsQuota": false, "CpuCfsPeriod": false, "MemoryLimit": true}))
	caps, err := pod.Capabilities(ctx)
	require.NoError(t, err)
	require.True(t, caps.SupportsResourceLimits, "Podman")

	// A daemon that cannot be asked is the health check's to refuse, with its
	// own message; capabilities do not invent a second reason.
	f, a := newFakeDaemon(t, nil)
	f.on("GET /info", respond(http.StatusInternalServerError, map[string]string{"message": "boom"}))
	caps, err = a.Capabilities(ctx)
	require.NoError(t, err)
	require.True(t, caps.SupportsResourceLimits)
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
