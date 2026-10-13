package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR095_AGoWorkspaceProgramIsBuiltFromTheWorkspaceRoot asserts R-095: a
// program in a Go workspace imports the modules beside it, which resolve only
// from the go.work root (issue #90). There is no single module there for
// `go mod download` to read.
func TestR095_AGoWorkspaceProgramIsBuiltFromTheWorkspaceRoot(t *testing.T) {
	root := withFiles(t, map[string]string{
		"go.work":                     "go 1.23\n\nuse (\n\t./api\n\t./shared\n)\n",
		"api/go.mod":                  "module example.com/api\n\ngo 1.23\n",
		"api/main.go":                 "package main\n",
		"shared/go.mod":               "module example.com/shared\n\ngo 1.23\n",
		"shared/greeting/greeting.go": "package greeting\n",
	})
	b, ok := readGoBuild(root)
	require.True(t, ok)
	require.Equal(t, goBuild{Version: "1.23", Package: "./api", Workspace: true}, b)

	name, err := writeGoPlan(root, b)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "go build -o /out/app ./api")
	require.NotContains(t, string(body), "go mod download")

	two := withFiles(t, map[string]string{
		"go.work":        "go 1.23\nuse ./api\nuse ./worker\n",
		"api/go.mod":     "module a\ngo 1.23\n",
		"api/main.go":    "package main\n",
		"worker/go.mod":  "module w\ngo 1.23\n",
		"worker/main.go": "package main\n",
	})
	_, ok = readGoBuild(two)
	require.False(t, ok, "two programs, and nothing saying which is the app")
}

// TestR095_AGoModuleWhoseOnlyProgramIsInADirectoryOfItsOwnIsBuilt asserts
// R-095: the one main package of a module need not be under cmd/.
func TestR095_AGoModuleWhoseOnlyProgramIsInADirectoryOfItsOwnIsBuilt(t *testing.T) {
	b, ok := readGoBuild(withFiles(t, map[string]string{
		"go.mod": "module x\ngo 1.24\n", "server/main.go": "package main\n", "store/store.go": "package store\n",
		"examples/demo/main.go": "package main\n",
	}))
	require.True(t, ok)
	require.Equal(t, "./server", b.Package)

	// A server and a client: the server is the app (serverDirs, issue #90).
	b, ok = readGoBuild(withFiles(t, map[string]string{
		"go.mod": "module x\ngo 1.24\n", "server/main.go": "package main\n", "client/main.go": "package main\n",
	}))
	require.True(t, ok)
	require.Equal(t, "./server", b.Package)

	_, ok = readGoBuild(withFiles(t, map[string]string{
		"go.mod": "module x\ngo 1.24\n", "server/main.go": "package main\n", "api/main.go": "package main\n",
	}))
	require.False(t, ok, "two programs named as servers, and nothing saying which is the app")
}

// TestR095_ACargoWorkspaceWithOneProgramIsBuiltFromItsRoot asserts R-095: a
// virtual Cargo workspace has no package at its root for a planner to read,
// and its program's path dependencies on the crates beside it resolve only
// from the root (issue #90).
func TestR095_ACargoWorkspaceWithOneProgramIsBuiltFromItsRoot(t *testing.T) {
	root := withFiles(t, map[string]string{
		"Cargo.toml":                "[workspace]\nresolver = \"2\"\nmembers = [\"crates/core\", \"crates/server\"]\n",
		"crates/core/Cargo.toml":    "[package]\nname = \"greeter-core\"\nversion.workspace = true\n",
		"crates/core/src/lib.rs":    "",
		"crates/server/Cargo.toml":  "[package]\nname = \"greeter-server\"\n\n[dependencies]\ngreeter-core = { path = \"../core\" }\n",
		"crates/server/src/main.rs": "fn main() {}\n",
		"rust-toolchain.toml":       "[toolchain]\nchannel = \"1.82\"\n",
	})
	ws, ok := readRustWorkspace(root)
	require.True(t, ok)
	require.Equal(t, rustWorkspace{Version: "1.82", Package: "greeter-server", Binary: "greeter-server"}, ws)

	name, err := writeRustWorkspacePlan(root, ws)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "FROM rust:1.82-bookworm AS build")
	require.Contains(t, string(body), "cargo build --release -p greeter-server")
	require.Contains(t, string(body), "COPY --from=build /src/target/release/greeter-server /usr/local/bin/app")

	globbed := withFiles(t, map[string]string{
		"Cargo.toml":          "[workspace]\nmembers = [\"crates/*\"]\n",
		"crates/a/Cargo.toml": "[package]\nname = \"a\"\n\n[[bin]]\nname = \"a-server\"\npath = \"src/bin.rs\"\n",
		"crates/a/src/bin.rs": "fn main() {}\n",
		"crates/b/Cargo.toml": "[package]\nname = \"b\"\n",
		"crates/b/src/lib.rs": "",
	})
	ws, ok = readRustWorkspace(globbed)
	require.True(t, ok)
	require.Equal(t, rustWorkspace{Version: "1", Package: "a", Binary: "a-server"}, ws)

	for name, files := range map[string]map[string]string{
		"a package at the root": {
			"Cargo.toml": "[package]\nname = \"app\"\n\n[workspace]\nmembers = [\"x\"]\n", "src/main.rs": "",
		},
		"two programs": {
			"Cargo.toml":    "[workspace]\nmembers = [\"a\", \"b\"]\n",
			"a/Cargo.toml":  "[package]\nname = \"a\"\n",
			"a/src/main.rs": "",
			"b/Cargo.toml":  "[package]\nname = \"b\"\n",
			"b/src/main.rs": "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := readRustWorkspace(withFiles(t, files))
			require.False(t, ok)
		})
	}
}
