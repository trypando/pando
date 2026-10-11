package buildkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// flagsFor is what Railpack is told for a repository, through whichever
// declaration ranks highest. The tests below assert the same observable thing
// they always did: the settings that come out the far end.
func flagsFor(root string) []string {
	return readDeclaredBuild(root).railpackEnv("")
}

func withFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
	}
	return root
}

// macscout's Makefile, which is the repository that made this necessary: a Go
// module at the root whose build compiles a React client into cmd/server/dist
// and then embeds it. Convention-matching plans `go build` and ships a binary
// that starts, reports healthy and serves an empty page.
const macscoutMakefile = `.PHONY: dev build run test clean

dev:
	@echo "Run in two terminals:"

run-api:
	go run ./cmd/server

build:
	cd web && npm ci && npm run build
	touch cmd/server/dist/.gitkeep
	go build -o macscout ./cmd/server

run: build
	./macscout

test:
	go test ./...

clean:
	rm -f macscout
`

// R-094 tier 3: the maintainer's own build commands outrank convention.
func TestR094_AMakefileBuildBecomesTheBuildCommand(t *testing.T) {
	root := withFiles(t, map[string]string{
		"Makefile":                 macscoutMakefile,
		"go.mod":                   "module example.com/app\n",
		"web/package.json":         `{"name":"client"}`,
		"cmd/server/dist/.gitkeep": "",
	})

	args := flagsFor(root)
	joined := strings.Join(args, " ")

	require.Contains(t, joined, "RAILPACK_BUILD_CMD=make build",
		"the repository says how it is built, so the plan runs that")
	require.Contains(t, joined, "RAILPACK_START_CMD=./macscout",
		"and the run target says what starts it")

	// make is in Railpack's builder image, and the Go provider has no reason
	// to install Node — but this Makefile's build runs npm.
	require.Contains(t, joined, "RAILPACK_PACKAGES=node",
		"a package.json under web/ is the client declaring itself")
}

// A Makefile that says nothing about building leaves planning alone.
func TestAMakefileWithoutABuildTargetChangesNothing(t *testing.T) {
	root := withFiles(t, map[string]string{
		"Makefile": ".PHONY: lint release\n\nlint:\n\tgolangci-lint run\n\nrelease:\n\tgoreleaser release\n",
		"go.mod":   "module example.com/app\n",
	})
	require.Empty(t, flagsFor(root),
		"lint and release helpers are not a deployment instruction")
}

func TestNoMakefileChangesNothing(t *testing.T) {
	require.Empty(t, flagsFor(withFiles(t, map[string]string{"go.mod": "module x\n"})))
}

func TestTheStartCommandIsReadFromTheRunTarget(t *testing.T) {
	for name, tc := range map[string]struct {
		makefile string
		want     string
	}{
		"run target after a dependency": {
			makefile: "build:\n\tgo build -o app .\n\nrun: build\n\t./app\n",
			want:     "./app",
		},
		"start is preferred over run": {
			// `run` is usually the development one. Deploying somebody's
			// file-watching dev server is a bad way to find that out.
			makefile: "build:\n\tgo build .\n\nrun:\n\t./app --dev --watch\n\nstart:\n\t./app\n",
			want:     "./app",
		},
		"a target that only delegates is not an answer": {
			makefile: "build:\n\tgo build .\n\nrun:\n\tmake build\n",
			want:     "",
		},
		"a shell pipeline is left alone": {
			// Which half is the app? Guessing produces a container that runs
			// the wrong one.
			makefile: "build:\n\tgo build .\n\nrun:\n\tcd web && npm run dev\n",
			want:     "",
		},
		"an echo is not a start command": {
			makefile: "build:\n\tgo build .\n\nrun:\n\techo hello\n",
			want:     "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, parseMakefile(strings.NewReader(tc.makefile)).StartCommand)
		})
	}
}

func TestTheBuildTargetIsFoundInPreferenceOrder(t *testing.T) {
	for name, tc := range map[string]struct {
		makefile string
		want     string
	}{
		"build wins":                 {makefile: "all:\n\ttrue\n\nbuild:\n\ttrue\n", want: "build"},
		"all when there is no build": {makefile: "all:\n\ttrue\n", want: "all"},
		"compile":                    {makefile: "compile:\n\ttrue\n", want: "compile"},
		"none of them":               {makefile: "lint:\n\ttrue\n", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, parseMakefile(strings.NewReader(tc.makefile)).BuildTarget)
		})
	}
}

// Variables, directives and comments are not targets.
func TestTheParserIsNotFooledByThingsThatLookLikeTargets(t *testing.T) {
	d := parseMakefile(strings.NewReader(
		"# build: this is a comment\n" +
			"GO ?= go\n" +
			"BIN := bin/app\n" +
			".PHONY: build\n" +
			"include other.mk\n" +
			"build:\n\t$(GO) build -o $(BIN)\n"))
	require.Equal(t, "build", d.BuildTarget)
	require.Empty(t, d.StartCommand)
}

// A recipe line belongs to the target above it, and a non-indented line ends it.
func TestARecipeEndsAtTheNextUnindentedLine(t *testing.T) {
	d := parseMakefile(strings.NewReader(
		"build:\n\tgo build .\n\nVERSION = 1\n\nstart:\n\t./app\n"))
	require.Equal(t, "build", d.BuildTarget)
	require.Equal(t, "./app", d.StartCommand)
}

func TestGNUmakefileIsPreferredTheWayMakePrefersIt(t *testing.T) {
	root := withFiles(t, map[string]string{
		"GNUmakefile": "build:\n\ttrue\n\nstart:\n\t./from-gnumakefile\n",
		"Makefile":    "build:\n\ttrue\n\nstart:\n\t./from-makefile\n",
	})
	d, _ := readMakefileAt(root)
	require.Equal(t, "GNUmakefile", d.File)
	require.Equal(t, "./from-gnumakefile", d.StartCommand)
}

func TestNodeIsAddedOnlyWhenAPackageJsonExists(t *testing.T) {
	withMake := map[string]string{"Makefile": "build:\n\tgo build .\n"}
	require.NotContains(t, strings.Join(flagsFor(withFiles(t, withMake)), " "), "RAILPACK_PACKAGES=node")

	withNode := map[string]string{
		"Makefile":         "build:\n\tgo build .\n",
		"web/package.json": "{}",
	}
	require.Contains(t, strings.Join(flagsFor(withFiles(t, withNode)), " "), "RAILPACK_PACKAGES=node")
}

// A Makefile over C sources needs a compiler, which the nixpacks environment
// lacked (issue #55). Railpack's builder image carries build-essential, so
// nothing is asked for: an unknown mise package fails the plan outright.
func TestNoPackageIsAskedForTheCompilerOrMake(t *testing.T) {
	root := withFiles(t, map[string]string{"Makefile": "build:\n\tcc -o app main.c\n", "main.c": "int main(){}"})
	require.NotContains(t, strings.Join(flagsFor(root), " "), "RAILPACK_PACKAGES")
}

// node_modules is somebody else's package.json, several thousand times over.
func TestAVendoredPackageJsonDoesNotCount(t *testing.T) {
	root := withFiles(t, map[string]string{
		"Makefile":                           "build:\n\tgo build .\n",
		"node_modules/left-pad/package.json": "{}",
		"vendor/thing/package.json":          "{}",
	})
	require.NotContains(t, strings.Join(flagsFor(root), " "), "RAILPACK_PACKAGES=node")
}

// TestNodeIsNotAddedWhenTheRepositoryItselfIsNode asserts the narrower rule.
//
// Railpack reads a root package.json itself, so its Node provider already
// brings the toolchain, at the version the package names. Naming it again
// would ask mise for the newest Node instead. (Under nixpacks it made the
// plan unbuildable outright: "undefined variable 'npm-9_x'".)
func TestNodeIsNotAddedWhenTheRepositoryItselfIsNode(t *testing.T) {
	root := withFiles(t, map[string]string{
		"package.json": `{"name":"site","scripts":{"build":"vite build"}}`,
		".github/workflows/ci.yml": "name: build\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n" +
			"    steps:\n      - run: npm ci\n      - run: npm run build\n",
	})
	require.NotContains(t, strings.Join(flagsFor(root), " "), "RAILPACK_PACKAGES=node",
		"Railpack brings the Node the package asks for")
}
