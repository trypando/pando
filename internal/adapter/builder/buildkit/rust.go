package buildkit

import (
	"fmt"
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
		clean := path.Clean(strings.TrimSpace(member))
		if clean == "." || strings.HasPrefix(clean, "..") || path.IsAbs(clean) {
			continue
		}
		dirs, _ := filepath.Glob(filepath.Join(contextDir, filepath.FromSlash(clean)))
		for _, dir := range dirs {
			c, ok := readCargoToml(dir)
			if !ok || c.Package == nil || !safeCargoName(c.Package.Name) {
				continue
			}
			binary := ""
			switch {
			case len(c.Bin) == 1 && safeCargoName(c.Bin[0].Name):
				binary = c.Bin[0].Name
			case len(c.Bin) == 0 && fileExists(dir, "src/main.rs"):
				binary = c.Package.Name
			default:
				continue
			}
			found = append(found, rustWorkspace{Package: c.Package.Name, Binary: binary})
		}
	}
	if len(found) != 1 {
		return rustWorkspace{}, false
	}
	ws := found[0]
	ws.Version = rustToolchain(contextDir)
	return ws, true
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
