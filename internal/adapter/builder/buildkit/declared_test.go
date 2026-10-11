package buildkit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// macscout with its Makefile deleted, which is the case this file exists for.
//
// Nothing left says "build the client first". Two files say it between them:
// the Vite config names its output directory, and the Go source embeds that
// same directory. Neither mentions the other.
func TestR094_AnEmbedDirectiveAndAClientOutputAreOneDeclaration(t *testing.T) {
	root := withFiles(t, map[string]string{
		"go.mod":                   "module example.com/app\n",
		"cmd/server/main.go":       "package main\n\nimport \"embed\"\n\n//go:embed all:dist\nvar dist embed.FS\n",
		"cmd/server/dist/.gitkeep": "",
		"web/package.json":         `{"name":"client","scripts":{"build":"tsc -b && vite build"}}`,
		"web/vite.config.ts":       "export default { build: { outDir: \"../cmd/server/dist\" } };\n",
	})

	d := readDeclaredBuild(root)
	require.Equal(t, rankEmbedded, d.Rank)
	require.Equal(t, "web/vite.config.ts", d.Source)
	require.Equal(t, "(cd web && npm ci && npm run build)", d.Before,
		"a subshell, so the cd does not outlive the client build")
	require.Empty(t, d.Build, "and what it comes before is Railpack's business, not ours")
	require.True(t, d.needsSecondPass())
	require.Contains(t, d.Packages, "node")

	// The second pass wraps rather than replaces.
	args := strings.Join(d.railpackEnv("go build -o out ./cmd/server"), " ")
	require.Contains(t, args, "RAILPACK_BUILD_CMD=(cd web && npm ci && npm run build) && go build -o out ./cmd/server")
}

// The pairing is the whole claim. Either half alone says nothing.
func TestAnEmbedWithoutAMatchingClientOutputIsNotADeclaration(t *testing.T) {
	t.Run("the client builds somewhere else", func(t *testing.T) {
		root := withFiles(t, map[string]string{
			"cmd/server/main.go": "package main\n\n//go:embed all:dist\nvar dist embed.FS\n",
			"web/package.json":   `{"scripts":{"build":"vite build"}}`,
			"web/vite.config.ts": "export default { build: { outDir: \"./public\" } };\n",
		})
		require.False(t, readDeclaredBuild(root).found())
	})

	t.Run("the client does not say where it builds", func(t *testing.T) {
		// An assumed output path that happens to match an embed is a
		// coincidence, not a declaration.
		root := withFiles(t, map[string]string{
			"cmd/server/main.go": "package main\n\n//go:embed all:dist\nvar dist embed.FS\n",
			"web/package.json":   `{"scripts":{"build":"vite build"}}`,
		})
		require.False(t, readDeclaredBuild(root).found())
	})

	t.Run("nothing embeds anything", func(t *testing.T) {
		root := withFiles(t, map[string]string{
			"cmd/server/main.go": "package main\n\nfunc main() {}\n",
			"web/package.json":   `{"scripts":{"build":"vite build"}}`,
			"web/vite.config.ts": "export default { build: { outDir: \"../cmd/server/dist\" } };\n",
		})
		require.False(t, readDeclaredBuild(root).found())
	})
}

// An embed directive resolves against the package that declares it, never the
// module root: one in cmd/server means cmd/server/dist.
func TestEmbedPathsResolveAgainstTheirPackage(t *testing.T) {
	root := withFiles(t, map[string]string{
		"cmd/server/main.go": "package main\n\n//go:embed all:dist\nvar dist embed.FS\n",
		"other/main.go":      "package other\n\n//go:embed assets\nvar a embed.FS\n",
	})
	targets := goEmbedTargets(root)
	require.True(t, targets["cmd/server/dist"])
	require.True(t, targets["other/assets"])
	require.False(t, targets["dist"], "not relative to the module root")
}

// A wildcard names a set of files, not a directory something builds into.
func TestAWildcardEmbedIsNotADirectoryToFill(t *testing.T) {
	root := withFiles(t, map[string]string{
		"main.go": "package main\n\n//go:embed templates/*.html\nvar t embed.FS\n",
	})
	require.Empty(t, goEmbedTargets(root))
}

// --- ranking ----------------------------------------------------------------

// R-094 is a ladder, and a repository that says the same thing twice is read
// from the higher rung.
func TestR094_TheHighestRungWins(t *testing.T) {
	files := map[string]string{
		"go.mod":   "module example.com/app\n",
		"Makefile": "build:\n\tgo build .\n\nstart:\n\t./app\n",
		"Procfile": "web: ./app\n",
		".github/workflows/ci.yml": "name: CI\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n" +
			"    steps:\n      - uses: actions/checkout@v4\n      - run: go build -o app .\n",
	}

	require.Equal(t, rankWorkflow, readDeclaredBuild(withFiles(t, files)).Rank,
		"what runs on every push outranks what was written down")

	delete(files, ".github/workflows/ci.yml")
	require.Equal(t, rankRunner, readDeclaredBuild(withFiles(t, files)).Rank)

	delete(files, "Makefile")
	require.Equal(t, rankProcfile, readDeclaredBuild(withFiles(t, files)).Rank,
		"a Procfile says how to run it and nothing about building it")

	delete(files, "Procfile")
	require.False(t, readDeclaredBuild(withFiles(t, files)).found(),
		"a bare Go module is convention-matching, which is the bottom rung")
}

func TestTheLadderIsOrderedAndScored(t *testing.T) {
	require.Greater(t, rankWorkflow.confidence(), rankRunner.confidence())
	require.Greater(t, rankRunner.confidence(), rankProcfile.confidence())
	require.Greater(t, rankProcfile.confidence(), rankEmbedded.confidence())

	// Above convention-matching, below a Dockerfile at the root. Those two
	// numbers live in internal/detect and are the fixed points this sits
	// between; R-027 stops them being imported, so they are written out.
	const conventionMatching, dockerfileAtRoot = 0.45, 0.92
	require.Greater(t, rankEmbedded.confidence(), conventionMatching)
	require.Less(t, rankWorkflow.confidence(), dockerfileAtRoot)
}

// A command that cannot survive the trip into a Dockerfile is declined, and the
// repository is planned by convention instead.
func TestACommandThatWouldNeedEscapingIsDeclined(t *testing.T) {
	for name, recipe := range map[string]string{
		"a quoted argument": "build:\n\tgo build -ldflags \"-X main.v=1\" .\n",
		"a shell variable":  "build:\n\tgo build -o $(BIN) .\n",
		"a backtick":        "build:\n\tgo build -o `cat name` .\n",
	} {
		t.Run(name, func(t *testing.T) {
			// The target exists, so the reading is attempted and then refused —
			// which is the difference between declining and not noticing.
			require.False(t, safeCommand(strings.SplitN(recipe, "\n\t", 2)[1]))
		})
	}
}
