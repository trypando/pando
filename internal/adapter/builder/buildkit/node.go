package buildkit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Node servers are planned by Pando rather than by Railpack.
//
// nixpacks 1.41 installs Node from a pinned Nix package set whose newest 22 is
// 22.11 — which cannot require() an ES module, so Express and NestJS apps built
// and exited with ERR_REQUIRE_ESM — and which has no Node 24 at all, so asking
// for it failed every build with "undefined variable 'nodejs_24'" (issue #55).
// Installing Node through Nix is also the most memory-hungry step of a build,
// and several concurrent builds were killed for it. The official Node image
// carries current releases of every line and needs no Nix.

// nodeServer is what reading package.json said about a Node server.
type nodeServer struct {
	Node    string
	Install string
	Build   bool
	Start   string
	// Native is a project with a native addon, built with Python, make and
	// a C++ compiler, which the slim image does not carry.
	Native bool
}

// nodeEntryPoints are the files a Node server with no start script is started
// from, in the order Node developers reach for them.
var nodeEntryPoints = []string{"server.js", "index.js", "app.js", "main.js", "server.mjs", "index.mjs"}

// notNodeServer names the files that make a package.json part of a project
// some other toolchain builds: a Maven or Gradle build that runs the front
// end's npm build through its plugins (JHipster, which was built as a Node app
// and crashed in `npm install`), a Deno app, and a Meteor app (issue #90).
var notNodeServer = []string{"pom.xml", "build.gradle", "build.gradle.kts", "deno.json", "deno.jsonc", ".meteor/release"}

// pnpmLockfile is pnpm's lockfile.
const pnpmLockfile = "pnpm-lock.yaml"

// readNodeServer reports a Node app Pando knows how to build and start.
//
// A static site is not one (staticsite.go takes it first), and neither is a
// Bun project, which needs its own runtime and is left to Railpack. An app with
// no way to start is left to Railpack too, and detection asks for the start
// command.
func readNodeServer(contextDir, startCommand string) (nodeServer, bool) {
	pkg, ok := readPackageJSON(contextDir)
	if !ok || !plainNodeProject(contextDir, pkg) {
		return nodeServer{}, false
	}

	start := nodeStart(contextDir, startCommand, pkg)
	if start == "" || strings.ContainsAny(start, "\n\r") {
		return nodeServer{}, false
	}

	install := installCommand(contextDir)
	// A start script that runs pnpm or pnpx needs pnpm in the image whatever
	// installed the dependencies. TanStack's examples start with `pnpx srvx`
	// and exited 127 with "pnpx: not found" (issue #90).
	if !strings.Contains(install, "pnpm") && (runsPnpm(start, pkg.Scripts) || pnpmCommand.MatchString(pkg.Scripts["build"])) {
		install = "npm install -g pnpm@" + pnpmVersion(contextDir, pkg.PackageManager) + " && " + install
	}
	return nodeServer{
		Node:    nodeMajor(contextDir, pkg.Engines),
		Install: install,
		Build:   strings.TrimSpace(pkg.Scripts["build"]) != "",
		Start:   start,
		Native:  compilesNative(pkg),
	}, true
}

// nativeAddons are packages whose install compiles C or C++ with node-gyp
// whenever no prebuilt binary fits, which is always for some of their
// releases: better-sqlite3 13 builds from source at every install, and the
// AdonisJS starter kits stopped there with "gyp ERR! find Python" (issue #90).
var nativeAddons = []string{"better-sqlite3", "sqlite3", "bcrypt", "argon2"}

// compilesNative reports a project that depends on a native addon.
func compilesNative(pkg packageJSON) bool {
	for _, name := range nativeAddons {
		if pkg.Dependencies[name] != "" || pkg.DevDependencies[name] != "" {
			return true
		}
	}
	return false
}

// plainNodeProject reports a package.json project that Node alone builds: not
// a Bun project, not part of another toolchain's build, not a workspace root.
func plainNodeProject(contextDir string, pkg packageJSON) bool {
	for _, bun := range []string{"bun.lockb", "bun.lock", "bunfig.toml"} {
		if _, err := os.Stat(filepath.Join(contextDir, bun)); err == nil {
			return false
		}
	}
	if belongsToAnotherToolchain(contextDir, notNodeServer...) {
		return false
	}
	// A workspace root builds several packages, in an order Railpack and the
	// workspace tool know better than a generic plan does.
	w := strings.TrimSpace(string(pkg.Workspaces))
	return w == "" || w == "null" || w == "[]"
}

// nodeStart is the command a Node server starts with: the one given, the
// start script, package.json's main, a conventional entry point, or the
// framework's server; "" when there is none.
func nodeStart(contextDir, startCommand string, pkg packageJSON) string {
	if start := strings.TrimSpace(startCommand); start != "" {
		return start
	}
	if strings.TrimSpace(pkg.Scripts["start"]) != "" {
		return "npm start"
	}
	if pkg.Main != "" && fileExists(contextDir, pkg.Main) {
		return "node " + pkg.Main
	}
	for _, entry := range nodeEntryPoints {
		if fileExists(contextDir, entry) {
			return "node " + entry
		}
	}
	return frameworkServerStart(contextDir, pkg)
}

var pnpmCommand = regexp.MustCompile(`(^|[\s;&|(])(pnpm|pnpx)(\s|$)`)

// runsPnpm reports a start command, or the start script it runs, that calls
// pnpm.
func runsPnpm(start string, scripts map[string]string) bool {
	if start == "npm start" {
		start = scripts["start"]
	}
	return pnpmCommand.MatchString(start)
}

var svelteKitAdapterOut = regexp.MustCompile(`\bout\s*:\s*["']([^"']+)["']`)

// frameworkServerStart is how a framework's production build is started when
// the package.json does not say.
//
// SvelteKit and Nuxt templates ship no start script: the build writes a server
// and the documentation says how to run it. Planned as a Vite static site, a
// SvelteKit app on adapter-node built its server into build/ and the image
// copied a dist/ that was never written (issue #90).
func frameworkServerStart(contextDir string, pkg packageJSON) string {
	switch {
	case pkg.has("@sveltejs/kit") && pkg.has("@sveltejs/adapter-node"):
		// adapter-node's `out` option, default build/. It listens on PORT.
		out := "build"
		if _, config := readFileIn(contextDir, ".", "svelte.config.js", "svelte.config.mjs", "svelte.config.ts"); config != nil {
			if dir := submatch(svelteKitAdapterOut, config); safeRelative(dir) {
				out = dir
			}
		}
		return "node " + out
	case pkg.has("@sveltejs/kit") && !pkg.has("@sveltejs/adapter-static"):
		// adapter-auto finds no platform to adapt to and writes no server, and
		// SvelteKit's template says to run `vite preview` over the build.
		return "npx vite preview --host 0.0.0.0 --port ${PORT:-4173}"
	case pkg.has("nuxt") && strings.HasPrefix(strings.TrimSpace(pkg.Scripts["build"]), "nuxt build"):
		// Nitro's node server, which listens on PORT on every interface.
		return "node .output/server/index.mjs"
	}
	return ""
}

func fileExists(dir, name string) bool {
	clean := filepath.Clean("/" + name)
	info, err := os.Stat(filepath.Join(dir, clean))
	return err == nil && !info.IsDir()
}

// --- installing dependencies -------------------------------------------------

// installCommand installs dependencies the way the lockfile says.
//
// pnpm is installed at the version the project uses rather than through
// corepack, which fetches the newest pnpm when package.json names none. The
// newest refused a lockfile written by pnpm 8 (ERR_PNPM_BROKEN_LOCKFILE) and
// stopped on build scripts nobody had approved (ERR_PNPM_IGNORED_BUILDS), where
// the version the author used installs both (issue #90). Bun's lockfile is
// installed by Bun.
func installCommand(contextDir string) string {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(contextDir, name))
		return err == nil
	}
	switch {
	case has(pnpmLockfile):
		pkg, _ := readPackageJSON(contextDir)
		version := pnpmVersion(contextDir, pkg.PackageManager)
		install := "npm install -g pnpm@" + version + " && pnpm install --frozen-lockfile"
		// pnpm 10 and later run no dependency's build scripts unless the
		// project approves them, and newer releases fail the install over
		// it. A build runs what the author's machine ran: npm, Yarn and
		// earlier pnpm all run them. A project that says which builds it
		// approves is left to say so: pnpm refuses the flag beside its own
		// list (ERR_PNPM_CONFIG_CONFLICT_BUILT_DEPENDENCIES, issue #90).
		if major, err := strconv.Atoi(strings.SplitN(version, ".", 2)[0]); err == nil && major >= 10 && !pnpmApprovesBuilds(contextDir, pkg) {
			install += " --config.dangerously-allow-all-builds=true"
		}
		return install
	case usesBun(contextDir):
		return "npm install -g bun && bun install --frozen-lockfile"
	case has("yarn.lock"):
		return "corepack enable && yarn install --frozen-lockfile"
	case has("package-lock.json"):
		return "npm ci"
	default:
		return "npm install"
	}
}

// usesBun reports a project whose lockfile is Bun's and that has no npm, pnpm
// or Yarn lockfile beside it.
func usesBun(contextDir string) bool {
	for _, other := range []string{pnpmLockfile, "yarn.lock", "package-lock.json"} {
		if fileExists(contextDir, other) {
			return false
		}
	}
	return fileExists(contextDir, "bun.lock") || fileExists(contextDir, "bun.lockb")
}

// pnpmDefault is the pnpm a project gets when nothing says which. [P] 9, the
// newest major that runs dependency build scripts without being told to.
const pnpmDefault = "9"

var (
	packageManagerPnpm = regexp.MustCompile(`^pnpm@(\d+(?:\.\d+){0,2}(?:-[0-9A-Za-z.]+)?)(?:\+.*)?$`)
	lockfileVersion    = regexp.MustCompile(`^lockfileVersion:\s*['"]?(\d+)(?:\.(\d+))?`)
)

// pnpmVersion is the pnpm a project uses: the version packageManager names, or
// the major that writes the lockfile's format, or the default.
//
// A pnpm-workspace.yaml that holds only settings, with no packages list, was
// written for pnpm 10, which reads settings there; pnpm 9 refuses the file
// ("packages field missing or empty", issue #90). Both write lockfile 9.0, so
// the workspace file is what tells them apart.
func pnpmVersion(contextDir, packageManager string) string {
	if m := packageManagerPnpm.FindStringSubmatch(strings.TrimSpace(packageManager)); m != nil {
		return m[1]
	}
	version := pnpmLockfileVersion(contextDir)
	if ws, ok := pnpmWorkspace(contextDir); ok && version == "9" {
		if packages, _ := ws["packages"].([]any); len(packages) == 0 {
			return "10"
		}
	}
	return version
}

// pnpmLockfileVersion is the pnpm major that writes the lockfile's format, or
// the default.
func pnpmLockfileVersion(contextDir string) string {
	f, err := os.Open(filepath.Join(contextDir, pnpmLockfile))
	if err != nil {
		return pnpmDefault
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for i := 0; i < 5 && scanner.Scan(); i++ {
		m := lockfileVersion.FindStringSubmatch(strings.TrimSpace(scanner.Text()))
		if m == nil {
			continue
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		switch {
		case major >= 9:
			return "9"
		case major == 6:
			return "8"
		case major == 5 && minor >= 4:
			return "7"
		case major == 5:
			return "6"
		}
		return pnpmDefault
	}
	return pnpmDefault
}

// pnpmWorkspace reads pnpm-workspace.yaml, reporting whether there is one. A
// file that does not parse reads as empty settings.
func pnpmWorkspace(contextDir string) (map[string]any, bool) {
	body, err := os.ReadFile(filepath.Join(contextDir, "pnpm-workspace.yaml")) //nolint:gosec // G304: a file in the checkout, read to plan its build.
	if err != nil {
		return nil, false
	}
	ws := map[string]any{}
	_ = yaml.Unmarshal(body, &ws)
	return ws, true
}

// pnpmBuildSettings are the pnpm settings by which a project says whose build
// scripts run, as pnpm-workspace.yaml and package.json's "pnpm" field spell
// them, and as .npmrc spells them.
var (
	pnpmBuildSettings  = []string{"onlyBuiltDependencies", "onlyBuiltDependenciesFile", "neverBuiltDependencies", "allowBuilds", "dangerouslyAllowAllBuilds"}
	pnpmBuildSettingRC = regexp.MustCompile(`(?m)^\s*(only-built-dependencies|only-built-dependencies-file|never-built-dependencies|dangerously-allow-all-builds)\s*(\[\])?\s*=`)
)

// pnpmApprovesBuilds reports a project that says itself which dependency build
// scripts run.
func pnpmApprovesBuilds(contextDir string, pkg packageJSON) bool {
	ws, _ := pnpmWorkspace(contextDir)
	for _, key := range pnpmBuildSettings {
		if _, ok := ws[key]; ok {
			return true
		}
		if _, ok := pkg.Pnpm[key]; ok {
			return true
		}
	}
	return pnpmBuildSettingRC.MatchString(readFile(contextDir, ".npmrc"))
}

// nodeOnto adds Node, npm and corepack, from the official Node image of the
// given version, to a stage built on another language's image. Only Node's own
// files are copied, so the rest of /usr/local, where the PHP and Python images
// keep their interpreters, is left as it was. The links are the ones the Node
// image itself makes.
func nodeOnto(version string) string {
	return fmt.Sprintf(`COPY --from=node:%[1]s-slim /usr/local/bin/node /usr/local/bin/node
COPY --from=node:%[1]s-slim /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -sf ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && \
    ln -sf ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx && \
    if [ -d /usr/local/lib/node_modules/corepack ]; then ln -sf ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack; fi
`, version)
}

// --- choosing Node -----------------------------------------------------------

var (
	leadingMajor = regexp.MustCompile(`^\D*(\d+)`)
	singleMajor  = regexp.MustCompile(`^[\^~]?\d+(\.[\dx*]+)*$`)
)

// nodeLines are the Node majors a range is matched against, the default first
// and then the newest, so a range gets the default when it allows it and the
// newest line it allows otherwise.
var nodeLines = []int{22, 24, 20, 18, 16, 14, 12, 10}

// nodeMajor is the Node major the app is built with: what .nvmrc or
// .node-version says, or a single major engines.node names, or the line a
// range allows.
//
// A range is matched rather than its lower bound taken: ">= 7.6" means any
// Node from 7.6 on, and building on 7 failed `npm install` on every dependency
// written since (issue #90).
func nodeMajor(contextDir string, engines map[string]any) string {
	if v := nodeVersionFile(contextDir); v != "" {
		return v
	}
	if v, ok := engines["node"].(string); ok {
		if major := nodeMajorFor(v); major != "" {
			return major
		}
	}
	return staticSiteNodeDefault
}

// nodeVersionFile is the major .nvmrc or .node-version names, or "".
func nodeVersionFile(contextDir string) string {
	for _, name := range []string{".nvmrc", ".node-version"} {
		body, err := os.ReadFile(filepath.Join(contextDir, name))
		if err != nil {
			continue
		}
		if m := leadingMajor.FindStringSubmatch(strings.TrimSpace(string(body))); m != nil {
			return m[1]
		}
	}
	return ""
}

// nodeMajorFor is the major an engines.node value asks for, or "" when it
// allows none Pando can tell.
func nodeMajorFor(spec string) string {
	spec = strings.TrimSpace(spec)
	if singleMajor.MatchString(spec) {
		return leadingMajor.FindStringSubmatch(spec)[1]
	}
	for _, line := range nodeLines {
		if rangeAllows(spec, line) {
			return strconv.Itoa(line)
		}
	}
	// Nothing Pando knows satisfies it, ">=26" say: the lower bound is the
	// best reading there is.
	if m := leadingMajor.FindStringSubmatch(spec); m != nil {
		return m[1]
	}
	return ""
}

// spacedOperator matches an operator with a space after it, ">= 7.6", which
// npm reads as ">=7.6".
var spacedOperator = regexp.MustCompile(`([<>]=?|=|\^|~)\s+`)

// rangeAllows reports whether an engines.node range allows the newest release
// of a Node line, which is what the line's image carries. The range is read
// with the same constraint reading a Laravel app's PHP is chosen by; npm's and
// Composer's differ only in corners an engines field does not reach.
func rangeAllows(spec string, line int) bool {
	spec = strings.TrimSpace(spacedOperator.ReplaceAllString(spec, "$1"))
	if spec == "" || spec == "*" || spec == "x" {
		return true
	}
	return satisfies([3]int{line, 999, 999}, spec)
}

// railpackNodeDefaults pins the Node line Railpack installs when engines.node
// is a range and nothing else names a version.
//
// Railpack hands the range to mise, which installed the oldest release it
// allowed: ">= 7.6" built on Node 7.10 and npm 4, and `npm install` failed on
// the first dependency written since (issue #90). A version file is Railpack's
// to read, as it reads it already.
func railpackNodeDefaults(contextDir string) []string {
	pkg, ok := readPackageJSON(contextDir)
	if !ok || nodeVersionFile(contextDir) != "" {
		return nil
	}
	v, ok := pkg.Engines["node"].(string)
	if !ok || strings.TrimSpace(v) == "" || singleMajor.MatchString(strings.TrimSpace(v)) {
		return nil
	}
	if major := nodeMajorFor(v); major != "" {
		return []string{"RAILPACK_NODE_VERSION=" + major}
	}
	return nil
}

// writeNodeServerPlan builds and starts the app on the official Node image.
//
// Dependencies are installed with development dependencies included, because
// the build step usually needs them (TypeScript, a bundler), and NODE_ENV is set
// to production only for the running app.
func writeNodeServerPlan(contextDir string, n nodeServer) (string, error) {
	build, toolchain := "", ""
	if n.Build {
		build = "RUN npm run build\n"
	}
	if n.Native {
		toolchain = "RUN apt-get update && apt-get install -y --no-install-recommends python3 make g++ && rm -rf /var/lib/apt/lists/*\n"
	}
	start, err := json.Marshal(n.Start)
	if err != nil {
		return "", err
	}
	content := fmt.Sprintf(`# A Node server, built and run on Node %s.
FROM node:%s-slim
%sWORKDIR /app
COPY . .
RUN %s
%sENV NODE_ENV=production
ENTRYPOINT ["/bin/sh", "-c"]
CMD [%s]
`, n.Node, n.Node, toolchain, n.Install, build, start)
	return writePlan(contextDir, content)
}
