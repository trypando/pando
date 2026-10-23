package spec_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR025_AnAppsUploadIsItsOwn asserts R-025 for uploaded source where a
// spec is written: an upload_id naming another app, or a path, is refused, and
// the app's own or none at all is accepted.
func TestR025_AnAppsUploadIsItsOwn(t *testing.T) {
	up := func(id string) spec.Source { return spec.Source{Type: spec.SourceUpload, UploadID: id} }

	require.NoError(t, spec.CheckUploadOwner(up(""), "app_B"))
	require.NoError(t, spec.CheckUploadOwner(up("app_B"), "app_B"))
	require.NoError(t, spec.CheckUploadOwner(spec.Source{Type: spec.SourceGit, UploadID: "app_A"}, "app_B"),
		"only an upload reads it")

	for _, id := range []string{"app_A", "../app_A", "../../etc/x"} {
		err := spec.CheckUploadOwner(up(id), "app_B")
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), id)
		require.Contains(t, errs.As(err).Remedy, "app_B")
	}
}

// TestR162_AHostnameIsADNSName asserts that an app's hostname is a DNS name
// whichever way the spec is written, so nothing else reaches a routing
// adapter's configuration as an address.
func TestR162_AHostnameIsADNSName(t *testing.T) {
	sub := func(h string) *spec.AppSpec {
		s := valid()
		s.Routing = spec.Routing{AdapterRef: "rte_traefik", Mode: spec.RoutingSubdomain, Hostname: h}
		return s
	}
	for _, h := range []string{"notes.example.com", "Notes.Example.com", "notes.example.com.", "a-1.b.localhost"} {
		require.NoError(t, spec.Validate(sub(h)), h)
	}
	for _, h := range []string{"x`) || HostRegexp(`.+", "notes example.com", "notes", "*.example.com", "-x.example.com", "notes.example.com:8080"} {
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(spec.Validate(sub(h))), h)
	}
}

// TestR240_StorageIsNotMountedOverWhatTheRuntimeProvides asserts that a mount
// path is absolute, carries no colon a runtime could read as options, and is
// not under /proc, /sys or /dev, however it is spelled.
func TestR240_StorageIsNotMountedOverWhatTheRuntimeProvides(t *testing.T) {
	mounted := func(p string) *spec.AppSpec {
		s := valid()
		s.Volumes = []spec.Volume{{ID: "vol_1", Name: "data"}}
		s.Workloads[0].Mounts = []spec.Mount{{VolumeID: "vol_1", Path: p}}
		return s
	}
	for _, p := range []string{"/data", "/var/lib/postgresql/data", "/etc/pihole/", "/var/www/html/", "/device", "/system"} {
		require.NoError(t, spec.Validate(mounted(p)), p)
	}
	for _, p := range []string{"data", "/proc", "/proc/sys", "/sys/fs/cgroup", "/dev", "/dev/shm", "/data/../proc", "/data:rshared", "//sys"} {
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(spec.Validate(mounted(p))), p)
	}
}

// TestR025_AWorkloadAnswersOnlyToItsOwnNames asserts that the names a
// workload answers to on its network are network names, outside the space
// Pando names its own containers in, and not another workload's.
func TestR025_AWorkloadAnswersOnlyToItsOwnNames(t *testing.T) {
	aliased := func(aliases ...string) *spec.AppSpec {
		s := valid()
		s.Workloads[0].Aliases = aliases
		s.Workloads = append(s.Workloads, spec.Workload{Name: "db", Image: "postgres:16"})
		return s
	}
	require.NoError(t, spec.Validate(aliased("appseed_app", "notes.internal", "web", "Postgres-1")))
	for _, a := range []string{"pando-app_01HQ8-db", "PANDO-egress", "pando", "db", "has space", "-x", "", "a:b"} {
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(spec.Validate(aliased(a))), a)
	}

	named := valid()
	named.Workloads[0].Name = "pando-web"
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(spec.Validate(named)))
	require.False(t, spec.ReservedName("pandora"))
}

// TestR240_ALimitIsNeverNegative: no runtime reads a negative size as one.
func TestR240_ALimitIsNeverNegative(t *testing.T) {
	s := valid()
	s.Resources.MemoryBytes = -1
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(spec.Validate(s)))

	w := valid()
	w.Workloads[0].Resources = &spec.ResourceLimits{CPUMillis: -5}
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(spec.Validate(w)))

	require.Equal(t, spec.ResourceLimits{CPUMillis: 500, MemoryBytes: 1 << 20},
		spec.LimitWithin(spec.ResourceLimits{CPUMillis: 500, MemoryBytes: 1 << 20}, spec.Resources{}),
		"an app with no limit of its own leaves the workload's")
}
