package buildkit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/trypando/pando/internal/errs"
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

	// Program and Start are the Procfile's: the name it runs the compiled
	// program by, and the command, when it runs the program this plan builds
	// or a file committed in the repository. Empty runs the program as it is.
	Program string
	Start   string
}

var (
	goDirective   = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)
	goMainPackage = regexp.MustCompile(`(?m)^package\s+main\b`)
	goModule      = regexp.MustCompile(`(?m)^module\s+"?([^\s"]+)"?`)
	goMajorSuffix = regexp.MustCompile(`^v[0-9]+$`)
	goProgramSafe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// goProgramName is the name `go build` and `go install` give the program in
// pkg: the module path's last element for the module's root, less a major
// version suffix (miniflux.app/v2 makes miniflux.app), and the directory's
// name for any other. It is the name a Heroku-style Procfile runs the program
// by, since that is what the Go buildpack installs it as.
func goProgramName(mod, pkg string) string {
	if pkg != "." {
		return path.Base(pkg)
	}
	m := goModule.FindStringSubmatch(mod)
	if m == nil {
		return ""
	}
	parts := strings.Split(m[1], "/")
	if len(parts) > 1 && goMajorSuffix.MatchString(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	return parts[len(parts)-1]
}

// procfileProgram reads the Procfile's command for a Go program named name.
//
// The Procfile is the author saying how the app starts, flags and all (R-094
// tier 2), and a plan that ignored it started a program that listens on
// 127.0.0.1 by default when the Procfile says `-addr 0.0.0.0:9090`, and was
// refused as listening only on loopback (issue #90). Its program is the one
// built here when it names it, bare or under ./ or bin/ where the Go buildpack
// puts it; command is then the Procfile's with that program by name. A file
// committed in the repository runs as it is. A program that is neither is
// something this plan does not build, and ok is false.
func procfileProgram(contextDir, name string) (command string, ok bool) {
	fields := strings.Fields(readProcfileBuild(contextDir).Start)
	if len(fields) == 0 || name == "" {
		return "", false
	}
	program := path.Clean(fields[0])
	switch {
	case fileExists(contextDir, program) && !strings.HasPrefix(program, ".."):
		return strings.Join(fields, " "), true
	case program == name, program == "bin/"+name:
		fields[0] = name
		return strings.Join(fields, " "), true
	}
	return "", false
}

// readGoBuild reports a Go module with one main package Pando knows how to build.
func readGoBuild(contextDir string) (goBuild, bool) {
	mod := readFile(contextDir, fileGoMod)
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

	b := goBuild{Version: m[1], Package: "."}
	if !dirHasMain(contextDir) {
		pkg, ok := onlyMainPackage(contextDir)
		if !ok {
			return goBuild{}, false
		}
		b.Package = pkg
	}
	if name := goProgramName(mod, b.Package); goProgramSafe.MatchString(name) {
		if command, ok := procfileProgram(contextDir, name); ok {
			b.Program, b.Start = name, command
		}
	}
	return b, true
}

// onlyMainPackage is the one main package below a module's root: in cmd/*, or
// when cmd/ has none, in a directory of its own at the top (server/, api/).
// None, or several and nothing saying which is the server, is not ok.
func onlyMainPackage(moduleDir string) (string, bool) {
	for _, pattern := range []string{filepath.Join("cmd", "*"), "*"} {
		mains := mainPackagesIn(moduleDir, pattern)
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

// mainPackagesIn are the main packages in the directories pattern matches
// under moduleDir, as ./-relative paths.
func mainPackagesIn(moduleDir, pattern string) []string {
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
	return mains
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
	var programs []string
	for _, use := range goWorkUses(work) {
		if program, ok := goWorkProgram(contextDir, use); ok {
			programs = append(programs, program)
		}
	}
	if len(programs) != 1 {
		return goBuild{}, false
	}
	return goBuild{Version: m[1], Package: programs[0], Workspace: true}, true
}

// goWorkUses are the module directories a go.work's use directives name, in
// both the one-line and the block form.
func goWorkUses(work string) []string {
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
	return uses
}

// goWorkProgram is the main package of the workspace module at use, as a
// path from the workspace root, and whether it has exactly one.
func goWorkProgram(contextDir, use string) (string, bool) {
	clean := path.Clean(use)
	if !safeRelative(clean) {
		return "", false
	}
	dir := filepath.Join(contextDir, filepath.FromSlash(clean))
	if dirHasMain(dir) {
		return "./" + clean, true
	}
	if pkg, ok := onlyMainPackage(dir); ok {
		return "./" + path.Join(clean, strings.TrimPrefix(pkg, "./")), true
	}
	return "", false
}

// withProcfileProgram makes a Railpack build of a Go module produce the program
// its Procfile runs.
//
// A Procfile written for Heroku names the program as the Go buildpack installs
// it, by its `go install` name. Railpack reads the Procfile and runs that name,
// and builds its program as ./out, or whatever a declared build writes:
// miniflux's Procfile runs `miniflux.app`, its Makefile builds `miniflux`, and
// the app exited 127 with "miniflux.app: command not found" (issue #90). So the
// build also compiles the package the Procfile names, into bin/, and the start
// command runs it from there. A Procfile whose program is committed in the
// repository, or is not one of the module's programs, is left as it is.
func withProcfileProgram(contextDir string, declared declaredBuild) declaredBuild {
	mod := readFile(contextDir, fileGoMod)
	procfile := readProcfileBuild(contextDir)
	fields := strings.Fields(procfile.Start)
	if mod == "" || len(fields) == 0 || (declared.Start != "" && declared.Start != procfile.Start) {
		return declared
	}
	if fileExists(contextDir, fields[0]) {
		return declared
	}
	program := strings.TrimPrefix(path.Clean(fields[0]), "bin/")
	if !goProgramSafe.MatchString(program) {
		return declared
	}
	pkg := ""
	for _, candidate := range []string{".", "./cmd/" + program, "./" + program} {
		dir := filepath.Join(contextDir, filepath.FromSlash(candidate))
		if goProgramName(mod, candidate) == program && dirHasMain(dir) {
			pkg = candidate
			break
		}
	}
	if pkg == "" {
		return declared
	}

	step := "go build -o bin/" + program + " " + pkg
	switch {
	case declared.Build != "":
		declared.Build += " && " + step
	case declared.Before != "":
		declared.Build, declared.Before = declared.Before+" && "+step, ""
	default:
		declared.Build = step
	}
	fields[0] = "/app/bin/" + program
	declared.Start = strings.Join(fields, " ")
	return declared
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

// goAlias also installs the program under the name `go install` gives it. A
// start command written for the program, as a README or the person asked
// gives one, runs it by that name: golang/example's helloserver starts as
// `helloserver -addr :8080`, and exited 127 with "helloserver: not found" when
// only `app` was installed (issue #90). A command the base image already has by
// that name is not shadowed.
func goAlias(contextDir string, b goBuild) string {
	name := goProgramName(readFile(contextDir, fileGoMod), b.Package)
	if b.Program != "" || name == "app" || !goProgramSafe.MatchString(name) {
		return ""
	}
	return fmt.Sprintf("RUN command -v %[1]s >/dev/null || ln -s app /usr/local/bin/%[1]s\n", name)
}

// writeGoPlan compiles the main package on the Go release the module names and
// runs it from the repository's files, as Railpack does, so an app that reads
// its templates or static files from the working directory finds them. A
// Procfile that runs the program is followed, with the program installed under
// the name it uses.
func writeGoPlan(contextDir string, b goBuild) (string, error) {
	download := "RUN go mod download\n"
	if b.Workspace {
		download = ""
	}
	install, start := goAlias(contextDir, b), `"exec app"`
	if b.Start != "" {
		// The Procfile was read with safeStart (procfile.go), which lets a
		// command read $PORT as Heroku's do; the plan runs it under sh -c.
		if !goProgramSafe.MatchString(b.Program) || !safeStart(b.Start) {
			return "", errs.Newf(errs.BuildFailed, "The Procfile's command %q is not one Pando can run as written.", b.Start)
		}
		install = fmt.Sprintf("COPY --from=build /out/app /usr/local/bin/%s\n", b.Program)
		command := b.Start
		if !strings.ContainsAny(command, ";&|") {
			command = "exec " + command
		}
		quoted, err := json.Marshal(command)
		if err != nil {
			return "", errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
		}
		start = string(quoted)
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
%[5]sENTRYPOINT ["/bin/sh", "-c"]
CMD [%[6]s]
`, b.Version, b.Package, programRuntimeImage, download, install, start)
	return writePlan(contextDir, content)
}
