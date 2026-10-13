package buildkit

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR095_PlansForWhatTheBenchmarkFoundMissing asserts R-095: where Pando
// writes a language's plan, the plan builds the app the way its own tooling
// documents (issue #90, second round).
func TestR095_PlansForWhatTheBenchmarkFoundMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	// Shiny's two-file layout, whose files never name Shiny: RStudio's
	// k-means example calls pageWithSidebar with no library(shiny).
	shiny := planInDir(t, writeTree(t, map[string]string{
		"ui.R":     "pageWithSidebar(headerPanel('Iris k-means clustering'))\n",
		"server.R": "function(input, output, session) { kmeans(iris, 3) }\n",
	}))
	require.Contains(t, shiny, "install2.r --error --skipinstalled shiny\n")
	require.Contains(t, shiny, "EXPOSE 3838")
	_, ok := rPlan(writeTree(t, map[string]string{"app.R": "print('hello')\n"}))
	require.False(t, ok, "an app.R that never names Shiny is any R script")

	// The modules a cpanfile requires, at the versions its snapshot records;
	// what they depend on is left to cpanm.
	perl := planInDir(t, writeTree(t, map[string]string{
		"cpanfile": "requires 'YAML' => '0';\nrequires 'Dancer2' => '0.158000';\nrequires 'List::Util' => '0';\n" +
			"test_requires 'Test::More' => '0';\n",
		"cpanfile.snapshot": "# carton snapshot format: version 1.0\nDISTRIBUTIONS\n" +
			"  Dancer2-0.158000\n    pathname: X/XS/XSAWYERX/Dancer2-0.158000.tar.gz\n    provides:\n      Dancer2 0.158000\n      Dancer2::Core 0.158000\n" +
			"  Scalar-List-Utils-1.42\n    provides:\n      List::Util 1.42\n" +
			"  Template-Toolkit-2.26\n    provides:\n      Template 2.26\n      YAML 0\n" +
			"  YAML-1.13\n    provides:\n      YAML 1.13\n",
		"app.psgi": "use Dancer2;\n",
	}))
	require.Contains(t, perl, "RUN cpanm --notest YAML@1.13 Dancer2@0.158000\nRUN cpanm --notest --installdeps .\n",
		"List::Util comes from a distribution named otherwise, and is left to cpanm")
	require.NotContains(t, planInDir(t, writeTree(t, map[string]string{"cpanfile": "requires 'Dancer2';\n", "app.psgi": ""})), "@")

	// GHC before 9.2 compiles through LLVM on 64-bit Arm.
	stack := planInDir(t, writeTree(t, map[string]string{
		"stack.yaml": "resolver: lts-18.3\n", "package.yaml": "name: x\nexecutables:\n  example-servant-minimal:\n    main: Main.hs\n",
	}))
	require.Contains(t, stack, `if [ "$(uname -m)" = aarch64 ]`)
	require.Contains(t, stack, "llvm-13")
	require.Less(t, strings.Index(stack, "llvm-13"), strings.Index(stack, "stack build"))

	// A Go module whose main packages are a server and a client builds
	// neither: nothing says which is the app, and R-102 does not pick one by
	// its directory's name.
	_, ok := readGoBuild(writeTree(t, map[string]string{
		"go.mod":           "module github.com/gin-gonic/examples/websocket\ngo 1.21\n",
		"server/server.go": "package main\n", "client/client.go": "package main\n",
	}))
	require.False(t, ok)
}

// TestR094_ACrateWhosePathDependenciesAreAboveItIsBuiltFromThere asserts
// R-094 tier 4 for a crate deployed from a subdirectory whose Cargo.toml
// reaches above it: axum's examples depend on `path = "../../axum"`. The plan
// builds from the directory the dependency is in, by the crate's manifest,
// and says so, so the app's directory moves up to it (issue #90).
func TestR094_ACrateWhosePathDependenciesAreAboveItIsBuiltFromThere(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	repo := writeTree(t, map[string]string{
		"axum/Cargo.toml":                  "[package]\nname = \"axum\"\n",
		"examples/hello-world/Cargo.toml":  "[package]\nname = \"example-hello-world\"\n\n[dependencies]\naxum = { path = \"../../axum\" }\ntokio = \"1\"\n",
		"examples/hello-world/src/main.rs": "fn main() {}\n",
		"examples/other/Cargo.toml":        "[package]\nname = \"other\"\n[dependencies]\nlocal = { path = \"local\" }\n",
		"examples/other/local/Cargo.toml":  "[package]\nname = \"local\"\n",
		"examples/other/src/main.rs":       "fn main() {}\n",
	})
	crate := filepath.Join(repo, "examples", "hello-world")
	plan, err := New().Plan(context.Background(), view{crate})
	require.NoError(t, err)
	require.Equal(t, "../..", plan.Context)
	body := plan.Files[plan.Entry]
	require.Contains(t, body, "RUN cargo build --release --manifest-path examples/hello-world/Cargo.toml --target-dir /src/.pando-target")
	require.Contains(t, body, "COPY examples/hello-world/ .")
	require.Contains(t, body, "COPY --from=build /src/.pando-target/release/example-hello-world /usr/local/bin/app")

	_, ok := readRustPathCrate(filepath.Join(repo, "examples", "other"))
	require.False(t, ok, "a path dependency inside the crate's own directory needs nothing above it")
}
