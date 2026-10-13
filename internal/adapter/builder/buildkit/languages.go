package buildkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Languages Railpack has no provider for, planned by Pando as Dockerfiles on
// each language's official image, the way Go, .NET and the JVM already are.
//
// Each one fell through to "no recognizable project layout" and a question
// nobody could answer (issue #90), or worse, to Railpack's static-file
// provider, which served a Crystal or Perl app's public/ directory with Caddy
// and answered every route with a 404. What each plan does is what the
// language's own tooling says to do — `shards build`, `lein uberjar`, `sbt
// stage`, `stack build`, `cpanm --installdeps`, `install2.r` — and the start
// command is the one the project declares (a Procfile, a deps.edn alias, a
// :main) or its framework's documented one.
//
// Each image is the official one for a language line, tagged by that line, as
// the Go and .NET plans' are. The app's files are in the working directory at
// run time, as Railpack leaves them: templates, config files and databases
// shipped beside the code are read from there.

// languagePlan writes the plan for a repository in one of these languages,
// and reports whether it was one.
func languagePlan(contextDir string) (string, bool, error) {
	for _, try := range []func(string) (string, bool){
		crystalPlan, rPlan, clojurePlan, sbtPlan, stackPlan, perlPlan, setupPyPlan, plainPHPPlan,
	} {
		if content, ok := try(contextDir); ok {
			name, err := writePlan(contextDir, content)
			return name, true, err
		}
	}
	return "", false, nil
}

// shellCMD is the Dockerfile lines that start command with a shell, as every
// plan Pando writes does, so $PORT is expanded when the container starts and
// an answered start command runs the same way.
func shellCMD(command string) string {
	quoted, _ := json.Marshal(command)
	return "ENTRYPOINT [\"/bin/sh\", \"-c\"]\nCMD [" + string(quoted) + "]\n"
}

// exposeLine declares the port a framework serves on by default, or nothing
// when it has none.
func exposeLine(port int) string {
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("EXPOSE %d\n", port)
}

// procfileStart is the Procfile's web process, when it is one a shell can be
// handed as it is.
func procfileStart(contextDir string) string {
	d := readProcfileBuild(contextDir)
	return d.Start
}

// --- Crystal ----------------------------------------------------------------

// crystalImage is the Crystal a shard is built and run on. [P] A current
// release line. Run on the same image, which has the libraries a Crystal
// program links against dynamically (libgc, libevent, libpcre2).
const crystalImage = "crystallang/crystal:1.14"

var (
	shardName    = regexp.MustCompile(`(?m)^name:\s*["']?([A-Za-z0-9_\-]+)`)
	shardTargets = regexp.MustCompile(`(?m)^targets:\s*\n((?:[ \t]+.*\n?|\s*\n)*)`)
	shardTarget  = regexp.MustCompile(`(?m)^  ([A-Za-z0-9_\-]+):\s*$`)
)

// crystalPlan builds a shard's first target with `shards build`, or its
// src/<name>.cr when it declares none.
func crystalPlan(contextDir string) (string, bool) {
	shard := readFile(contextDir, "shard.yml")
	if shard == "" {
		return "", false
	}
	var target, build string
	if m := shardTargets.FindStringSubmatch(shard); m != nil {
		if t := shardTarget.FindStringSubmatch(m[1]); t != nil {
			target = t[1]
			build = "shards build " + target + " --release --production"
		}
	}
	if target == "" {
		m := shardName.FindStringSubmatch(shard)
		if m == nil {
			return "", false
		}
		main := "src/" + strings.ReplaceAll(m[1], "-", "_") + ".cr"
		if !fileExists(contextDir, main) {
			return "", false
		}
		target = m[1]
		build = "mkdir -p bin && crystal build --release " + main + " -o bin/" + target
	}
	packages := ""
	// The SQLite shard links the system library.
	if strings.Contains(shard, "sqlite3") {
		packages = "RUN apt-get update && apt-get install -y --no-install-recommends libsqlite3-dev && rm -rf /var/lib/apt/lists/*\n"
	}
	port := 0
	if strings.Contains(shard, "kemal") {
		port = 3000 // Kemal's default
	}
	start := procfileStart(contextDir)
	if start == "" {
		start = "exec ./bin/" + target
	}
	return fmt.Sprintf(`# A Crystal shard, built with shards.
FROM %s
%sWORKDIR /app
COPY . .
RUN shards install --production
RUN %s
%s%s`, crystalImage, packages, build, exposeLine(port), shellCMD(start)), true
}

// --- R ----------------------------------------------------------------------

// rImage is the R an app runs on. [P] Rocker's versioned R, whose package
// repository serves prebuilt binaries for its Ubuntu base, so installing a
// package takes seconds rather than a compile.
const rImage = "rocker/r-ver:4.5"

var (
	rLibrary   = regexp.MustCompile(`(?:library|require|requireNamespace)\(\s*["']?([A-Za-z][A-Za-z0-9.]*)["']?`)
	rNamespace = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9.]*)::`)
)

// rBase are the packages every R installation has.
var rBase = map[string]bool{
	"base": true, "compiler": true, "datasets": true, "graphics": true, "grDevices": true, "grid": true,
	"methods": true, "parallel": true, "splines": true, "stats": true, "stats4": true, "tcltk": true,
	"tools": true, "utils": true,
}

// rPlan serves a plumber API (plumber.R) or a Shiny app (app.R, or ui.R and
// server.R) on every interface, on $PORT.
func rPlan(contextDir string) (string, bool) {
	var kind, port, start string
	switch {
	case fileExists(contextDir, "plumber.R"):
		kind, port = "plumber", "8000"
		start = `R -e "plumber::pr_run(plumber::pr('plumber.R'), host = '0.0.0.0', port = as.integer(Sys.getenv('PORT', '8000')))"`
	case fileExists(contextDir, "app.R") || (fileExists(contextDir, "ui.R") && fileExists(contextDir, "server.R")):
		kind, port = "shiny", "3838"
		start = `R -e "shiny::runApp('.', host = '0.0.0.0', port = as.integer(Sys.getenv('PORT', '3838')))"`
	default:
		return "", false
	}
	if kind == "shiny" && !strings.Contains(rSources(contextDir), "shiny") {
		return "", false
	}

	install := "RUN install2.r --error --skipinstalled " + strings.Join(rPackages(contextDir, kind), " ") + "\n"
	if fileExists(contextDir, "renv.lock") {
		// The project's own lockfile, restored as it says.
		install = "RUN install2.r --error renv && R -e \"renv::restore(prompt = FALSE)\"\n"
	}
	if p := procfileStart(contextDir); p != "" {
		start = p
	}
	return fmt.Sprintf(`# An R %[1]s app.
FROM %[2]s
RUN apt-get update && apt-get install -y --no-install-recommends libsodium-dev libcurl4-openssl-dev libssl-dev libxml2-dev zlib1g-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY . .
%[3]sEXPOSE %[4]s
%[5]s`, kind, rImage, install, port, shellCMD(start)), true
}

// rSources is every R file at the top of the app and in R/, together.
func rSources(contextDir string) string {
	var b strings.Builder
	for _, pattern := range []string{"*.R", "*.r", "R/*.R"} {
		files, _ := filepath.Glob(filepath.Join(contextDir, pattern))
		for _, f := range files {
			body, err := os.ReadFile(f) //nolint:gosec // G304: a file in the checkout, read to plan its build.
			if err == nil && len(body) < 1<<20 {
				b.Write(body)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// rPackages are the packages the app's R files load, and the framework's.
func rPackages(contextDir, framework string) []string {
	seen := map[string]bool{framework: true}
	pkgs := []string{framework}
	source := rSources(contextDir)
	for _, pattern := range []*regexp.Regexp{rLibrary, rNamespace} {
		for _, m := range pattern.FindAllStringSubmatch(source, -1) {
			name := m[1]
			if rBase[name] || seen[name] || len(name) > 64 {
				continue
			}
			seen[name] = true
			pkgs = append(pkgs, name)
		}
	}
	sort.Strings(pkgs[1:])
	return pkgs
}

// --- Clojure ----------------------------------------------------------------

// The images a Clojure project is built and run with. [P] Temurin 21, the
// current LTS, as the JVM plans use.
const (
	leinImage      = "clojure:temurin-21-lein"
	toolsDepsImage = "clojure:temurin-21-tools-deps"
)

var (
	leinMain        = regexp.MustCompile(`(?m):main\s+(?:\^:[A-Za-z\-]+\s+)?([A-Za-z][A-Za-z0-9.\-_*+!?]*)`)
	leinUberjarName = regexp.MustCompile(`:uberjar-name\s+"([A-Za-z0-9._\-]+\.jar)"`)
	cljMainOpts     = regexp.MustCompile(`(?s):([A-Za-z][A-Za-z0-9\-_]*)\s*\{[^{}]*:main-opts\s*\[\s*"-m"\s*"([A-Za-z][A-Za-z0-9.\-_]*)"`)
	cljNamespace    = regexp.MustCompile(`\(ns\s+([A-Za-z][A-Za-z0-9.\-_]*)`)
	cljMainFn       = regexp.MustCompile(`\(defn-?\s+-main\b`)
)

// clojurePlan builds a Leiningen project into an uberjar, or prepares a
// deps.edn project's dependencies and runs its main namespace.
func clojurePlan(contextDir string) (string, bool) {
	if project := readFile(contextDir, "project.clj"); project != "" {
		return leinPlan(contextDir, project)
	}
	if deps := readFile(contextDir, "deps.edn"); deps != "" {
		return depsPlan(contextDir, deps)
	}
	return "", false
}

// leinPlan: `lein uberjar` for a project with a :main, which Leiningen
// compiles ahead of time into the jar's entry point, and that jar run. A
// Procfile's command is run instead when there is one, after the same build.
func leinPlan(contextDir, project string) (string, bool) {
	start := procfileStart(contextDir)
	main := leinMain.FindStringSubmatch(project)
	if main == nil && start == "" {
		return "", false
	}
	build := "RUN lein deps\n"
	if main != nil {
		// Wherever this Leiningen puts it: target/ or target/uberjar/, under
		// :uberjar-name or the project's standalone name.
		find := "target/uberjar/*-standalone.jar target/*-standalone.jar"
		if m := leinUberjarName.FindStringSubmatch(project); m != nil {
			find = "target/uberjar/" + m[1] + " target/" + m[1] + " " + find
		}
		build = "RUN lein uberjar && cp \"$(ls -1 " + find + " 2>/dev/null | head -n 1)\" /app.jar\n"
		if start == "" {
			start = "exec java -jar /app.jar"
			if !leinMainCompiled(contextDir, project, main[1]) {
				start = "exec java -cp /app.jar clojure.main -m " + main[1]
			}
		}
	}
	return fmt.Sprintf(`# A Leiningen project.
FROM %s
WORKDIR /app
COPY . .
%s%s`, leinImage, build, shellCMD(start)), true
}

var (
	leinSkipAOT = regexp.MustCompile(`:main\s+\^:skip-aot\b`)
	cljGenClass = regexp.MustCompile(`\(:gen-class\b`)
)

// leinMainCompiled reports a :main namespace the uberjar's manifest can start:
// one compiled ahead of time with a class of its own. Leiningen compiles the
// :main namespace, but only `(:gen-class)` makes a class named for it.
// reitit's ring-integrant example has neither that nor :aot, and `java -jar`
// stopped with "Could not find or load main class example.server" (issue
// #90). Such a namespace is started by Clojure, from the jar's sources.
func leinMainCompiled(contextDir, project, ns string) bool {
	if strings.Contains(project, ":aot") {
		return true
	}
	if leinSkipAOT.MatchString(project) {
		return false
	}
	file := strings.ReplaceAll(strings.ReplaceAll(ns, "-", "_"), ".", "/")
	for _, ext := range []string{".clj", ".cljc"} {
		if cljGenClass.MatchString(readFile(contextDir, "src/"+file+ext)) {
			return true
		}
	}
	return false
}

// depsPlan: a deps.edn project started by the alias that names a main
// namespace (`clojure -M:server`), or by the one namespace defining -main.
func depsPlan(contextDir, deps string) (string, bool) {
	var run, prepare string
	if alias := depsMainAlias(deps); alias != "" {
		run, prepare = "clojure -M:"+alias, "clojure -P -M:"+alias
	}
	if run == "" {
		if ns := cljMainNamespace(contextDir); ns != "" {
			run, prepare = "clojure -M -m "+ns, "clojure -P"
		}
	}
	if p := procfileStart(contextDir); p != "" {
		run = p
		if prepare == "" {
			prepare = "clojure -P"
		}
	}
	if run == "" {
		return "", false
	}
	if !strings.HasPrefix(run, "exec ") {
		run = "exec " + run
	}
	return fmt.Sprintf(`# A Clojure CLI project (deps.edn).
FROM %s
WORKDIR /app
COPY . .
RUN %s
%s`, toolsDepsImage, prepare, shellCMD(run)), true
}

// depsMainAlias is the deps.edn alias naming a main namespace that a project
// is most likely started by, in order of preference, or "".
func depsMainAlias(deps string) string {
	aliases := cljMainOpts.FindAllStringSubmatch(deps, -1)
	for _, preferred := range []string{"run", "server", "start", "serve", "main", "prod"} {
		for _, a := range aliases {
			if a[1] == preferred {
				return a[1]
			}
		}
	}
	return ""
}

// cljMainNamespace is the one namespace under src/ that defines -main, or ""
// when there is none or several.
func cljMainNamespace(contextDir string) string {
	var found []string
	_ = filepath.WalkDir(filepath.Join(contextDir, "src"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || len(found) > 1 {
			return nil //nolint:nilerr // an unreadable file is not a build failure
		}
		if ext := filepath.Ext(path); ext != ".clj" && ext != ".cljc" {
			return nil
		}
		body, err := os.ReadFile(path) //nolint:gosec // G304: a file in the checkout, read to plan its build.
		if err != nil || !cljMainFn.Match(body) {
			return nil //nolint:nilerr // as above
		}
		if m := cljNamespace.FindSubmatch(body); m != nil {
			found = append(found, string(m[1]))
		}
		return nil
	})
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

// --- Scala (sbt) ------------------------------------------------------------

// The JDK and sbt launcher an sbt build runs with. [P] Temurin 17, which the
// sbt and Scala releases of the last several years all run on; the launcher
// boots whichever sbt project/build.properties names.
const (
	sbtJDKImage   = "eclipse-temurin:17-jdk"
	sbtLauncher   = "1.10.7"
	sbtLauncherAt = "https://github.com/sbt/sbt/releases/download/v" + sbtLauncher + "/sbt-" + sbtLauncher + ".tgz"
)

// sbtPlan builds an sbt project into something that starts without sbt: the
// start script sbt-native-packager stages (which Play always has), or the jar
// sbt-assembly makes. A project with neither is compiled, and run from its
// classpath with the main class sbt finds.
func sbtPlan(contextDir string) (string, bool) {
	if !fileExists(contextDir, "build.sbt") {
		return "", false
	}
	plugins := readFile(contextDir, "project/plugins.sbt")
	play := strings.Contains(plugins, `"sbt-plugin"`) &&
		(strings.Contains(plugins, "org.playframework") || strings.Contains(plugins, "com.typesafe.play"))

	var build, start string
	switch {
	case play || strings.Contains(plugins, "sbt-native-packager"):
		build = `sbt -batch stage && ls -1 target/universal/stage/bin/* */target/universal/stage/bin/* 2>/dev/null | grep -v '\.bat$' | head -n 1 > /app/.pando-start && test -s /app/.pando-start`
		start = `exec "$(cat /app/.pando-start)"`
		if play {
			// Play in production: its HTTP port, the secret it refuses to
			// start without, and every host, since Pando's proxy is what
			// the app is reached through.
			start += ` -Dhttp.port=${PORT:-9000} -Dplay.http.secret.key="$APPLICATION_SECRET" -Dplay.filters.hosts.allowed.0=. -Dpidfile.path=/dev/null`
		}
	case strings.Contains(plugins, "sbt-assembly"):
		build = `sbt -batch assembly && cp "$(ls -1 target/scala-*/*.jar */target/scala-*/*.jar 2>/dev/null | grep -i assembly | head -n 1)" /app.jar`
		start = "exec java -jar /app.jar"
	default:
		build = `sbt -batch -Dsbt.log.noformat=true compile "export Runtime / fullClasspath" "show Compile / discoveredMainClasses" > /tmp/sbt.out` +
			` && grep -v '^\[' /tmp/sbt.out | grep ':' | tail -n 1 > /app/.pando-classpath` +
			` && sed -n 's/^\[info\] \* //p' /tmp/sbt.out | head -n 1 > /app/.pando-main` +
			` && test -s /app/.pando-classpath && test -s /app/.pando-main`
		start = `exec java -cp "$(cat /app/.pando-classpath)" "$(cat /app/.pando-main)"`
	}
	if p := procfileStart(contextDir); p != "" {
		start = p
	}
	port := 0
	if play {
		port = 9000
	}
	return fmt.Sprintf(`# An sbt project.
FROM %s
RUN apt-get update && apt-get install -y --no-install-recommends curl && rm -rf /var/lib/apt/lists/* \
 && curl -fsSL %s | tar -xz -C /opt
ENV PATH=/opt/sbt/bin:$PATH
WORKDIR /app
COPY . .
RUN %s
%s%s`, sbtJDKImage, sbtLauncherAt, build, exposeLine(port), shellCMD(start)), true
}

// --- Haskell (Stack) --------------------------------------------------------

// The images a Stack project is built and run with. [P] The official Haskell
// image, which carries Stack; Stack installs the GHC the project's resolver
// names. The program runs on Debian slim with the libraries GHC programs link.
const haskellImage = "haskell:9"

var (
	hpackKey        = regexp.MustCompile(`^  ([A-Za-z0-9_\-]+):`)
	cabalExecutable = regexp.MustCompile(`(?m)^executable\s+([A-Za-z0-9_\-]+)`)
)

// stackPlan builds a Stack project's executable and runs it from the
// project's files.
func stackPlan(contextDir string) (string, bool) {
	if !fileExists(contextDir, "stack.yaml") {
		return "", false
	}
	var exes []string
	exes = append(exes, hpackExecutables(readFile(contextDir, "package.yaml"))...)
	cabals, _ := filepath.Glob(filepath.Join(contextDir, "*.cabal"))
	for _, c := range cabals {
		for _, e := range cabalExecutable.FindAllStringSubmatch(readFile(contextDir, filepath.Base(c)), -1) {
			exes = append(exes, e[1])
		}
	}
	exes = dedupeKeepOrder(exes)
	start := procfileStart(contextDir)
	if len(exes) != 1 && start == "" {
		// None to run, or several and nothing saying which is the server.
		return "", false
	}
	if start == "" {
		start = "exec /usr/local/bin/" + exes[0]
	}
	return fmt.Sprintf(`# A Haskell project, built with Stack.
FROM %[1]s AS build
RUN apt-get update && apt-get install -y --no-install-recommends libtinfo5 || true
WORKDIR /src
COPY . .
RUN stack build --install-ghc --copy-bins --local-bin-path /out

FROM %[2]s
USER root
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libgmp10 libffi8 zlib1g netbase && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY . .
COPY --from=build /out/ /usr/local/bin/
%[3]s`, haskellImage, programRuntimeImage, shellCMD(start)), true
}

// hpackExecutables are the names under package.yaml's executables:, two
// spaces in.
func hpackExecutables(body string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			in = strings.HasPrefix(line, "executables:")
			continue
		}
		if m := hpackKey.FindStringSubmatch(line); in && m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// --- Perl -------------------------------------------------------------------

// perlImage is the Perl an app runs on. [P] The official image, which has
// cpanm and a compiler for the modules that build one.
const perlImage = "perl:5.40"

var mojoliciousLite = regexp.MustCompile(`(?m)^\s*use\s+Mojolicious::Lite\b`)

// perlPlan installs a Perl app's dependencies with cpanm and runs its
// Procfile's command, its PSGI app under plackup, or its Mojolicious script.
func perlPlan(contextDir string) (string, bool) {
	hasDeps := fileExists(contextDir, "cpanfile") || fileExists(contextDir, "Makefile.PL")
	start := procfileStart(contextDir)
	port := 0
	extra := ""
	if start == "" {
		switch {
		case fileExists(contextDir, "app.psgi"), fileExists(contextDir, "bin/app.psgi"):
			psgi := "app.psgi"
			if !fileExists(contextDir, psgi) {
				psgi = "bin/app.psgi"
			}
			start, port, extra = "exec plackup --host 0.0.0.0 --port ${PORT:-5000} "+psgi, 5000, " Plack"
		default:
			script := mojoliciousScript(contextDir)
			if script == "" {
				return "", false
			}
			start, port = "exec perl "+script+" daemon -m production -l http://*:${PORT:-3000}", 3000
		}
	}
	if !hasDeps && port == 0 && !strings.HasPrefix(strings.TrimPrefix(start, "exec "), "perl ") {
		return "", false
	}
	install := ""
	if hasDeps {
		install = "RUN cpanm --notest --installdeps .\n"
	}
	if extra != "" {
		install += "RUN cpanm --notest" + extra + "\n"
	}
	return fmt.Sprintf(`# A Perl app.
FROM %s
WORKDIR /app
COPY . .
%s%s%s`, perlImage, install, exposeLine(port), shellCMD(start)), true
}

// mojoliciousScript is the Mojolicious::Lite script at the top of the app, or
// "".
func mojoliciousScript(contextDir string) string {
	files, _ := filepath.Glob(filepath.Join(contextDir, "*.pl"))
	for _, f := range files {
		if mojoliciousLite.MatchString(readFile(contextDir, filepath.Base(f))) {
			return filepath.Base(f)
		}
	}
	return ""
}

// --- Python, setup.py only --------------------------------------------------

// setupPyImage is the Python a setup.py package is installed on. [P] A
// current release, slim.
const setupPyImage = "python:3.12-slim"

var (
	consoleScript = regexp.MustCompile(`["']([A-Za-z0-9_.\-]+)\s*=\s*([A-Za-z0-9_.]+):([A-Za-z0-9_]+)["']`)
	argparsePort  = regexp.MustCompile(`add_argument\(\s*["']--port["']`)
	argparseHost  = regexp.MustCompile(`add_argument\(\s*["']--host["']`)
)

// setupPyPlan installs a package that has only a setup.py — no
// requirements.txt, pyproject.toml or Pipfile, so Railpack does not see Python
// at all — and runs its one console script. A script that takes --port is
// given the port Pando routes to, and --host every interface.
func setupPyPlan(contextDir string) (string, bool) {
	if !fileExists(contextDir, "setup.py") {
		return "", false
	}
	for _, other := range []string{"requirements.txt", "pyproject.toml", "Pipfile"} {
		if fileExists(contextDir, other) {
			return "", false
		}
	}
	start := procfileStart(contextDir)
	if start == "" {
		scripts := consoleScript.FindAllStringSubmatch(readFile(contextDir, "setup.py"), -1)
		if len(scripts) == 1 {
			start = "exec " + scripts[0][1]
			module := strings.ReplaceAll(scripts[0][2], ".", "/")
			body := readFile(contextDir, module+".py") + readFile(contextDir, module+"/__init__.py")
			if argparseHost.MatchString(body) {
				start += " --host 0.0.0.0"
			}
			if argparsePort.MatchString(body) {
				start += " --port ${PORT:-8000}"
			}
		}
	}
	cmd := ""
	if start != "" {
		cmd = shellCMD(start)
	}
	return fmt.Sprintf(`# A Python package installed from its setup.py.
FROM %s
WORKDIR /app
COPY . .
RUN pip install --no-cache-dir .
%s`, setupPyImage, cmd), true
}

// --- PHP, no composer.json --------------------------------------------------

func plainPHPPlan(contextDir string) (string, bool) {
	root := plainPHPRoot(contextDir)
	if root == "" {
		return "", false
	}
	return plainPHPDockerfile(root), true
}
