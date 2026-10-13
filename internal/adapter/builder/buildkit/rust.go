package buildkit

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// A Cargo workspace with no package at its root — a "virtual" workspace — and
// one program among its members is planned by Pando (issue #90).
//
// A planner that reads the root's Cargo.toml for the package to build finds
// none in a virtual workspace, and the member cannot be built from its own
// directory either: its path dependencies on the crates beside it reach
// outside it. Built from the root with `cargo build -p`, it compiles the way
// it does for its author.

type rustWorkspace struct {
	Version string // "1", or the release rust-toolchain names
	Package string // the member's package name
	Binary  string // the program cargo writes to target/release
}

// cargoToml is the part of a Cargo.toml read here.
type cargoToml struct {
	Package *struct {
		Name string `toml:"name"`
	} `toml:"package"`
	Workspace *struct {
		Members []string `toml:"members"`
	} `toml:"workspace"`
	Bin []struct {
		Name string `toml:"name"`
	} `toml:"bin"`
}

func readCargoToml(dir string) (cargoToml, bool) {
	var c cargoToml
	raw := readFile(dir, "Cargo.toml")
	if raw == "" || toml.Unmarshal([]byte(raw), &c) != nil {
		return cargoToml{}, false
	}
	return c, true
}

// readRustWorkspace reports a virtual Cargo workspace with exactly one member
// that builds a program.
func readRustWorkspace(contextDir string) (rustWorkspace, bool) {
	root, ok := readCargoToml(contextDir)
	if !ok || root.Package != nil || root.Workspace == nil {
		return rustWorkspace{}, false
	}
	var found []rustWorkspace
	for _, member := range root.Workspace.Members {
		found = append(found, rustMemberPrograms(contextDir, member)...)
	}
	if len(found) != 1 {
		return rustWorkspace{}, false
	}
	ws := found[0]
	ws.Version = rustToolchain(contextDir)
	return ws, true
}

// rustMemberPrograms are the programs built by the crates a workspace member
// entry (a path or a glob) names inside the workspace.
func rustMemberPrograms(contextDir, member string) []rustWorkspace {
	clean := path.Clean(strings.TrimSpace(member))
	if clean == "." || strings.HasPrefix(clean, "..") || path.IsAbs(clean) {
		return nil
	}
	var found []rustWorkspace
	dirs, _ := filepath.Glob(filepath.Join(contextDir, filepath.FromSlash(clean)))
	for _, dir := range dirs {
		if ws, ok := cargoProgram(dir); ok {
			found = append(found, ws)
		}
	}
	return found
}

// cargoProgram is the one program the crate in dir builds: its single [[bin]],
// or with none, src/main.rs under the package's name.
func cargoProgram(dir string) (rustWorkspace, bool) {
	c, ok := readCargoToml(dir)
	if !ok || c.Package == nil || !safeCargoName(c.Package.Name) {
		return rustWorkspace{}, false
	}
	switch {
	case len(c.Bin) == 1 && safeCargoName(c.Bin[0].Name):
		return rustWorkspace{Package: c.Package.Name, Binary: c.Bin[0].Name}, true
	case len(c.Bin) == 0 && fileExists(dir, "src/main.rs"):
		return rustWorkspace{Package: c.Package.Name, Binary: c.Package.Name}, true
	}
	return rustWorkspace{}, false
}

var (
	cargoName        = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	toolchainChannel = regexp.MustCompile(`(?m)^\s*channel\s*=\s*"(\d+\.\d+(?:\.\d+)?)"`)
)

func safeCargoName(name string) bool { return cargoName.MatchString(name) }

// rustToolchain is the release rust-toolchain(.toml) pins, or "1": the newest
// stable 1.x, which is what a workspace that pins nothing builds with.
func rustToolchain(contextDir string) string {
	if m := toolchainChannel.FindStringSubmatch(readFile(contextDir, "rust-toolchain.toml")); m != nil && safeVersion(m[1]) {
		return m[1]
	}
	if v := strings.TrimSpace(readFile(contextDir, "rust-toolchain")); v != "" && safeVersion(v) && strings.Contains(v, ".") {
		return v
	}
	return "1"
}

// A crate whose path dependencies reach above it: axum's examples depend on
// `axum = { path = "../../axum" }`. Deployed from its own directory, the
// build could not read what it depends on ("failed to read /axum/Cargo.toml",
// issue #90). The crate is built from the directory its dependencies reach,
// by its manifest, and run from its own files.

// rustPathCrate is such a crate.
type rustPathCrate struct {
	Version string // the Rust release, as readRustWorkspace's
	Dir     string // the crate's directory, from the directory the build reads
	Up      int    // how many directories above the crate that directory is
	Binary  string // the program cargo writes
}

// Context is the directory the crate is built from, relative to the crate.
func (c rustPathCrate) Context() string {
	return strings.TrimSuffix(strings.Repeat("../", c.Up), "/")
}

var cargoPathDependency = regexp.MustCompile(`\bpath\s*=\s*"([^"]+)"`)

// readRustPathCrate reports a crate with one program whose path dependencies
// reach above its directory, to crates that are there.
func readRustPathCrate(contextDir string) (rustPathCrate, bool) {
	program, ok := cargoProgram(contextDir)
	if !ok {
		return rustPathCrate{}, false
	}
	up := 0
	for _, m := range cargoPathDependency.FindAllStringSubmatch(readFile(contextDir, "Cargo.toml"), -1) {
		n := 0
		for _, part := range strings.Split(path.Clean(m[1]), "/") {
			if part != ".." {
				break
			}
			n++
		}
		if n <= up {
			continue
		}
		// Above the crate, which fileExists does not reach: the checkout
		// holds the whole repository, and the dependency must be in it.
		if info, err := os.Stat(filepath.Join(contextDir, filepath.FromSlash(path.Clean(m[1])), "Cargo.toml")); err == nil && !info.IsDir() {
			up = n
		}
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(contextDir)), "/")
	if up == 0 || up >= len(parts) {
		return rustPathCrate{}, false
	}
	dir := strings.Join(parts[len(parts)-up:], "/")
	if !safeRelative(dir) {
		return rustPathCrate{}, false
	}
	root := filepath.Join(contextDir, filepath.FromSlash(strings.Repeat("../", up)))
	return rustPathCrate{Version: rustToolchain(root), Dir: dir, Up: up, Binary: program.Binary}, true
}

// writeRustPathCratePlan builds the crate by its manifest from the directory
// its dependencies reach, and runs it from the crate's own files.
func writeRustPathCratePlan(contextDir string, c rustPathCrate) (string, error) {
	content := fmt.Sprintf(`# A Rust program, %[3]s, in %[2]s, whose path dependencies are above it. Built
# from there, by its manifest, with Rust %[1]s.
FROM rust:%[1]s-bookworm AS build
WORKDIR /src
COPY . .
RUN cargo build --release --manifest-path %[2]s/Cargo.toml --target-dir /src/.pando-target

FROM %[4]s
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY %[2]s/ .
COPY --from=build /src/.pando-target/release/%[3]s /usr/local/bin/app
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec app"]
`, c.Version, c.Dir, c.Binary, programRuntimeImage)
	return writePlan(contextDir, content)
}

// writeRustWorkspacePlan builds the member from the workspace root and runs
// it from the repository's files, as the Go plan does.
func writeRustWorkspacePlan(contextDir string, ws rustWorkspace) (string, error) {
	content := fmt.Sprintf(`# A Rust program, %[2]s, in a Cargo workspace, built with Rust %[1]s.
FROM rust:%[1]s-bookworm AS build
WORKDIR /src
COPY . .
RUN cargo build --release -p %[2]s

FROM %[4]s
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY . .
COPY --from=build /src/target/release/%[3]s /usr/local/bin/app
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec app"]
`, ws.Version, ws.Package, ws.Binary, programRuntimeImage)
	return writePlan(contextDir, content)
}
