package detect_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/builder/buildkit"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// TestR094_ADockerfileThatCopiesBuildOutputIsProposedWithTheBuildFirst
// asserts R-094 tier 2 with the real builder as planner: the Dockerfile still
// wins, and what it proposes builds the repository's own build first, with
// the reason in the evidence (issue #90).
func TestR094_ADockerfileThatCopiesBuildOutputIsProposedWithTheBuildFirst(t *testing.T) {
	src := checkoutOf(t, map[string]string{
		"Dockerfile":                         "FROM open-liberty\nCOPY src/main/liberty/config/ /config/\nCOPY target/*.war /config/apps/\nEXPOSE 9080\n",
		"pom.xml":                            "<project><packaging>war</packaging></project>",
		"src/main/liberty/config/server.xml": "<server/>",
	})
	result, err := detect.NewAuction(
		detect.DockerfileDetector{Planner: buildkit.New()},
		detect.BuildpackDetector{Planner: buildkit.New()},
	).Run(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, spec.BuildDockerfile, result.Winner.Strategy)
	build := result.Winner.Draft.Build
	require.Equal(t, ".pando-plan/Dockerfile", build.Dockerfile)
	require.Contains(t, build.GeneratedFiles[build.Dockerfile], "AS pando-prebuild")
	require.Contains(t, result.Winner.Evidence, "Dockerfile copies target/*.war, which the repository does not have until it is built, so Maven runs first")
	require.Equal(t, 9080, result.Winner.Draft.Workloads[0].Ports[0].Number, "the Dockerfile is still read for what it declares")

	plain, err := detect.NewAuction(detect.DockerfileDetector{Planner: buildkit.New()}).
		Run(context.Background(), checkoutOf(t, map[string]string{"Dockerfile": "FROM nginx\nCOPY index.html /usr/share/nginx/html/\n", "index.html": ""}))
	require.NoError(t, err)
	require.Equal(t, "Dockerfile", plain.Winner.Draft.Build.Dockerfile, "a Dockerfile that builds as it is, is built as it is")
	require.Empty(t, plain.Winner.Draft.Build.GeneratedFiles)
}

// TestR094_AnAppWhoseBuildReachesAboveItMovesUp asserts R-094 tier 4 for a
// crate in a subdirectory whose path dependencies are above it: the plan
// says where it builds from, and the assembled spec is built from there
// (issue #90).
func TestR094_AnAppWhoseBuildReachesAboveItMovesUp(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	s := detect.Assemble("app_x", spec.Source{Subdir: "examples/hello-world"}, detect.Draft{
		Build: spec.Build{Strategy: spec.BuildBuildpack, Context: "../..", Dockerfile: ".pando-plan/Dockerfile"},
	})
	require.Equal(t, "", s.Source.Subdir)
	require.Equal(t, ".", s.Build.Context)

	result, err := detect.NewAuction(detect.BuildpackDetector{Planner: buildkit.New()}).Run(context.Background(),
		subdirOf(t, map[string]string{
			"axum/Cargo.toml":                  "[package]\nname = \"axum\"\n",
			"examples/hello-world/Cargo.toml":  "[package]\nname = \"example-hello-world\"\n[dependencies]\naxum = { path = \"../../axum\" }\n",
			"examples/hello-world/src/main.rs": "fn main() {}\n",
		}, "examples/hello-world"))
	require.NoError(t, err)
	require.Equal(t, "../..", result.Winner.Draft.Build.Context)
}

// subdirOf writes files into a checkout on disk and views it from subdir.
func subdirOf(t *testing.T, files map[string]string, subdir string) api.SourceView {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	return (&source.Checkout{Dir: root}).View(subdir)
}

// TestR103_AShinyAppInTwoFilesIsReadWithoutAQuestion asserts R-103 for
// Shiny's ui.R and server.R layout, whose files need not name Shiny
// (issue #90).
func TestR103_AShinyAppInTwoFilesIsReadWithoutAQuestion(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	result, err := detect.NewAuction(detect.StaticDetector{}, detect.BuildpackDetector{Planner: buildkit.New()}).
		Run(context.Background(), checkoutOf(t, map[string]string{
			"ui.R": "pageWithSidebar(headerPanel('k-means'))\n", "server.R": "function(input, output) {}\n", "DESCRIPTION": "Type: Shiny\n",
		}))
	require.NoError(t, err)
	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
	require.Empty(t, detect.Asked(result.Questions))
}
