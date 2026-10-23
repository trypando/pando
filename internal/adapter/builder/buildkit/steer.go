package buildkit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Steering Railpack.
//
// Railpack picks a toolchain from what a repository says, and where the
// repository says nothing it picks a default. Both go wrong in ways a build
// log makes obvious and Railpack itself never looks at: a Rust default too old
// for the crates the lockfile resolves, a Python too new for a C extension the
// lockfile pins, a PHP extension a dependency needs and the root composer.json
// does not name. Each has a documented knob — RAILPACK_RUST_VERSION,
// RAILPACK_PYTHON_VERSION, RAILPACK_PHP_EXTENSIONS, the apt package lists — or
// is a small edit to the plan Railpack wrote.
//
// So Pando steers, two ways:
//
//   - Up front, from the repository, where the repository already says enough
//     (proactiveSteers): a version file mise cannot read, a Rust project that
//     pins no toolchain, a lockfile whose packages need an extension.
//   - After a build of the plan fails, from its log (reactiveSteers): plan
//     again with the knob that answers the failure, and build again.
//
// Bounded: each steer is applied once, at most maxSteers retries in all, and a
// failure no rule recognizes is reported as it was. Every steer is written to
// the build log and recorded beside the plan (steeringFile), and the plan it
// produced is the one the spec carries from then on — what a deploy replays is
// what was built (R-020). Only detection's build steers (api.BuildRequest.Steer);
// a deploy builds the spec's plan and fails if it does.

// maxSteers is how many times one build is planned again.
const maxSteers = 3

// steeringFile records, beside the plan, every steer that shaped it and why.
// It is in the plan's directory, so it travels with the plan into the spec and
// stays out of the image.
const steeringFile = "steering.txt"

// defaultRustVersion is the Rust a project that pins no toolchain is built
// with. [P] A current stable release. Railpack maps a Cargo edition to the
// oldest Rust that supports it (2021 to 1.84.0) and treats that as the version,
// so a project pinning nothing was built on a compiler older than its own
// lockfile's crates ("feature `edition2024` is required", "time@0.3.47
// requires rustc 1.88.0", issue #90). A version the repository names — a
// rust-toolchain file, rust-version in Cargo.toml — still wins: Railpack
// reads those after this setting.
const defaultRustVersion = "1.90.0"

// Literals steer.go uses more than once.
const (
	msgWritePlan        = "Could not write the build plan."
	steerKeyRustVersion = "rust-version:"
	envRustVersion      = "RAILPACK_RUST_VERSION="
	filePythonVersion   = ".python-version"
	fileRequirementsTxt = "requirements.txt"
	filePyprojectTOML   = "pyproject.toml"
	fileCargoTOML       = "Cargo.toml"
	fileGoMod           = "go.mod"
	cmdPDMInstall       = "pdm install"
)

// steer is one change to how Railpack is asked to plan.
type steer struct {
	// Key names the steer, so it is applied once.
	Key string

	// Why is one line for the build log and the record: what was seen, and
	// what Pando changed because of it.
	Why string

	// Env are Railpack settings, KEY=value. Later steers' list settings
	// (packages, extensions) add to earlier ones; a scalar one replaces it.
	Env []string

	// Edit changes the plan Railpack wrote. Nil for a steer that is only
	// settings.
	Edit func(plan map[string]any)
}

// --- applying steers --------------------------------------------------------

// steeredPlanner is base with the steers applied: their settings added to
// every planning call, their edits made to the plan it writes, and a record of
// them left beside it.
func steeredPlanner(base planner, steers []steer) planner {
	if len(steers) == 0 {
		return base
	}
	return func(contextDir string, env []string) (string, error) {
		all := append([]string(nil), env...)
		for _, s := range steers {
			all = append(all, s.Env...)
		}
		name, err := base(contextDir, mergeEnv(all))
		if err != nil {
			return "", err
		}
		if err := editPlan(contextDir, name, steers); err != nil {
			return "", err
		}
		if err := recordSteers(contextDir, steers); err != nil {
			return "", err
		}
		return name, nil
	}
}

// listSettings are Railpack's settings that take a list, which several steers
// may each add to.
var listSettings = map[string]bool{
	"RAILPACK_PACKAGES":            true,
	"RAILPACK_BUILD_APT_PACKAGES":  true,
	"RAILPACK_DEPLOY_APT_PACKAGES": true,
	"RAILPACK_PHP_EXTENSIONS":      true,
}

// mergeEnv folds settings named more than once into one: a list setting is the
// union of every value given, in order; any other is the last one given.
func mergeEnv(env []string) []string {
	var order []string
	values := map[string]string{}
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		prev, seen := values[key]
		if !seen {
			order = append(order, key)
		}
		if seen && listSettings[key] {
			value = strings.Join(dedupeKeepOrder(append(strings.Fields(prev), strings.Fields(value)...)), " ")
		}
		values[key] = value
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, key+"="+values[key])
	}
	return out
}

func dedupeKeepOrder(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// editPlan makes each steer's edit to the plan at name in contextDir.
func editPlan(contextDir, name string, steers []steer) error {
	var edits []func(map[string]any)
	for _, s := range steers {
		if s.Edit != nil {
			edits = append(edits, s.Edit)
		}
	}
	if len(edits) == 0 {
		return nil
	}
	body, err := readContextFile(contextDir, name)
	if err != nil {
		return errs.Wrap(errs.BuildFailed, "Could not read the build plan.", err)
	}
	var plan map[string]any
	if err := json.Unmarshal(body, &plan); err != nil {
		return errs.Wrap(errs.BuildFailed, "Railpack wrote a build plan Pando could not read.", err)
	}
	for _, edit := range edits {
		edit(plan)
	}
	out, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return errs.Wrap(errs.BuildFailed, msgWritePlan, err)
	}
	if err := writeContextFile(contextDir, name, append(out, '\n')); err != nil {
		return errs.Wrap(errs.BuildFailed, msgWritePlan, err)
	}
	return nil
}

// recordSteers writes what steered the plan beside it.
func recordSteers(contextDir string, steers []steer) error {
	var b strings.Builder
	b.WriteString("# Pando changed how Railpack planned this build. Each line is one change and why.\n")
	for _, s := range steers {
		fmt.Fprintf(&b, "%s: %s\n", s.Key, s.Why)
	}
	if err := writeContextFile(contextDir, filepath.Join(planDir, steeringFile), []byte(b.String())); err != nil {
		return errs.Wrap(errs.BuildFailed, msgWritePlan, err)
	}
	return nil
}

// --- the retry --------------------------------------------------------------

// steer plans a failed Railpack build again with whatever its log calls for,
// and builds that, until it builds, nothing more is recognized, or maxSteers
// is reached. The error returned on giving up is the last build's.
func (a *Adapter) steer(ctx context.Context, req api.BuildRequest, contextDir, cacheDir string, timeout time.Duration, failed error, log *logTail) (api.BuildResult, error) {
	// What the repository called for is in the plan already, and is planned
	// with again.
	applied := proactiveSteers(contextDir)
	for range maxSteers {
		body, _ := os.ReadFile(filepath.Join(contextDir, planDir, railpackPlanFile)) //nolint:gosec // G304: the checkout's own plan.
		next := newSteers(reactiveSteers(log.String(), contextDir, body), applied)
		if len(next) == 0 {
			return api.BuildResult{}, failed
		}
		applied = append(applied, next...)
		for _, s := range next {
			fmt.Fprintf(log.out, "=> Pando: %s\n", s.Why)
		}
		fmt.Fprintf(log.out, "=> Pando: planning the build again and building it once more\n")
		log.Reset()

		replan := req
		replan.GeneratedFiles = nil
		name, err := buildpackPlanSteered(replan, contextDir, applied)
		if err != nil {
			return api.BuildResult{}, err
		}
		if !isRailpackPlan(name) {
			// Steering is Railpack's; a plan that is no longer Railpack's has
			// nothing here to steer, and the first failure stands.
			return api.BuildResult{}, failed
		}
		plan, err := collectPlan(contextDir)
		if err != nil {
			return api.BuildResult{}, err
		}
		result, err := a.solve(ctx, req, contextDir, filepath.Dir(filepath.Join(contextDir, name)), name, cacheDir, timeout)
		if err == nil {
			result.GeneratedFiles = plan
			return result, nil
		}
		failed = err
		if ctx.Err() != nil {
			return api.BuildResult{}, err
		}
	}
	return api.BuildResult{}, failed
}

// newSteers are the steers in found not already applied, by key.
func newSteers(found, applied []steer) []steer {
	done := map[string]bool{}
	for _, s := range applied {
		done[s.Key] = true
	}
	var out []steer
	for _, s := range found {
		if !done[s.Key] {
			done[s.Key] = true
			out = append(out, s)
		}
	}
	return out
}

// logTail passes a build's log on and keeps its end, which is where the
// failing step is.
type logTail struct {
	out io.Writer
	mu  sync.Mutex
	buf []byte
}

const logTailMax = 64 << 10

func newLogTail(out io.Writer) *logTail {
	if out == nil {
		out = io.Discard
	}
	return &logTail{out: out}
}

func (t *logTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - logTailMax; over > 0 {
		t.buf = t.buf[over:]
	}
	t.mu.Unlock()
	return t.out.Write(p)
}

func (t *logTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// Reset forgets what was kept, so the next build's log is read on its own.
func (t *logTail) Reset() {
	t.mu.Lock()
	t.buf = nil
	t.mu.Unlock()
}

// --- reading a failure ------------------------------------------------------

var (
	rustcRequired    = regexp.MustCompile(`requires rustc (\d+\.\d+(?:\.\d+)?)`)
	cargoEdition     = regexp.MustCompile("feature `edition20(\\d\\d)` is required")
	pythonNoBuild    = regexp.MustCompile(`no precompiled python found for core:python@(\d+)\.(\d+)`)
	miseRangeFailed  = regexp.MustCompile(`semver range "[^"]*" is not supported for python|core:python@[<>=~^]`)
	composerMissing  = regexp.MustCompile(`requires? ext-([A-Za-z0-9_]+) \S+ -> it is missing from your system`)
	pdmStaleLock     = regexp.MustCompile(`Lockfile doesn't satisfy the project requirements|pdm lock --check`)
	pdmProjectFailed = regexp.MustCompile(`\[BuildError\]: Build backend raised error`)
	gemFailed        = regexp.MustCompile(`An error occurred while installing ([A-Za-z0-9_.\-]+) \(`)
	bundleBinstub    = regexp.MustCompile(`/usr/local/bundle/bin/[A-Za-z0-9_\-]+: \d+: exec: /usr/local/bundle/bin/ruby: not found`)
	failedStep       = regexp.MustCompile(`(?m)^!! ([^\n]*?): process\b`)
	installedPython  = regexp.MustCompile(`/mise/installs/python/3\.(\d+)\.`)
	pythonCompile    = regexp.MustCompile(`error: [Cc]ommand '\[?'?(?:cc|gcc|x86_64-linux-gnu-gcc|clang)|Failed building wheel for|error: too few arguments to function|fatal error: Python\.h|Py_UNICODE|longintrepr\.h`)
	pythonModules    = regexp.MustCompile(`No module named '(?:pkg_resources|distutils|imp)'`)
)

// reactiveSteers are the steers a failed build's log calls for. plan is the
// plan that failed, to read the versions it chose.
func reactiveSteers(log, contextDir string, plan []byte) []steer {
	var out []steer
	out = append(out, rustSteers(log)...)
	out = append(out, pythonVersionSteers(log)...)

	// Python: a C extension that does not build on the Python Railpack chose,
	// or a module newer Pythons no longer ship. One minor release older, as
	// far as the project's own requirement allows.
	if step := lastFailedStep(log); pythonInstallStep(step) && (pythonCompile.MatchString(log) || pythonModules.MatchString(log)) {
		if s, ok := olderPython(log, contextDir, plan); ok {
			out = append(out, s)
		}
	}

	out = append(out, phpExtensionSteers(log)...)

	// PDM: a lockfile older than the project, which `pdm install --check`
	// refuses and a plain `pdm install` brings up to date, as the author's
	// own install would.
	if pdmStaleLock.MatchString(log) {
		out = append(out, steer{
			Key: "pdm-install",
			Why: "pdm.lock does not match pyproject.toml, which `pdm install --check` refuses, so the plan runs `pdm install` and lets PDM update the lock as it installs",
			Edit: func(plan map[string]any) {
				replaceInCommands(plan, "pdm install --check", cmdPDMInstall)
			},
		})
	}

	// PDM: the project itself, which `pdm install` builds and installs beside
	// its dependencies, does not build in Railpack's install step, which has
	// only pyproject.toml and pdm.lock: hatchling refused it with "Readme
	// file does not exist: README.md" (issue #90). The app runs from its
	// files in /app, so the step installs the dependencies alone.
	if strings.Contains(lastFailedStep(log), cmdPDMInstall) && pdmProjectFailed.MatchString(log) {
		out = append(out, steer{
			Key: "pdm-no-self",
			Why: "the project did not build as a package in the install step, which has only pyproject.toml and pdm.lock, so the step installs its dependencies alone and the app runs from its files",
			Edit: func(plan map[string]any) {
				replaceInCommands(plan, cmdPDMInstall, cmdPDMInstall+" --no-self")
			},
		})
	}

	out = append(out, rubySteers(log)...)
	return out
}

// rustSteers answer a crate that needs a newer compiler than the one
// installed.
func rustSteers(log string) []steer {
	if m := rustcRequired.FindAllStringSubmatch(log, -1); m != nil {
		need := ""
		for _, v := range m {
			if versionLess(need, v[1]) {
				need = v[1]
			}
		}
		if versionLess(need, defaultRustVersion) {
			need = defaultRustVersion
		}
		return []steer{{
			Key: steerKeyRustVersion + need,
			Why: "a crate in the build requires rustc " + need + " or newer, so Rust is set to " + need,
			Env: []string{envRustVersion + need},
		}}
	}
	if cargoEdition.MatchString(log) {
		return []steer{{
			Key: steerKeyRustVersion + defaultRustVersion,
			Why: "a crate in the build needs a newer Cargo than the one installed, so Rust is set to " + defaultRustVersion,
			Env: []string{envRustVersion + defaultRustVersion},
		}}
	}
	return nil
}

// pythonVersionSteers answer a version file mise cannot install.
func pythonVersionSteers(log string) []steer {
	if m := pythonNoBuild.FindStringSubmatch(log); m != nil {
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		if minor <= 0 {
			return nil
		}
		version := fmt.Sprintf("%d.%d", major, minor-1)
		return []steer{{
			Key: "python-version:" + version,
			Why: fmt.Sprintf("no build of Python %d.%d is available yet, so Python is set to %s, the release before it", major, minor, version),
			Env: []string{"RAILPACK_PYTHON_VERSION=" + version},
			Edit: func(plan map[string]any) {
				excludeFromPlan(plan, pythonVersionFiles...)
			},
		}}
	}
	if miseRangeFailed.MatchString(log) {
		return []steer{unreadablePythonVersionFile()}
	}
	return nil
}

// phpExtensionSteers answer an extension a package needs and the image does
// not have.
func phpExtensionSteers(log string) []steer {
	m := composerMissing.FindAllStringSubmatch(log, -1)
	if m == nil {
		return nil
	}
	var exts []string
	for _, v := range m {
		exts = append(exts, strings.ToLower(v[1]))
	}
	exts = dedupeKeepOrder(exts)
	sort.Strings(exts)
	return []steer{{
		Key: "php-extensions:" + strings.Join(exts, ","),
		Why: "Composer reported the PHP extension " + strings.Join(exts, ", ") + " missing, so it is installed",
		Env: []string{"RAILPACK_PHP_EXTENSIONS=" + strings.Join(exts, " ")},
	}}
}

// rubySteers answer a native gem that does not compile with GCC 14's
// defaults, and a Bundler binstub that cannot find Ruby.
func rubySteers(log string) []steer {
	var out []steer
	// Ruby: a native gem older than GCC 14, which makes incompatible pointer
	// types an error. The gem is built with that error a warning again.
	if strings.Contains(log, "incompatible-pointer-types") || strings.Contains(log, "incompatible pointer type") {
		if m := gemFailed.FindStringSubmatch(log); m != nil {
			gem := m[1]
			key := "BUNDLE_BUILD__" + strings.ToUpper(strings.ReplaceAll(gem, "-", "___"))
			out = append(out, steer{
				Key: "gem-cflags:" + gem,
				Why: "the gem " + gem + " does not compile with this compiler's stricter defaults, so it is built with incompatible pointer types reported as warnings",
				Edit: func(plan map[string]any) {
					setStepVariable(plan, "install", key,
						"--with-cflags=-Wno-error=incompatible-pointer-types -Wno-error=int-conversion -Wno-error=implicit-function-declaration")
				},
			})
		}
	}

	// Ruby: Bundler's own binstub names a Ruby beside it, where Railpack's
	// mise-installed Ruby is not.
	if bundleBinstub.MatchString(log) {
		out = append(out, steer{
			Key: "bundle-ruby-link",
			Why: "a Bundler binstub looks for ruby beside it in /usr/local/bundle/bin, so the plan links the installed Ruby there",
			Edit: func(plan map[string]any) {
				appendStepCommand(plan, "install", `sh -c 'ln -sf "$(mise which ruby)" /usr/local/bundle/bin/ruby'`)
			},
		})
	}
	return out
}

// lastFailedStep is the command of the build step that failed, from BuildKit's
// `!! <step>: process …` line.
func lastFailedStep(log string) string {
	m := failedStep.FindAllStringSubmatch(log, -1)
	if m == nil {
		return ""
	}
	return m[len(m)-1][1]
}

// pythonInstallStep reports a step that installs Python packages.
func pythonInstallStep(step string) bool {
	for _, tool := range []string{"pip ", "pip3 ", "poetry ", "uv ", "pdm ", "pipenv ", "python -m pip"} {
		if strings.Contains(step, tool) {
			return true
		}
	}
	return false
}

// olderPython is the steer to the Python one minor release older than the one
// that failed, when the project allows it and it is no older than 3.10.
func olderPython(log, contextDir string, plan []byte) (steer, bool) {
	minor := 0
	if m := installedPython.FindStringSubmatch(log); m != nil {
		minor, _ = strconv.Atoi(m[1])
	}
	if minor == 0 {
		if v := misePackageVersion(plan, "python"); v != "" {
			if parts := strings.Split(v, "."); len(parts) >= 2 {
				minor, _ = strconv.Atoi(parts[1])
			}
		}
	}
	// A module removed from the standard library or from a fresh virtualenv
	// (pkg_resources, distutils) was last there in 3.11.
	target := minor - 1
	if pythonModules.MatchString(log) && target > 11 {
		target = 11
	}
	if minor == 0 || target < 10 {
		return steer{}, false
	}
	// A version the project names exactly is the author's, and not steered.
	if v := strings.TrimSpace(firstLine(readFile(contextDir, filePythonVersion))); v != "" && plainVersion(v) {
		return steer{}, false
	}
	if c := pythonRequirement(contextDir); c != "" && !pythonAllows(c, target) {
		return steer{}, false
	}
	version := fmt.Sprintf("3.%d", target)
	return steer{
		Key: "python-version:" + version,
		Why: fmt.Sprintf("a Python package did not build on Python 3.%d, so Python is set to %s, which the project allows", minor, version),
		Env: []string{"RAILPACK_PYTHON_VERSION=" + version},
		Edit: func(plan map[string]any) {
			excludeFromPlan(plan, pythonVersionFiles...)
		},
	}, true
}

// --- steering up front ------------------------------------------------------

// proactiveSteers are the steers the repository already calls for, before any
// build has failed.
func proactiveSteers(contextDir string) []steer {
	var out []steer
	if fileExists(contextDir, fileCargoTOML) && !rustPinned(contextDir) {
		out = append(out, steer{
			Key: steerKeyRustVersion + defaultRustVersion,
			Why: "the project pins no Rust toolchain, so it is built with a current stable Rust, " + defaultRustVersion,
			Env: []string{envRustVersion + defaultRustVersion},
		})
	}
	if v := strings.TrimSpace(readFile(contextDir, filePythonVersion)); v != "" && !plainVersion(firstLine(v)) {
		out = append(out, unreadablePythonVersionFile())
	}
	if s, ok := pythonForPinnedPackages(contextDir); ok {
		out = append(out, s)
	}
	if s, ok := installPythonProject(contextDir); ok {
		out = append(out, s)
	}
	if s, ok := installSrcLayoutPackage(contextDir); ok {
		out = append(out, s)
	}
	if exts := lockedPHPExtensions(contextDir); len(exts) > 0 {
		out = append(out, steer{
			Key: "php-extensions:" + strings.Join(exts, ","),
			Why: "composer.lock's packages need the PHP extensions " + strings.Join(exts, ", ") + ", so they are installed",
			Env: []string{"RAILPACK_PHP_EXTENSIONS=" + strings.Join(exts, " ")},
		})
	}
	if s, ok := composerPostInstall(contextDir); ok {
		out = append(out, s)
	}
	if s, ok := rubyForLockedRack(contextDir); ok {
		out = append(out, s)
	}
	if pythonDependsOn(contextDir, "reflex") {
		// Reflex installs Bun the first time it starts, with unzip (issue #90:
		// "SystemPackageMissingError: System package 'unzip' is missing").
		out = append(out, steer{
			Key: "deploy-packages:unzip",
			Why: "Reflex installs Bun with unzip when it starts, so unzip is in the image",
			Env: []string{"RAILPACK_DEPLOY_APT_PACKAGES=unzip curl"},
		})
	}
	return out
}

// pythonVersionFiles are the files mise reads a Python version from.
var pythonVersionFiles = []string{filePythonVersion}

// unreadablePythonVersionFile leaves a .python-version mise cannot read out of
// the build. Railpack resolves the range itself; mise, which also reads the
// file, cannot ("semver range \">=\" is not supported for python").
func unreadablePythonVersionFile() steer {
	return steer{
		Key: "python-version-file",
		Why: ".python-version holds a range rather than a version, which mise cannot install, so the version Railpack resolved from it is used and the file is left out of the build",
		Edit: func(plan map[string]any) {
			excludeFromPlan(plan, pythonVersionFiles...)
		},
	}
}

var plainVersionPattern = regexp.MustCompile(`^(?:python-|cpython-)?\d+(?:\.\d+){0,2}$`)

func plainVersion(v string) bool { return plainVersionPattern.MatchString(strings.TrimSpace(v)) }

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// toolchainChannel is declared in rust.go.
var cargoRustVersion = regexp.MustCompile(`(?m)^\s*rust-version\s*=`)

// rustPinned reports a project that names its Rust release: a rust-toolchain
// file with a numbered channel, a rust-version file, or rust-version in
// Cargo.toml. A channel of "stable" names no release.
func rustPinned(contextDir string) bool {
	for _, name := range []string{"rust-version.txt", ".rust-version"} {
		if strings.TrimSpace(readFile(contextDir, name)) != "" {
			return true
		}
	}
	if toolchainChannel.MatchString(readFile(contextDir, "rust-toolchain.toml")) {
		return true
	}
	if v := strings.TrimSpace(readFile(contextDir, "rust-toolchain")); v != "" && plainVersion(v) {
		return true
	}
	return cargoRustVersion.MatchString(readFile(contextDir, fileCargoTOML))
}

var pinnedGunicorn = regexp.MustCompile(`(?im)^\s*gunicorn\s*==\s*(\d+)\.`)

// pythonForPinnedPackages is Python 3.11 for a project that pins a gunicorn
// older than 22, which imports pkg_resources: a virtualenv made by Python
// 3.12 or newer has no setuptools, so the app stopped at start with "No module
// named 'pkg_resources'" (issue #90). Not when the project names its Python.
func pythonForPinnedPackages(contextDir string) (steer, bool) {
	m := pinnedGunicorn.FindStringSubmatch(readFile(contextDir, fileRequirementsTxt))
	if m == nil {
		return steer{}, false
	}
	if major, _ := strconv.Atoi(m[1]); major >= 22 {
		return steer{}, false
	}
	if pythonPinned(contextDir) {
		return steer{}, false
	}
	// A range the project gives, in .python-version or pyproject.toml, still
	// has to admit 3.11.
	for _, c := range []string{firstLine(strings.TrimSpace(readFile(contextDir, filePythonVersion))), pythonRequirement(contextDir)} {
		if c != "" && !pythonAllows(c, 11) {
			return steer{}, false
		}
	}
	return steer{
		Key: "python-version:3.11",
		Why: "requirements.txt pins a gunicorn that imports pkg_resources, which a virtualenv made by Python 3.12 or newer does not have, so Python is set to 3.11",
		Env: []string{"RAILPACK_PYTHON_VERSION=3.11"},
	}, true
}

// pythonVenv is where Railpack keeps a Python app's virtualenv, and so where
// the project is installed when Railpack plans no install of its own.
const pythonVenv = "/app/.venv"

var buildSystem = regexp.MustCompile(`(?m)^\[build-system\]`)

// installPythonProject installs a pyproject.toml project Railpack plans no
// install for: one built by flit, hatchling or setuptools with no lockfile
// Railpack reads (requirements.txt, uv.lock, poetry.lock, pdm.lock, a
// Pipfile). Railpack's plan for it had no install step at all, so nothing
// the project depends on was installed: "flask: command not found" (issue
// #90). The project's own build backend installs it, into the virtualenv
// Railpack's Python plans use, whose bin/ goes on the image's PATH.
func installPythonProject(contextDir string) (steer, bool) {
	if !buildSystem.MatchString(readFile(contextDir, filePyprojectTOML)) {
		return steer{}, false
	}
	for _, lock := range []string{fileRequirementsTxt, "uv.lock", "poetry.lock", "pdm.lock", "Pipfile"} {
		if fileExists(contextDir, lock) {
			return steer{}, false
		}
	}
	return steer{
		Key: "python-install-project",
		Why: "pyproject.toml has no lockfile Railpack installs from, so the plan installs the project itself with pip, as its build backend says",
		Env: []string{"RAILPACK_INSTALL_CMD=python -m venv " + pythonVenv + " && " + pythonVenv + "/bin/pip install ."},
		Edit: func(plan map[string]any) {
			deploy, _ := plan["deploy"].(map[string]any)
			if deploy == nil {
				return
			}
			paths, _ := deploy["paths"].([]any)
			for _, p := range paths {
				if p == pythonVenv+"/bin" {
					return
				}
			}
			deploy["paths"] = append([]any{pythonVenv + "/bin"}, paths...)
			vars, _ := deploy["variables"].(map[string]any)
			if vars == nil {
				vars = map[string]any{}
			}
			vars["VIRTUAL_ENV"] = pythonVenv
			deploy["variables"] = vars
		},
	}, true
}

// installSrcLayoutPackage installs a project that keeps its package under
// src/ and its pinned dependencies in requirements.txt. Railpack installs the
// requirements, which are the project's dependencies and not the project, and
// src/ is not on Python's path: Flask's Celery example stopped with "Could not
// import 'task_app'" (issue #90). Its README installs the project after the
// requirements, and so does the plan, without its dependencies, which the
// requirements already pinned.
func installSrcLayoutPackage(contextDir string) (steer, bool) {
	if !buildSystem.MatchString(readFile(contextDir, filePyprojectTOML)) || !fileExists(contextDir, fileRequirementsTxt) {
		return steer{}, false
	}
	if pkgs, _ := filepath.Glob(filepath.Join(contextDir, "src", "*", "__init__.py")); len(pkgs) == 0 {
		return steer{}, false
	}
	return steer{
		Key: "python-install-src-package",
		Why: "the package is under src/, which requirements.txt does not install, so the plan installs the project after its requirements",
		Edit: func(plan map[string]any) {
			for _, step := range planSteps(plan) {
				if step["name"] != "build" {
					continue
				}
				commands, _ := step["commands"].([]any)
				step["commands"] = append(commands, map[string]any{"cmd": pythonVenv + "/bin/pip install --no-deps ."})
			}
		},
	}, true
}

// pythonPinned reports a project that names its Python. A .python-version
// that holds a range names none: Google's Flask sample says `>= 3.10`, which
// Railpack read as the newest Python there is, and its pinned gunicorn then
// stopped at start without pkg_resources (issue #90).
func pythonPinned(contextDir string) bool {
	if v := strings.TrimSpace(readFile(contextDir, filePythonVersion)); v != "" && plainVersion(firstLine(v)) {
		return true
	}
	for _, name := range []string{"runtime.txt", ".tool-versions"} {
		if strings.TrimSpace(readFile(contextDir, name)) != "" {
			return true
		}
	}
	return false
}

var (
	requiresPython = regexp.MustCompile(`(?m)^\s*requires-python\s*=\s*["']([^"']+)["']`)
	poetryPython   = regexp.MustCompile(`(?m)^\s*python\s*=\s*["']([^"']+)["']`)
)

// pythonRequirement is the Python version constraint the project declares, or
// "".
func pythonRequirement(contextDir string) string {
	body := readFile(contextDir, filePyprojectTOML)
	if m := requiresPython.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	if m := poetryPython.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// pythonAllows reports whether a Python version constraint, in PEP 440's or
// Poetry's spelling, admits some release of Python 3.minor. Read with
// Composer's constraint rules, which share the operators; `~=` is Composer's
// `~`. PEP 440 allows a space after the operator (`>= 3.10`), which
// Composer's rules read as two parts.
func pythonAllows(constraint string, minor int) bool {
	constraint = operatorSpace.ReplaceAllString(strings.ReplaceAll(constraint, "~=", "~"), "$1")
	return minorSatisfies(fmt.Sprintf("3.%d", minor), constraint)
}

var operatorSpace = regexp.MustCompile(`([<>=!~^]+)\s+`)

// pythonDependsOn reports a Python project that names pkg among its
// dependencies.
func pythonDependsOn(contextDir, pkg string) bool {
	pattern := regexp.MustCompile(`(?im)(^|["'\s])` + regexp.QuoteMeta(pkg) + `\s*([=<>~!\[;"',]|$)`)
	for _, name := range []string{fileRequirementsTxt, filePyprojectTOML, "Pipfile"} {
		if pattern.MatchString(readFile(contextDir, name)) {
			return true
		}
	}
	return false
}

// lockedPHPExtensions are the PHP extensions composer.lock's packages
// require, which Railpack, reading only the root composer.json, does not
// install ("drupal/core 11.x-dev requires ext-gd * -> it is missing from your
// system", issue #90).
func lockedPHPExtensions(contextDir string) []string {
	body := readFile(contextDir, "composer.lock")
	if body == "" {
		return nil
	}
	var lock struct {
		Packages []struct {
			Require map[string]string `json:"require"`
		} `json:"packages"`
	}
	if json.Unmarshal([]byte(body), &lock) != nil {
		return nil
	}
	seen := map[string]bool{}
	var exts []string
	for _, p := range lock.Packages {
		for name := range p.Require {
			ext, ok := strings.CutPrefix(strings.ToLower(name), "ext-")
			if !ok || seen[ext] || !safePackageName(ext) {
				continue
			}
			seen[ext] = true
			exts = append(exts, ext)
		}
	}
	sort.Strings(exts)
	return exts
}

// composerPostInstall runs the scripts composer.json hooks to
// post-install-cmd, once the app's files are in place.
//
// Railpack installs Composer packages with --no-scripts, in a step that has
// only composer.json and the lockfile, and never runs the scripts after.
// `composer install` runs them for everyone else, and apps depend on what
// they do: CakePHP's installer writes config/app_local.php with the security
// salt, and Symfony's demo renames its .env.local.demo into place and builds
// its stylesheet with sass:build. Without them each answered every request
// with a 500 (issue #90). A script that fails here is reported in the build
// log and the build goes on, since the build did not run any before. They run
// in the app's own environment: Railpack sets APP_ENV=production for Laravel
// on every PHP build, and Symfony refused to clear its cache in an
// environment it does not have.
func composerPostInstall(contextDir string) (steer, bool) {
	var manifest struct {
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if json.Unmarshal([]byte(readFile(contextDir, "composer.json")), &manifest) != nil {
		return steer{}, false
	}
	raw, ok := manifest.Scripts["post-install-cmd"]
	if !ok {
		return steer{}, false
	}
	var one string
	var many []string
	if (json.Unmarshal(raw, &one) != nil || one == "") && (json.Unmarshal(raw, &many) != nil || len(many) == 0) {
		return steer{}, false
	}
	return steer{
		Key: "composer-post-install",
		Why: "composer.json runs scripts after `composer install`, which Railpack's install skips, so the build runs them once the app's files are in place",
		Edit: func(plan map[string]any) {
			appendStepCommand(plan, "build", `sh -c 'env -u APP_ENV -u APP_DEBUG composer run-script post-install-cmd --no-interaction || echo "Pando: composer post-install-cmd scripts failed; the build goes on without them" >&2'`)
		},
	}, true
}

var packageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.+\-]*$`)

func safePackageName(s string) bool { return len(s) <= 64 && packageNamePattern.MatchString(s) }

var (
	lockedRack2  = regexp.MustCompile(`(?m)^    rack \(2\.`)
	lockedBase64 = regexp.MustCompile(`(?m)^    base64 \(`)
	lockRuby     = regexp.MustCompile(`(?m)^RUBY VERSION`)
)

// rubyForLockedRack is Ruby 3.3 for a lockfile on Rack 2 that names no Ruby:
// Ruby 3.4 made base64 a gem of its own, which Rack 2 requires without
// declaring, so the app stopped at start with "cannot load such file --
// base64" (issue #90). 3.3 is the last Ruby that ships it.
func rubyForLockedRack(contextDir string) (steer, bool) {
	lock := readFile(contextDir, "Gemfile.lock")
	if !lockedRack2.MatchString(lock) || lockedBase64.MatchString(lock) || lockRuby.MatchString(lock) {
		return steer{}, false
	}
	if gemfileRuby.MatchString(readFile(contextDir, "Gemfile")) ||
		strings.TrimSpace(readFile(contextDir, ".ruby-version")) != "" ||
		strings.Contains(readFile(contextDir, ".tool-versions"), "ruby") {
		return steer{}, false
	}
	return steer{
		Key: "ruby-version:3.3",
		Why: "Gemfile.lock is on Rack 2, which needs the base64 library Ruby 3.4 no longer ships, and names no Ruby, so Ruby is set to 3.3",
		Env: []string{"RAILPACK_RUBY_VERSION=3.3"},
	}, true
}

// --- editing a plan ---------------------------------------------------------

// planSteps is the plan's steps, each as a map.
func planSteps(plan map[string]any) []map[string]any {
	raw, _ := plan["steps"].([]any)
	var out []map[string]any
	for _, s := range raw {
		if step, ok := s.(map[string]any); ok {
			out = append(out, step)
		}
	}
	return out
}

// excludeFromPlan leaves files out of the build: no step copies them, and the
// build context does not carry them, so the image does not either.
func excludeFromPlan(plan map[string]any, files ...string) {
	drop := map[string]bool{}
	for _, f := range files {
		drop[f] = true
	}
	for _, step := range planSteps(plan) {
		commands, ok := step["commands"].([]any)
		if !ok {
			continue
		}
		kept := commands[:0]
		for _, c := range commands {
			if !copiesDropped(c, drop) {
				kept = append(kept, c)
			}
		}
		step["commands"] = kept
	}
	exclude, _ := plan["exclude"].([]any)
	for _, f := range files {
		if !slices.Contains(exclude, any(f)) {
			exclude = append(exclude, f)
		}
	}
	plan["exclude"] = exclude
}

// copiesDropped reports a plan command that copies one of drop from the build
// context rather than from an image.
func copiesDropped(c any, drop map[string]bool) bool {
	cmd, ok := c.(map[string]any)
	src, isCopy := cmd["src"].(string)
	if !ok || !isCopy || !drop[strings.TrimPrefix(src, "./")] {
		return false
	}
	_, fromImage := cmd["image"]
	return !fromImage
}

// replaceInCommands rewrites old to replacement in every command the plan runs.
func replaceInCommands(plan map[string]any, old, replacement string) {
	for _, step := range planSteps(plan) {
		commands, _ := step["commands"].([]any)
		for _, c := range commands {
			cmd, ok := c.(map[string]any)
			if !ok {
				continue
			}
			for _, field := range []string{"cmd", "customName"} {
				if s, ok := cmd[field].(string); ok {
					cmd[field] = strings.ReplaceAll(s, old, replacement)
				}
			}
		}
	}
}

// setStepVariable sets an environment variable for one step.
func setStepVariable(plan map[string]any, stepName, key, value string) {
	for _, step := range planSteps(plan) {
		if step["name"] != stepName {
			continue
		}
		vars, _ := step["variables"].(map[string]any)
		if vars == nil {
			vars = map[string]any{}
		}
		vars[key] = value
		step["variables"] = vars
	}
}

// appendStepCommand runs one more command at the end of a step.
func appendStepCommand(plan map[string]any, stepName, command string) {
	for _, step := range planSteps(plan) {
		if step["name"] != stepName {
			continue
		}
		commands, _ := step["commands"].([]any)
		step["commands"] = append(commands, map[string]any{"cmd": command})
	}
}

var miseToolVersion = regexp.MustCompile(`(?m)^\s*"?([A-Za-z0-9:_\-]+)"?\s*=\s*"([^"]+)"`)

// misePackageVersion is the version of tool the plan's mise configuration
// installs, or "".
func misePackageVersion(body []byte, tool string) string {
	var plan map[string]any
	if json.Unmarshal(body, &plan) != nil {
		return ""
	}
	for _, step := range planSteps(plan) {
		assets, _ := step["assets"].(map[string]any)
		for _, a := range assets {
			content, _ := a.(string)
			for _, m := range miseToolVersion.FindAllStringSubmatch(content, -1) {
				if m[1] == tool {
					return m[2]
				}
			}
		}
	}
	return ""
}

// --- versions ---------------------------------------------------------------

// versionLess compares dotted versions numerically; "" is less than any.
func versionLess(a, b string) bool {
	if a == "" {
		return b != ""
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}
