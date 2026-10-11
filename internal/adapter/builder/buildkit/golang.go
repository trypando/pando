package buildkit

import (
	"bufio"
	"fmt"
	"os"
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
	if declared := readDeclaredBuild(contextDir); mod == "" || declared.Build != "" || declared.Before != "" {
		return goBuild{}, false
	}
	m := goDirective.FindStringSubmatch(mod)
	if m == nil {
		return goBuild{}, false
	}

	if dirHasMain(contextDir) {
		return goBuild{Version: m[1], Package: "."}, true
	}
	var mains []string
	dirs, _ := filepath.Glob(filepath.Join(contextDir, "cmd", "*"))
	for _, d := range dirs {
		if info, err := os.Stat(d); err == nil && info.IsDir() && dirHasMain(d) {
			mains = append(mains, "./cmd/"+filepath.Base(d))
		}
	}
	sort.Strings(mains)
	if len(mains) != 1 || !safeRelative(strings.TrimPrefix(mains[0], "./")) {
		// None to build, or several and nothing saying which is the server.
		return goBuild{}, false
	}
	return goBuild{Version: m[1], Package: mains[0]}, true
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
	content := fmt.Sprintf(`# A Go program, built with Go %[1]s.
FROM golang:%[1]s AS build
WORKDIR /src
COPY . .
RUN go mod download
RUN go build -o /out/app %[2]s

FROM %[3]s
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY . .
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec app"]
`, b.Version, b.Package, programRuntimeImage)
	return writePlan(contextDir, content)
}
