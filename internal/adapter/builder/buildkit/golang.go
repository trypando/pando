package buildkit

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Go programs are planned by Pando rather than by Railpack, when nothing in the
// repository declares its own build.
//
// nixpacks installed Go from its pinned Nix package set, and a module naming a
// newer Go than that set sent it to download a toolchain it could not find
// ("go: download go1.27 for linux/arm64: toolchain not available", issue #55).
// The official Go image exists for every release. It also builds with cgo, which
// a SQLite driver needs and nixpacks' static build turned off ("Binary was
// compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work").
//
// A repository that says how it builds — a Makefile target, a CI workflow, a
// client embedded with go:embed — is left to the declared-build path, which
// knows how to run those steps; this plan only compiles a main package.

type goBuild struct {
	Version string // "1.27"
	Package string // "." or "./cmd/server"

	// Workspace is a program in a Go workspace (go.work): one module of
	// several, built from the workspace root so the modules it uses resolve
	// to the ones beside it. `go mod download` has no single module to read
	// there, and `go build` fetches what it needs.
	Workspace bool
}

var (
	goDirective   = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)
	goMainPackage = regexp.MustCompile(`(?m)^package\s+main\b`)
)

// readGoBuild reports a Go module with one main package Pando knows how to build.
func readGoBuild(contextDir string) (goBuild, bool) {
	mod := readFile(contextDir, "go.mod")
	// A declared *build* — a Makefile target, a workflow step, a client that
	// builds first — is run by the declared-build path. A Procfile only says
	// how to start the program this plan compiles, and Heroku's Go sample, whose
	// Procfile names the binary its buildpack makes, stayed on nixpacks and its
	// toolchain failure because of it (issue #55).
	if declared := readDeclaredBuild(contextDir); declared.Build != "" || declared.Before != "" {
		return goBuild{}, false
	}
	if mod == "" {
		return readGoWorkBuild(contextDir)
	}
	m := goDirective.FindStringSubmatch(mod)
	if m == nil {
		return goBuild{}, false
	}

	if dirHasMain(contextDir) {
		return goBuild{Version: m[1], Package: "."}, true
	}
	pkg, ok := onlyMainPackage(contextDir)
	if !ok {
		return goBuild{}, false
	}
	return goBuild{Version: m[1], Package: pkg}, true
}

// onlyMainPackage is the one main package below a module's root: in cmd/*, or
// when cmd/ has none, in a directory of its own at the top (server/, api/).
// None, or several and nothing saying which is the server, is not ok.
func onlyMainPackage(moduleDir string) (string, bool) {
	for _, pattern := range []string{filepath.Join("cmd", "*"), "*"} {
		var mains []string
		dirs, _ := filepath.Glob(filepath.Join(moduleDir, pattern))
		for _, d := range dirs {
			rel, err := filepath.Rel(moduleDir, d)
			if err != nil || ignoredGoDir(filepath.Base(d)) {
				continue
			}
			if info, err := os.Stat(d); err == nil && info.IsDir() && dirHasMain(d) {
				mains = append(mains, "./"+filepath.ToSlash(rel))
			}
		}
		if len(mains) == 0 {
			continue
		}
		sort.Strings(mains)
		if len(mains) != 1 || !safeRelative(strings.TrimPrefix(mains[0], "./")) {
			return "", false
		}
		return mains[0], true
	}
	return "", false
}

// ignoredGoDir is a directory the go tool skips, or one that holds examples
// and tests rather than the program.
func ignoredGoDir(name string) bool {
	switch strings.ToLower(name) {
	case "testdata", "vendor", "example", "examples", "test", "tests":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

var (
	goWorkUse      = regexp.MustCompile(`(?m)^\s*use\s+"?([^\s"(]+)"?\s*$`)
	goWorkUseBlock = regexp.MustCompile(`(?ms)^\s*use\s*\((.*?)\)`)
)

// readGoWorkBuild reports a Go workspace with one program among its modules
// (issue #90). Built from the workspace root, the program's imports of the
// other modules resolve the way they do on its author's machine; built from
// its own directory, they do not resolve at all.
func readGoWorkBuild(contextDir string) (goBuild, bool) {
	work := readFile(contextDir, "go.work")
	m := goDirective.FindStringSubmatch(work)
	if m == nil {
		return goBuild{}, false
	}
	var uses []string
	for _, u := range goWorkUse.FindAllStringSubmatch(work, -1) {
		uses = append(uses, u[1])
	}
	for _, block := range goWorkUseBlock.FindAllStringSubmatch(work, -1) {
		for _, line := range strings.Split(block[1], "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			if line = strings.Trim(strings.TrimSpace(line), `"`); line != "" {
				uses = append(uses, line)
			}
		}
	}
	var programs []string
	for _, use := range uses {
		clean := path.Clean(use)
		if !safeRelative(clean) {
			continue
		}
		dir := filepath.Join(contextDir, filepath.FromSlash(clean))
		if dirHasMain(dir) {
			programs = append(programs, "./"+clean)
		} else if pkg, ok := onlyMainPackage(dir); ok {
			programs = append(programs, "./"+path.Join(clean, strings.TrimPrefix(pkg, "./")))
		}
	}
	if len(programs) != 1 {
		return goBuild{}, false
	}
	return goBuild{Version: m[1], Package: programs[0], Workspace: true}, true
}

func dirHasMain(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if fh, err := os.Open(f); err == nil {
			scanner := bufio.NewScanner(fh)
			for i := 0; i < 200 && scanner.Scan(); i++ {
				if goMainPackage.MatchString(scanner.Text()) {
					_ = fh.Close()
					return true
				}
			}
			_ = fh.Close()
		}
	}
	return false
}

// writeGoPlan compiles the main package on the Go release the module names and
// runs it from the repository's files, as Railpack does, so an app that reads
// its templates or static files from the working directory finds them.
func writeGoPlan(contextDir string, b goBuild) (string, error) {
	download := "RUN go mod download\n"
	if b.Workspace {
		download = ""
	}
	content := fmt.Sprintf(`# A Go program, built with Go %[1]s.
FROM golang:%[1]s AS build
WORKDIR /src
COPY . .
%[4]sRUN go build -o /out/app %[2]s

FROM %[3]s
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY . .
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec app"]
`, b.Version, b.Package, programRuntimeImage, download)
	return writePlan(contextDir, content)
}
