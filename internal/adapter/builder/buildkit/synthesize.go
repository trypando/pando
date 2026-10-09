package buildkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Strategies that have no Dockerfile in the repository get one written for
// them.
//
// The Dockerfile is the builder's vocabulary, not Pando's (R-251). Core says
// "this app is a directory of files to serve"; what image serves them and how
// is decided here, and a different builder is free to decide differently.
//
// BuildKit already takes the context and the Dockerfile as two separate
// filesystems, so this needs no copy of the source: the context stays the
// checkout and only the generated Dockerfile lives in a temporary directory.

// staticServerImage serves a built static site.
//
// [P]. nginx because it is the smallest well-understood thing that serves a
// directory over HTTP, alpine for the same reason the provisioned services use
// alpine, and pinned to a minor version so a rebuild a year from now produces
// the same server. Listed in design 03 §8 with the other images Pando supplies.
const staticServerImage = "nginx:1.27-alpine"

// staticConfig makes nginx behave the way a single-page app needs.
//
// try_files falling back to index.html is what makes client-side routing work:
// without it, reloading on /admin/apps/123 asks nginx for a file that does not
// exist and gets a 404 — the same trap Pando's own console needed the server to
// handle. A static site with a router is the common case, and a site without
// one is unaffected by the fallback.
//
// absolute_redirect off makes the redirect that adds a directory's trailing
// slash relative. nginx otherwise writes it from the Host header without the
// port, so behind a port-mode address /solutions on localhost:9001 redirected
// to http://localhost/solutions/, where nothing listens (issue #67, R-160).
const staticConfig = `server {
  listen 80;
  absolute_redirect off;
  root /usr/share/nginx/html;
  location / { try_files $uri $uri/ /index.html; }
}
`

// generated is where a synthesized Dockerfile ended up.
type generated struct {
	// Dir holds the Dockerfile. Absolute.
	Dir string

	// Name is the Dockerfile's path relative to Dir.
	Name string

	// Cleanup removes anything temporary. Never removes the checkout: nixpacks
	// writes into it by necessity, and deleting it would take the source with
	// it.
	Cleanup func()
}

// synthesize writes a Dockerfile for a strategy that has none.
func synthesize(req api.BuildRequest, contextDir string) (generated, error) {
	switch req.Strategy {
	case spec.BuildStatic:
		dir, name, err := staticDockerfile(req, contextDir)
		if err != nil {
			return generated{}, err
		}
		return generated{Dir: dir, Name: name, Cleanup: func() { _ = os.RemoveAll(dir) }}, nil

	case spec.BuildBuildpack:
		// Files the spec already carries are replayed, not regenerated. That is
		// what makes the build reproducible and what makes an edited plan
		// actually take effect — regenerating would overwrite it every time.
		if len(req.GeneratedFiles) > 0 {
			if err := writeInto(contextDir, req.GeneratedFiles); err != nil {
				return generated{}, err
			}
			// The same .ruby-version the plan was made with, since a fresh
			// checkout does not have the one planning wrote.
			ensureRubyVersion(contextDir)
			name := req.Dockerfile
			if name == "" {
				name = filepath.Join(".nixpacks", "Dockerfile")
			}
			return generated{
				Dir:     filepath.Dir(filepath.Join(contextDir, name)),
				Name:    filepath.Base(name),
				Cleanup: func() {},
			}, nil
		}

		name, err := buildpackDockerfile(req, contextDir)
		if err != nil {
			return generated{}, err
		}
		// Inside the checkout, which the caller owns and removes.
		return generated{
			Dir:     filepath.Dir(filepath.Join(contextDir, name)),
			Name:    filepath.Base(name),
			Cleanup: func() {},
		}, nil

	default:
		return generated{}, errs.Newf(errs.PlanCapabilityUnsupported,
			"This builder cannot build %q.", req.Strategy)
	}
}

// nixpacksBinary is the generator. R-095: wrap an existing implementation
// rather than reimplementing convention-matching.
//
// It is called only to *plan* — `nixpacks build --out` writes a Dockerfile and
// does not build anything, so nothing here needs a container runtime and R-112
// is not in play. The Dockerfile it writes is BuildKit's to build, and is
// readable afterwards: an app owner can see exactly what was decided instead of
// being told a buildpack happened.
const nixpacksBinary = "nixpacks"

// buildpackDockerfile asks nixpacks what this repository needs.
//
// The output goes into the checkout rather than a temporary directory, and that
// is nixpacks' own contract rather than a shortcut: the Dockerfile it writes
// does `COPY . /app/.` and `COPY .nixpacks/...`, so the build context has to be
// the source *with* the generated directory inside it. The checkout is a
// throwaway clone, so nothing anybody keeps is touched.
func buildpackDockerfile(req api.BuildRequest, contextDir string) (string, error) {
	// A Laravel app, on the PHP its composer.json asks for (laravel.go). Its
	// package.json builds assets for the app, not a site or a server of its
	// own. An answered start command is honored by the runtime, which runs it
	// in place of the image's own.
	if app, ok := readLaravelApp(contextDir); ok {
		return writeLaravelPlan(contextDir, app)
	}
	// A site that builds to static files is planned here rather than by
	// nixpacks (staticsite.go), unless somebody said how it starts — then it
	// is not being served as files.
	if req.StartCommand == "" {
		// A Jekyll site is source for a site, not the site (jekyll.go).
		if site, ok := readJekyllSite(contextDir); ok {
			return writeJekyllPlan(contextDir, site)
		}
		if site, ok := readStaticSiteBuild(contextDir); ok {
			return writeStaticSitePlan(contextDir, site)
		}
		// A site somebody chose to build rather than serve as committed
		// (static_source: run-build). Its directory is already known: it is
		// where the committed copy was found. It had reached nixpacks with
		// nothing to start, and been refused (issue #55).
		if req.StaticDir != "" && safeRelative(req.StaticDir) {
			if _, err := os.Stat(filepath.Join(contextDir, "package.json")); err == nil {
				return writeStaticSitePlan(contextDir, staticSite{
					Framework: "static",
					OutDir:    path.Clean(req.StaticDir),
					Install:   installCommand(contextDir),
					Node:      nodeMajor(contextDir, nil),
				})
			}
		}
	}
	// A Node server, on the official Node image rather than Nix's (node.go).
	if server, ok := readNodeServer(contextDir, req.StartCommand); ok {
		return writeNodeServerPlan(contextDir, server)
	}
	// A Go program, on the Go release the module names (golang.go). Not when
	// somebody said how it starts: that command runs in nixpacks' image, which
	// has the Go toolchain on its PATH.
	if req.StartCommand == "" {
		if prog, ok := readGoBuild(contextDir); ok {
			return writeGoPlan(contextDir, prog)
		}
	}
	// .NET, on the SDK the project targets (dotnet.go).
	if app, ok := readDotnetBuild(contextDir); ok {
		return writeDotnetPlan(contextDir, app)
	}
	// And a JVM project, whose build tool already says how to build it
	// (jvm.go). An answered start command is honored by the runtime, which
	// runs it in place of the image's own.
	if jvm, ok := readJVMBuild(contextDir); ok {
		return writeJVMPlan(contextDir, jvm)
	}
	declared := declaredFor(req, contextDir)
	name, err := planTwice(contextDir, declared, runNixpacks)
	if err == nil {
		return name, nil
	}
	// Nothing for nixpacks to recognize, and a start command that runs a
	// program committed in the repository — a Procfile beside a binary. The
	// program is the app; it needs somewhere to run and nothing to build
	// (issue #55).
	if command := committedProgram(contextDir, declared.Start); command != "" {
		return writeCommittedProgramPlan(contextDir, command)
	}
	return "", err
}

// declaredFor is what the repository and the request together say about the
// build. A start command in the request is a person's answer, and outranks
// anything read out of the repository.
func declaredFor(req api.BuildRequest, contextDir string) declaredBuild {
	declared := readDeclaredBuild(contextDir)
	if req.StartCommand != "" {
		declared.Start = req.StartCommand
	}
	// A Procfile naming Heroku's PHP launcher names a program that exists only
	// on Heroku, and the app exited 127 (issue #55). Its argument is the
	// document root, which nixpacks' own PHP server takes as a setting; the
	// start command becomes that server's.
	if _, ok := herokuPHPRoot(declared.Start); ok && req.StartCommand == "" {
		declared.Start = nixpacksPHPStart
	}
	if declared.Start == "" {
		declared.Start = pythonAppStart(contextDir)
	}
	if declared.Build == "" && declared.Before == "" && djangoCollectsStatic(contextDir) {
		declared.Build = "python manage.py collectstatic --noinput"
	}
	if declared.Start == "" {
		declared.Start = elixirAppStart(contextDir)
	}
	if declared.Start == "" {
		declared.Start = rackAppStart(contextDir)
	}
	return declared
}

// rackAppStart is how a Rack app that is not Rails starts where it can be
// reached. nixpacks runs `rackup`, which binds 127.0.0.1 — inside a container,
// nothing else can reach it (issue #55).
func rackAppStart(contextDir string) string {
	if readFile(contextDir, "config.ru") == "" || fileExists(contextDir, "bin/rails") {
		return ""
	}
	return "bundle exec rackup -o 0.0.0.0 -p ${PORT:-3000}"
}

// djangoCollectsStatic reports a Django project that gathers its static files
// into a STATIC_ROOT.
//
// Collecting them is a build step every platform runs for Django, and nixpacks
// does not: an app using a manifest storage then answered every page with
// "Missing staticfiles manifest entry" (issue #55). Only when the settings name
// a STATIC_ROOT, because without one the command fails, and failing a build
// that would otherwise have run is worse than not collecting.
func djangoCollectsStatic(contextDir string) bool {
	if readFile(contextDir, "manage.py") == "" {
		return false
	}
	settings, _ := filepath.Glob(filepath.Join(contextDir, "*", "settings.py"))
	more, _ := filepath.Glob(filepath.Join(contextDir, "*", "settings", "*.py"))
	for _, f := range append(settings, more...) {
		body, err := os.ReadFile(f)
		if err == nil && strings.Contains(string(body), "STATIC_ROOT") {
			return true
		}
	}
	return false
}

// elixirAppStart is how a Mix project that is not a Phoenix app starts.
//
// nixpacks starts every Elixir project with `mix phx.server`, and a Plug or
// Bandit app has no such task: "The task phx.server could not be found", on a
// loop (issue #55). `mix run --no-halt` starts the application the project
// file names and keeps it running, which is how such an app is run.
func elixirAppStart(contextDir string) string {
	mix := readFile(contextDir, "mix.exs")
	if mix == "" || strings.Contains(mix, ":phoenix") {
		return ""
	}
	return "mix run --no-halt"
}

var (
	fastAPIApp = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*FastAPI\(`)
	flaskApp   = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*Flask\(`)
	runsItself = regexp.MustCompile(`__name__\s*==\s*['"]__main__['"]`)
)

// pythonAppStart is the server a Python web app runs under, when its module
// does not start one itself.
//
// nixpacks starts a repository with a main.py as `python main.py`. A FastAPI or
// Flask module creates the app and leaves serving it to uvicorn or gunicorn, so
// that command imported the app and exited 0, and the deploy reported success
// with nothing listening (issue #55). The server has to be one the app depends
// on: naming one it does not install would fail the same way, later.
func pythonAppStart(contextDir string) string {
	deps := strings.ToLower(readFile(contextDir, "requirements.txt") + readFile(contextDir, "pyproject.toml"))
	for _, module := range []string{"main", "app", "server", "asgi", "wsgi"} {
		body := readFile(contextDir, module+".py")
		if body == "" || runsItself.MatchString(body) {
			continue
		}
		if m := fastAPIApp.FindStringSubmatch(body); m != nil && strings.Contains(deps, "uvicorn") {
			return fmt.Sprintf("uvicorn %s:%s --host 0.0.0.0 --port ${PORT:-8000}", module, m[1])
		}
		if m := flaskApp.FindStringSubmatch(body); m != nil && strings.Contains(deps, "gunicorn") {
			return fmt.Sprintf("gunicorn %s:%s --bind 0.0.0.0:${PORT:-8000}", module, m[1])
		}
	}
	return ""
}

func readFile(dir, name string) string {
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(body)
}

// planner generates a plan and returns where it was written, relative to the
// context directory.
type planner func(contextDir string, args []string) (string, error)

// planTwice asks the planner once, and again when the first answer is what the
// second question needs.
//
// A declaration that names the build replaces what nixpacks would have chosen,
// and one call settles it. A declaration that names an *ordering* does not:
// `//go:embed dist` beside a client that builds into that directory says the
// client comes first and says nothing about what it comes before. So: plan,
// read the chosen build command back out of the generated Dockerfile, plan
// again with the client build ahead of it.
//
// Two subprocesses rather than one, which is the honest cost of not
// reimplementing the thing R-095 says to wrap: the alternative to asking
// nixpacks what it would have chosen is being nixpacks.
//
// The planner is a parameter so this can be tested without one. The composition
// is where the bug was last time — `cd web` outliving the client build — and
// subprocess plumbing is a poor place to hide logic worth checking.
func planTwice(contextDir string, declared declaredBuild, plan planner) (string, error) {
	name, err := plan(contextDir, declared.nixpacksArgs(""))
	if err != nil {
		return "", err
	}
	if !declared.needsSecondPass() {
		return name, nil
	}

	body, readErr := os.ReadFile(filepath.Join(contextDir, name))
	if readErr != nil {
		return "", errs.Wrap(errs.BuildFailed, "Could not read the build plan.", readErr)
	}

	// No build step to come before means there is nothing to wrap, and the
	// first plan stands. A repository whose build nixpacks could not work out
	// is one this has nothing to add to.
	chosen := planBuildCommand(string(body))
	if chosen == "" {
		return name, nil
	}

	return plan(contextDir, declared.nixpacksArgs(chosen))
}

// runNixpacks generates a plan, and returns where it was written.
func runNixpacks(contextDir string, extra []string) (string, error) {
	ensureRubyVersion(contextDir)
	args := append([]string{"build", contextDir, "--out", contextDir}, extra...)
	args = append(args, toolchainDefaults(contextDir)...)

	// G204: exec.Command takes an argv, so nothing here reaches a shell on this
	// host. contextDir is a checkout this process made, and every command in
	// `extra` was read out of the repository and passed safeCommand.
	//
	// Those commands do become the built image's build steps and CMD, which
	// nixpacks runs under `bash -l -c` — so the repository chooses what its own
	// build and container run. That is the same authority a Dockerfile already
	// has, and the container is the boundary either way (R-112, R-114).
	cmd := exec.Command(nixpacksBinary, args...) //nolint:gosec

	// A scrubbed environment, so nothing of Pando's (credentials, the
	// database address) is visible to a generator reading untrusted source.
	// This is not a network sandbox: nixpacks reaches whatever this process
	// can (issue #78). Generation reads the repository and does not need to
	// fetch, but blocking it would take a network namespace the Pando
	// container lacks the privilege to create. What the repository's own code
	// can reach is decided by the BuildKit build, not here (R-112, R-114).
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + contextDir}

	out, err := cmd.CombinedOutput()
	if err != nil {
		// nixpacks' own message, which names the language it could not place.
		// Replacing it with something generic would delete the only useful
		// detail (R-105).
		if errors.Is(err, exec.ErrNotFound) {
			return "", errs.New(errs.PlanCapabilityUnsupported,
				"This installation cannot work out how to build an app that carries no deployment instructions.").
				WithRemedy("Add a Dockerfile to the repository, or ask an administrator to install a build planner.")
		}
		return "", errs.Newf(errs.BuildFailed,
			"Pando could not work out how to build this app: %s", trim(out)).
			WithRemedy("Add a Dockerfile to the repository saying how it should be built.")
	}

	if _, statErr := os.Stat(filepath.Join(contextDir, ".nixpacks", "Dockerfile")); statErr != nil {
		return "", errs.Newf(errs.BuildFailed,
			"Pando could not work out how to build this app: %s", trim(out)).
			WithRemedy("Add a Dockerfile to the repository saying how it should be built.")
	}

	return filepath.Join(".nixpacks", "Dockerfile"), nil
}

// defaultNodeVersion is the Node a JavaScript app gets when it names none.
//
// [P]. nixpacks 1.41 defaults to 18, which is end-of-life, and current Vite,
// Next.js, Angular and NestJS refuse it with EBADENGINE — ten apps in the deploy
// QA run failed on nothing else (issue #55). 22 because it is the newest line
// nixpacks' Nix package set has: asking for 24 failed every build with
// "undefined variable 'nodejs_24'". A plain Node server is built on the
// official Node image instead (node.go), so this only reaches builds nixpacks
// still plans — a Node client inside a Go or Python repository, say.
const defaultNodeVersion = "22"

// toolchainDefaults are the nixpacks settings Pando supplies when the
// repository is silent.
//
// Only when silent: NIXPACKS_NODE_VERSION outranks engines.node and .nvmrc in
// nixpacks, so passing it unconditionally would override what the author
// declared. A .node-version file is read here because nixpacks does not read
// it, and it is the author declaring a version all the same.
func toolchainDefaults(contextDir string) []string {
	var args []string
	if v := nodeDefault(contextDir); v != "" {
		args = append(args, "--env", "NIXPACKS_NODE_VERSION="+v)
	}
	if poetryProject(contextDir) {
		args = append(args, "--env", "NIXPACKS_POETRY_VERSION="+defaultPoetryVersion)
	}
	if root, ok := herokuPHPRoot(procfileWeb(contextDir)); ok && root != "" && safeRelative(root) {
		args = append(args, "--env", "NIXPACKS_PHP_ROOT_DIR=/app/"+root)
	} else if phpServesFromPublic(contextDir) {
		args = append(args, "--env", "NIXPACKS_PHP_ROOT_DIR=/app/public")
	}
	return args
}

func nodeDefault(contextDir string) string {
	if _, err := os.Stat(filepath.Join(contextDir, "package.json")); err != nil {
		return ""
	}
	if declaresNodeVersion(contextDir) {
		return ""
	}
	if _, err := os.Stat(filepath.Join(contextDir, ".nvmrc")); err == nil {
		return ""
	}
	version := defaultNodeVersion
	if body, err := os.ReadFile(filepath.Join(contextDir, ".node-version")); err == nil {
		if v := strings.TrimPrefix(strings.TrimSpace(string(body)), "v"); v != "" && safeVersion(v) {
			version = v
		}
	}
	return version
}

// defaultPoetryVersion is the Poetry a Poetry project is installed with. [P]
//
// nixpacks installs 1.3.1, which refuses any pyproject.toml using settings
// added since — `package-mode`, for one — with "The Poetry configuration is
// invalid" (issue #55). 1.8.5 reads them, and still accepts the `--no-dev` flag
// nixpacks' install step passes, which Poetry 2 removed.
const defaultPoetryVersion = "1.8.5"

func poetryProject(contextDir string) bool {
	if _, err := os.Stat(filepath.Join(contextDir, "poetry.lock")); err == nil {
		return true
	}
	body, err := os.ReadFile(filepath.Join(contextDir, "pyproject.toml"))
	return err == nil && strings.Contains(string(body), "[tool.poetry]")
}

// phpServesFromPublic reports a PHP app whose front controller is in public/,
// the layout Slim, Laravel and Symfony share. nixpacks' web server serves the
// repository root unless told otherwise, which answered 403 (issue #55).
// nixpacksPHPStart is nixpacks' own start command for a PHP app: nginx and
// php-fpm, from the configuration its plan writes into /assets.
const nixpacksPHPStart = "node /assets/scripts/prestart.mjs /assets/nginx.template.conf /nginx.conf && " +
	"(php-fpm -y /assets/php-fpm.conf & nginx -c /nginx.conf)"

var herokuPHP = regexp.MustCompile(`^heroku-php-(apache2|nginx)(\s+.*)?$`)

// herokuPHPRoot reads the document root out of a Heroku PHP launcher command,
// `heroku-php-apache2 web/`. The root is the first argument that is not an
// option; none means the repository root.
func herokuPHPRoot(command string) (string, bool) {
	m := herokuPHP.FindStringSubmatch(strings.TrimSpace(command))
	if m == nil {
		return "", false
	}
	args := strings.Fields(m[2])
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "-C" || arg == "-F" || arg == "-i" || arg == "-l":
			i++ // the option's value: a config file, an ini, a log
		case strings.HasPrefix(arg, "-"):
		default:
			return strings.Trim(arg, "/"), true
		}
	}
	return "", true
}

// procfileWeb is the web process a Procfile declares, or "".
func procfileWeb(contextDir string) string {
	body := readFile(contextDir, "Procfile")
	for _, line := range strings.Split(body, "\n") {
		kind, command, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && strings.TrimSpace(kind) == "web" {
			return strings.TrimSpace(command)
		}
	}
	return ""
}

func phpServesFromPublic(contextDir string) bool {
	if _, err := os.Stat(filepath.Join(contextDir, "composer.json")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(contextDir, "index.php")); err == nil {
		return false
	}
	_, err := os.Stat(filepath.Join(contextDir, "public", "index.php"))
	return err == nil
}

// defaultRubyVersion is the Ruby a project that names none is built with. [P]
// A current stable release.
const defaultRubyVersion = "3.3.6"

var (
	gemfileRuby     = regexp.MustCompile(`(?m)^\s*ruby\s+['"](\d+\.\d+(?:\.\d+)?)['"]`)
	gemfileLockRuby = regexp.MustCompile(`(?m)^RUBY VERSION\s*\n\s*ruby\s+(\d+\.\d+\.\d+)`)
)

// ensureRubyVersion writes .ruby-version into the checkout when a Ruby project
// has none, from what the Gemfile or its lockfile says.
//
// nixpacks reads the Ruby version from .ruby-version and nowhere else, and
// refuses a project without one: "Please specify ruby's version in
// .ruby-version file" (issue #55). The Gemfile's `ruby` line is the same
// declaration in the place Bundler reads it. The checkout is a throwaway clone,
// and the plan nixpacks makes from it is what is stored.
func ensureRubyVersion(contextDir string) {
	if _, err := os.Stat(filepath.Join(contextDir, "Gemfile")); err != nil {
		return
	}
	target := filepath.Join(contextDir, ".ruby-version")
	if _, err := os.Stat(target); err == nil {
		return
	}
	// G306: a build input in the throwaway checkout; it holds no secret.
	_ = os.WriteFile(target, []byte(gemfileRubyVersion(contextDir)+"\n"), 0o644) //nolint:gosec
}

// gemfileRubyVersion is the Ruby the Gemfile or its lockfile names, the
// Gemfile's own line first, or the default.
func gemfileRubyVersion(contextDir string) string {
	version := defaultRubyVersion
	if body, err := os.ReadFile(filepath.Join(contextDir, "Gemfile.lock")); err == nil {
		if m := gemfileLockRuby.FindStringSubmatch(string(body)); m != nil {
			version = m[1]
		}
	}
	if body, err := os.ReadFile(filepath.Join(contextDir, "Gemfile")); err == nil {
		if m := gemfileRuby.FindStringSubmatch(string(body)); m != nil {
			version = m[1]
		}
	}
	return version
}

// declaresNodeVersion reports whether package.json names a Node version.
func declaresNodeVersion(contextDir string) bool {
	body, err := os.ReadFile(filepath.Join(contextDir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Engines map[string]any `json:"engines"`
	}
	if json.Unmarshal(body, &pkg) != nil {
		return false
	}
	v, ok := pkg.Engines["node"].(string)
	return ok && strings.TrimSpace(v) != ""
}

// safeVersion accepts what a version file holds and nothing a flag could be
// made of.
func safeVersion(v string) bool {
	for _, r := range v {
		if (r < '0' || r > '9') && r != '.' && r != 'x' {
			return false
		}
	}
	return len(v) <= 16
}

// planBuildCommand pulls the build step out of a generated plan.
//
// A nixpacks plan runs its phases as RUN lines in order: the environment first
// (`nix-env -if ...`), then install, then build. The build is the last RUN in
// the builder stage that is not one of nixpacks' own bookkeeping lines — the
// `RUN true` it emits for an empty phase, and the nix-env line.
func planBuildCommand(dockerfile string) string {
	var last string
	for _, line := range strings.Split(dockerfile, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "RUN ") {
			// A second FROM starts the runtime stage, and nothing after it
			// builds anything.
			if strings.HasPrefix(trimmed, "FROM ") && last != "" {
				break
			}
			continue
		}
		command := strings.TrimSpace(strings.TrimPrefix(trimmed, "RUN "))
		for strings.HasPrefix(command, "--") {
			_, rest, found := strings.Cut(command, " ")
			if !found {
				command = ""
				break
			}
			command = strings.TrimSpace(rest)
		}
		if command == "" || command == "true" || strings.HasPrefix(command, "nix-env ") {
			continue
		}
		last = command
	}
	return last
}

// writeInto materializes generated build inputs in the checkout.
//
// Into the checkout because they have to be in the build context: the plan's
// Dockerfile references them with COPY. The checkout is a throwaway clone, so
// nothing anybody keeps is written to.
//
// Every path is re-checked here even though spec validation already rejected
// one that climbs out. A spec is exportable and importable, and an imported one
// is untrusted input (design 01 §5) — this is the last point before content
// from it reaches a filesystem, and the cost of checking twice is nothing.
func writeInto(contextDir string, files map[string]string) error {
	for name, content := range files {
		clean := path.Clean("/" + filepath.ToSlash(name))
		rel := strings.TrimPrefix(clean, "/")
		if rel == "" || rel == "." || rel != filepath.ToSlash(name) {
			return errs.Newf(errs.ValidInvalid,
				"A generated build file is written to %q, which is not a path inside the app's source.", name)
		}

		full := filepath.Join(contextDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
		}
		// G306: 0644 is deliberate. This is a generated build input inside the
		// build context, and the rootless builder that reads it runs as a
		// different user. It holds no secret — a secret reaches a build as a
		// build secret, never as a file in the context.
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil { //nolint:gosec
			return errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
		}
	}
	return nil
}

// Plan makes a build plan without building anything, for detection.
//
// The same generator the build path uses, so what somebody reviews is what runs
// (R-102). It writes into a copy of nothing: the source view's directory is the
// detection checkout, which is discarded when detection finishes.
func (a *Adapter) Plan(_ context.Context, src api.SourceView) (map[string]string, string, *api.PlanDeclaration, error) {
	dir, ok := src.(interface{ Root() string })
	if !ok {
		return nil, "", nil, errs.New(errs.BuildFailed, "Planning needs the source on disk.")
	}
	root := dir.Root()

	name, err := buildpackDockerfile(api.BuildRequest{}, root)
	if err != nil {
		return nil, "", nil, err
	}

	files, err := collectPlan(root)
	if err != nil {
		return nil, "", nil, err
	}
	return files, name, readDeclaredBuild(root).asPlanDeclaration(), nil
}

// collectPlan reads back everything the generator wrote, as content.
//
// The spec carries the plan itself rather than a pointer to a directory that
// will not exist next time — so what this misses, the build does not have.
// Subdirectories included: a plan that serves static files puts its web
// server's configuration in .nixpacks/assets, and the Dockerfile it writes
// alongside says COPY .nixpacks/assets /assets/. Reading only the top level
// dropped that directory, and the build failed on the COPY with
// "/.nixpacks/assets: not found" — a plan Pando generated, refusing itself.
func collectPlan(root string) (map[string]string, error) {
	files := map[string]string{}

	// Rooted at the checkout: the walk reads paths a generator wrote, and a
	// symlink among them must not be followed out of the build context.
	// os.Root refuses that at the open rather than trusting the name.
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, errs.Wrap(errs.BuildFailed, "Could not read the build plan.", err)
	}
	defer func() { _ = dir.Close() }()

	err = fs.WalkDir(dir.FS(), ".nixpacks", func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Regular files only: a build input is content either way.
		if !e.Type().IsRegular() {
			return nil
		}
		body, readErr := fs.ReadFile(dir.FS(), name)
		if readErr != nil {
			return readErr
		}
		files[name] = string(body)
		return nil
	})
	if err != nil {
		return nil, errs.Wrap(errs.BuildFailed, "Could not read the build plan.", err)
	}
	return files, nil
}

// planArgs are the build arguments a generated plan says its Dockerfile needs.
//
// nixpacks writes its Dockerfile with ARG lines and records the values beside
// it, in .nixpacks/build.sh, as the docker build command it would have run.
// Pando stored that file and never read it, so every ARG defaulted to empty —
// and a plan that serves a built single-page app resolved its web root to
// `../app/` instead of `../app/dist`, served the repository's source
// index.html, and the site came up blank with a 200 and nothing in the logs.
//
// Read from the plan the spec carries, not from the repository (R-020), and
// overridden by anything the spec sets itself.
func planArgs(files map[string]string) map[string]string {
	args := map[string]string{}
	body, ok := files[path.Join(".nixpacks", "build.sh")]
	if !ok {
		return args
	}

	fields := strings.Fields(body)
	for i, field := range fields {
		var pair string
		switch {
		case field == "--build-arg" && i+1 < len(fields):
			pair = fields[i+1]
		case strings.HasPrefix(field, "--build-arg="):
			pair = strings.TrimPrefix(field, "--build-arg=")
		default:
			continue
		}
		key, value, found := strings.Cut(pair, "=")
		if !found || key == "" {
			continue
		}
		args[key] = strings.Trim(value, `"'`)
	}
	return args
}

// trim bounds a subprocess's output so one runaway generator cannot put a
// megabyte of text into an error envelope.
func trim(out []byte) string {
	const max = 2000
	s := strings.TrimSpace(string(out))
	if len(s) > max {
		return s[len(s)-max:]
	}
	return s
}

// printfFormat turns text into a single-quoted printf format that writes it back
// byte for byte, on one Dockerfile line.
//
// It was written with Go's %q inside `printf '%s'`, which is two quoting systems
// that disagree: the shell expanded `$uri` inside the double quotes to nothing,
// and printf's %s wrote the `\n` escapes literally. nginx refused the resulting
// one-line file with `unknown directive "\n"`, so every static site deployed a
// container that exited at once (issue #55).
//
// Inside single quotes the shell touches nothing, so only printf's own escapes
// matter: backslash and percent are doubled, newlines become `\n`, and a single
// quote closes, escapes and reopens the string.
func printfFormat(text string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `%%`, "\n", `\n`, `'`, `'\''`)
	return "'" + r.Replace(text) + "'"
}

func staticDockerfile(req api.BuildRequest, contextDir string) (string, string, error) {
	// The directory to serve, relative to the build context and constrained to
	// it. A StaticDir of "../../etc" would otherwise copy whatever the build
	// context's parent holds into a public web root.
	serve := path.Clean("/" + filepath.ToSlash(req.StaticDir))
	serve = strings.TrimPrefix(serve, "/")
	if serve == "" || serve == "." {
		serve = "."
	}

	if info, statErr := os.Stat(filepath.Join(contextDir, filepath.FromSlash(serve))); statErr != nil || !info.IsDir() {
		return "", "", errs.Newf(errs.BuildFailed,
			"This app is set to serve %q, and there is no such directory in the repository.",
			req.StaticDir).
			WithRemedy("Check the directory name in the app's configuration, or point it at the folder holding index.html.")
	}

	dir, err := os.MkdirTemp("", "pando-static-*")
	if err != nil {
		return "", "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}

	content := fmt.Sprintf(`FROM %s
COPY %s/ /usr/share/nginx/html/
RUN printf %s > /etc/nginx/conf.d/default.conf
`, staticServerImage, serve, printfFormat(staticConfig))

	// G306: a generated Dockerfile in the build context, read by the rootless
	// builder running as a different user. Not a secret — a secret reaches a
	// build as a build secret, never as a file in the context.
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(content), 0o644); err != nil { //nolint:gosec
		_ = os.RemoveAll(dir)
		return "", "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}
	return dir, "Dockerfile", nil
}
