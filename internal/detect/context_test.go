package detect_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// TestR096_AComposeBuildContextAboveTheAppsDirectoryIsBuiltFromThere asserts
// R-096: a compose file is imported as written, its build context included. A
// compose file in docker/standard/ building with `context: ../..` was cut back
// to docker/standard/, and the build could not find the Dockerfile the compose
// file named (issue #90).
func TestR096_AComposeBuildContextAboveTheAppsDirectoryIsBuiltFromThere(t *testing.T) {
	draft := detect.Draft{
		Build: spec.Build{Strategy: spec.BuildCompose, ComposeFile: "docker-compose.yml"},
		Workloads: []spec.Workload{
			{Name: "web", Primary: true, Build: &spec.WorkloadBuild{Context: "../..", Dockerfile: "docker/standard/Dockerfile"}},
			{Name: "worker", Build: &spec.WorkloadBuild{Context: "./worker"}},
			{Name: "db", Image: "postgres:17"},
		},
	}
	got := detect.Assemble("app_1", spec.Source{Type: spec.SourceGit, Subdir: "docker/standard"}, draft)

	require.Empty(t, got.Source.Subdir, "built from the repository root, which is what the compose file names")
	require.Equal(t, "docker/standard/docker-compose.yml", got.Build.ComposeFile)
	require.Equal(t, ".", got.Workloads[0].Build.Context)
	require.Equal(t, "docker/standard/Dockerfile", got.Workloads[0].Build.Dockerfile, "relative to its context, as compose has it")
	require.Equal(t, "docker/standard/worker", got.Workloads[1].Build.Context)
	require.Nil(t, got.Workloads[2].Build)
	require.Equal(t, "../..", draft.Workloads[0].Build.Context, "the draft itself is not changed")

	// One level up, and no further than it reaches.
	got = detect.Assemble("app_1", spec.Source{Subdir: "apps/site/docker"}, detect.Draft{
		Workloads: []spec.Workload{{Name: "web", Build: &spec.WorkloadBuild{Context: ".."}}},
	})
	require.Equal(t, "apps/site", got.Source.Subdir)
	require.Equal(t, ".", got.Workloads[0].Build.Context)

	// Contexts inside the directory leave it alone.
	got = detect.Assemble("app_1", spec.Source{Subdir: "deploy"}, detect.Draft{
		Workloads: []spec.Workload{{Name: "web", Build: &spec.WorkloadBuild{Context: "./app"}}},
	})
	require.Equal(t, "deploy", got.Source.Subdir)
	require.Equal(t, "./app", got.Workloads[0].Build.Context)
}

// TestR020_AComposeContextAboveTheRepositoryIsBuiltFromItsRoot asserts R-020
// beside R-096: a compose file at the repository's root naming `..`, or an
// absolute path, as a context is built from inside the repository as it
// always was, and the spec says so rather than recording a path validation
// refuses.
func TestR020_AComposeContextAboveTheRepositoryIsBuiltFromItsRoot(t *testing.T) {
	got := detect.Assemble("app_1", spec.Source{Type: spec.SourceGit}, detect.Draft{
		Workloads: []spec.Workload{
			{Name: "web", Primary: true, Build: &spec.WorkloadBuild{Context: ".."}},
			{Name: "api", Build: &spec.WorkloadBuild{Context: "/srv/api"}},
			{Name: "worker", Build: &spec.WorkloadBuild{Context: "./worker"}},
		},
	})
	require.Empty(t, got.Source.Subdir)
	require.Equal(t, ".", got.Workloads[0].Build.Context)
	require.Equal(t, "srv/api", got.Workloads[1].Build.Context)
	require.Equal(t, "worker", got.Workloads[2].Build.Context)

	got = detect.Assemble("app_1", spec.Source{Subdir: "deploy"}, detect.Draft{
		Workloads: []spec.Workload{{Name: "web", Build: &spec.WorkloadBuild{Context: "../../.."}}},
	})
	require.Empty(t, got.Source.Subdir)
	require.Equal(t, ".", got.Workloads[0].Build.Context)
}
