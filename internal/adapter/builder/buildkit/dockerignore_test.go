package buildkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// quarkusIgnore is every Quarkus quickstart's .dockerignore, written for its
// Dockerfiles that copy a jar built on the developer's machine.
const quarkusIgnore = "*\n!target/*-runner\n!target/*-runner.jar\n!target/lib/*\n!target/quarkus-app/\n"

// patterns is what an ignore file excludes: its lines that are not comments.
func patterns(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// TestR095_ARepositorysDockerignoreDoesNotApplyToPandosOwnBuild asserts R-095.
//
// Ten Quarkus quickstarts failed with "there is no POM in this directory": the
// repository's .dockerignore lets only target/ into the context, and Pando's
// Dockerfile, which builds from source, was given nothing to build (issue
// #90). BuildKit reads an ignore file beside the Dockerfile in place of the
// context's, so every Dockerfile Pando writes has one, and it ignores nothing.
func TestR095_ARepositorysDockerignoreDoesNotApplyToPandosOwnBuild(t *testing.T) {
	root := withFiles(t, map[string]string{
		"pom.xml":       "<project><packaging>quarkus</packaging></project>",
		".dockerignore": quarkusIgnore,
	})

	plan, err := New().Plan(t.Context(), view{root: root})
	require.NoError(t, err)
	require.Equal(t, ".pando-plan/Dockerfile", plan.Entry)
	ignore, ok := plan.Files[".pando-plan/Dockerfile.dockerignore"]
	require.True(t, ok, "the ignore file is part of the plan the spec carries")
	require.Empty(t, patterns(ignore))

	// Built from a fresh checkout, as a deploy does, and replayed from a spec
	// stored before the ignore file was written.
	for name, req := range map[string]api.BuildRequest{
		"planned at build time": {Strategy: spec.BuildBuildpack},
		"replayed from a spec": {
			Strategy: spec.BuildBuildpack, Dockerfile: ".pando-plan/Dockerfile",
			GeneratedFiles: map[string]string{".pando-plan/Dockerfile": plan.Files[".pando-plan/Dockerfile"]},
		},
	} {
		t.Run(name, func(t *testing.T) {
			checkout := withFiles(t, map[string]string{"pom.xml": "<project/>", ".dockerignore": quarkusIgnore})
			gen, err := synthesize(req, checkout)
			require.NoError(t, err)
			defer gen.Cleanup()
			body, err := os.ReadFile(filepath.Join(gen.Dir, gen.Name+".dockerignore"))
			require.NoError(t, err, "BuildKit looks for <Dockerfile>.dockerignore beside the Dockerfile")
			require.Empty(t, patterns(string(body)))
		})
	}

	// A committed site is served whatever the repository ignores.
	site := withFiles(t, map[string]string{"dist/index.html": "<h1>hi</h1>", ".dockerignore": "dist\n"})
	gen, err := synthesize(api.BuildRequest{Strategy: spec.BuildStatic, StaticDir: "dist"}, site)
	require.NoError(t, err)
	defer gen.Cleanup()
	body, err := os.ReadFile(filepath.Join(gen.Dir, gen.Name+".dockerignore"))
	require.NoError(t, err)
	require.Empty(t, patterns(string(body)))
}

// An ignore file the spec carries is the one used, and a plan Pando did not
// write as a Dockerfile in .pando-plan/ — a Railpack plan, or one stored under
// .nixpacks/ and replayed as it was built — gets none.
func TestOnlyPandosOwnDockerfilesGetAnIgnoreFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, planDir), 0o755))
	edited := filepath.Join(root, planDir, "Dockerfile.dockerignore")
	require.NoError(t, os.WriteFile(edited, []byte("node_modules\n"), 0o644))
	require.NoError(t, writePlanIgnore(root, ".pando-plan/Dockerfile"))
	body, err := os.ReadFile(edited)
	require.NoError(t, err)
	require.Equal(t, "node_modules\n", string(body))

	for _, name := range []string{".pando-plan/" + railpackPlanFile, ".nixpacks/Dockerfile"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755))
		require.NoError(t, writePlanIgnore(root, name))
		_, err := os.Stat(filepath.Join(root, name+".dockerignore"))
		require.True(t, os.IsNotExist(err), name)
	}
}
