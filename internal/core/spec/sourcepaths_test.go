package spec_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

// TestR020_ABuildCannotNameAPathOutsideTheSource asserts R-020: the
// subdirectory, a build context and a Dockerfile are paths inside the app's
// source, and none of them, alone or together, is absolute or climbs above
// the repository.
func TestR020_ABuildCannotNameAPathOutsideTheSource(t *testing.T) {
	for name, edit := range map[string]func(*spec.AppSpec){
		"a subdirectory above the repository": func(s *spec.AppSpec) { s.Source.Subdir = "../x" },
		"a context above the repository":      func(s *spec.AppSpec) { s.Build.Context = "../.." },
		"a context above it from a subdirectory": func(s *spec.AppSpec) {
			s.Source.Subdir = "app"
			s.Build.Context = "../.."
		},
		"an absolute context":               func(s *spec.AppSpec) { s.Build.Context = "/var/lib/pando" },
		"an absolute Dockerfile":            func(s *spec.AppSpec) { s.Build.Dockerfile = "/var/lib/pando/token.key" },
		"a Dockerfile above the repository": func(s *spec.AppSpec) { s.Build.Dockerfile = "../../../../../var/lib/pando/token.key" },
		"a Dockerfile above it from a context": func(s *spec.AppSpec) {
			s.Build.Context = "web"
			s.Build.Dockerfile = "../../Dockerfile"
		},
		"a service's context above the repository": func(s *spec.AppSpec) {
			s.Workloads[0].Build = &spec.WorkloadBuild{Context: ".."}
		},
		"a service's absolute Dockerfile": func(s *spec.AppSpec) {
			s.Workloads[0].Build = &spec.WorkloadBuild{Context: ".", Dockerfile: "/etc/passwd"}
		},
	} {
		s := valid()
		edit(s)
		err := spec.Validate(s)
		require.Error(t, err, name)
	}
}

// TestR020_ABuildMayNameASiblingInsideTheSource asserts the other side of
// R-020's rule: ".." that stays inside the repository is a path in the
// source, as a compose service's `dockerfile: ../docker/web.Dockerfile` is,
// and a leading slash on the subdirectory means the repository's root.
func TestR020_ABuildMayNameASiblingInsideTheSource(t *testing.T) {
	s := valid()
	s.Source.Subdir = "/services"
	s.Build.Context = "web"
	s.Build.Dockerfile = "../docker/web.Dockerfile"
	s.Workloads[0].Build = &spec.WorkloadBuild{Context: "api/..", Dockerfile: "docker/Dockerfile"}
	require.NoError(t, spec.Validate(s))
}

// TestR020_ARevisionStoredBeforeValidationStillDeploys asserts R-020 for
// revisions written before validation refused paths outside the repository:
// read back, a subdirectory or context that is absolute or climbs above the
// repository names the place a build was always given for it, and the
// revision validates. A path inside the repository is left exactly as it is.
func TestR020_ARevisionStoredBeforeValidationStillDeploys(t *testing.T) {
	s := valid()
	s.Source.Subdir = "../../app"
	s.Build.Context = "/web"
	s.Workloads[0].Build = &spec.WorkloadBuild{Context: "../../.."}
	worker := s.Workloads[0]
	worker.Name, worker.Primary = "worker", false
	worker.Build = &spec.WorkloadBuild{Context: "../shared"}
	s.Workloads = append(s.Workloads, worker)
	require.Error(t, spec.Validate(s))

	spec.ConfineStoredPaths(s)
	require.Equal(t, "app", s.Source.Subdir)
	require.Equal(t, "web", s.Build.Context)
	require.Empty(t, s.Workloads[0].Build.Context)
	require.Equal(t, "../shared", s.Workloads[1].Build.Context, "a sibling inside the repository is kept as written")
	require.NoError(t, spec.Validate(s))

	inside := valid()
	inside.Source.Subdir = "services"
	inside.Build.Context = "./web"
	before := *inside
	spec.ConfineStoredPaths(inside)
	require.Equal(t, before.Source, inside.Source)
	require.Equal(t, before.Build, inside.Build)
	spec.ConfineStoredPaths(nil)
}
