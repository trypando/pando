package buildkit

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Strategies that have no Dockerfile in the repository get a plan written for
// them.
//
// The plan is the builder's vocabulary, not Pando's (R-251). Core says "this
// app is a directory of files to serve"; what image serves them and how is
// decided here, and a different builder is free to decide differently.
//
// BuildKit already takes the context and the plan as two separate
// filesystems, so this needs no copy of the source: the context stays the
// checkout and only the generated plan lives elsewhere.

// planDir is where a generated plan is written in the checkout: a Dockerfile
// Pando wrote and the files it copies, or a Railpack plan.
//
// Inside the checkout because a Dockerfile Pando writes copies files from
// here, so they have to be in the build context. The checkout is a throwaway
// clone, so nothing anybody keeps is touched.
const planDir = ".pando-plan"

// legacyPlanDir is where plans went before Railpack replaced nixpacks
// (issue #172). A spec stored then carries its files under this name, and is
// replayed from it exactly as stored.
const legacyPlanDir = ".nixpacks"

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

// generated is where a synthesized plan ended up.
type generated struct {
	// Dir holds the plan. Absolute.
	Dir string

	// Name is the plan's path relative to Dir.
	Name string

	// Cleanup removes anything temporary. Never removes the checkout: plans
	// are written into it by necessity, and deleting it would take the source
	// with it.
	Cleanup func()
}

// synthesize writes a plan for a strategy that has none.
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
			name := req.Dockerfile
			if name == "" {
				name = filepath.Join(legacyPlanDir, "Dockerfile")
			}
			// The same .ruby-version a nixpacks plan was made with, since a
			// fresh checkout does not have the one planning wrote. Only for a
			// plan stored before Railpack: Railpack's mise reads the file too,
			// and one written here overrode the Ruby its plan names, so gems
			// were compiled for one Ruby and run on another ("missing
			// extensions", issue #172).
			if !isRailpackPlan(name) {
				ensureRubyVersion(contextDir)
			}
			if err := writePlanIgnore(contextDir, name); err != nil {
				return generated{}, err
			}
			return generated{
				Dir:     filepath.Dir(filepath.Join(contextDir, name)),
				Name:    filepath.Base(name),
				Cleanup: func() {},
			}, nil
		}

		name, err := buildpackPlan(req, contextDir)
		if err == nil {
			err = writePlanIgnore(contextDir, name)
		}
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

// buildpackPlan works out what this repository needs, and returns where the
// plan was written, relative to the checkout.
//
// The stacks below are planned by Pando, as Dockerfiles, because the deploy QA
// run found the general-purpose planner getting each one wrong (issue #55).
// Everything else goes to Railpack (R-095).
func buildpackPlan(req api.BuildRequest, contextDir string) (string, error) {
	return buildpackPlanSteered(req, contextDir, proactiveSteers(contextDir))
}

// buildpackPlanSteered is buildpackPlan with Railpack steered (steer.go): the
// steers the repository calls for, and any a failed build's log added.
func buildpackPlanSteered(req api.BuildRequest, contextDir string, steers []steer) (string, error) {
	// A Rust crate whose path dependencies are above it, built from where
	// they are (rust.go). First, because the plan builds from a different
	// directory than every other, and Plan reports it (BuildPlan.Context).
	if crate, ok := readRustPathCrate(contextDir); ok {
		return writeRustPathCratePlan(contextDir, crate)
	}
	// A Laravel app, on the PHP its composer.json asks for (laravel.go). Its
	// package.json builds assets for the app, not a site or a server of its
	// own. An answered start command is honored by the runtime, which runs it
	// in place of the image's own.
	if app, ok := readLaravelApp(contextDir); ok {
		return writeLaravelPlan(contextDir, app)
	}
	// A Meteor app, built into a bundle by Meteor's own tool (meteor.go).
	if app, ok := readMeteorApp(contextDir); ok {
		return writeMeteorPlan(contextDir, app)
	}
	// A site that builds to static files is planned here rather than by
	// Railpack (staticsite.go), unless somebody said how it starts — then it
	// is not being served as files.
	if req.StartCommand == "" {
		if name, ok, err := siteBuildPlan(req, contextDir); ok {
			return name, err
		}
	}
	// A Node server, on the official Node image (node.go).
	if server, ok := readNodeServer(contextDir, req.StartCommand); ok {
		return writeNodeServerPlan(contextDir, server)
	}
	// A Go program, on the Go release the module names (golang.go). Not when
	// somebody said how it starts: that command runs in Railpack's image,
	// which has the Go toolchain on its PATH.
	if req.StartCommand == "" {
		if prog, ok := readGoBuild(contextDir); ok {
			return writeGoPlan(contextDir, prog)
		}
	}
	// A Cargo workspace with one program in it, built from its root (rust.go).
	// An answered start command runs in place of the image's own.
	if ws, ok := readRustWorkspace(contextDir); ok {
		return writeRustWorkspacePlan(contextDir, ws)
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
	// The languages Railpack has no provider for, or plans as something they
	// are not — a Crystal app's public/ served as a static site (languages.go).
	if name, ok, err := languagePlan(contextDir); ok {
		return name, err
	}
	declared := declaredFor(req, contextDir)
	// Railpack is handed a PHP version it can resolve, for the moment it plans
	// (railpackPHPConstraint); the repository's own composer.json is what
	// the build installs against.
	restore := pinRailpackPHP(contextDir)
	name, err := planTwice(contextDir, declared, steeredPlanner(runRailpack, steers))
	restore()
	if err == nil {
		return name, nil
	}
	// Nothing for Railpack to recognize, and a start command that runs a
	// program committed in the repository — a Procfile beside a binary. The
	// program is the app; it needs somewhere to run and nothing to build
	// (issue #55).
	if command := committedProgram(contextDir, declared.Start); command != "" {
		return writeCommittedProgramPlan(contextDir, command)
	}
	return "", err
}

// siteBuildPlan plans a site that builds to static files, when the repository
// is one; ok is false when it is not.
func siteBuildPlan(req api.BuildRequest, contextDir string) (name string, ok bool, err error) {
	// A Jekyll site is source for a site, not the site (jekyll.go).
	if site, found := readJekyllSite(contextDir); found {
		name, err = writeJekyllPlan(contextDir, site)
		return name, true, err
	}
	// So are Hugo, Zola and MkDocs sites (ssg.go). Ahead of the Node site: a
	// Hugo site's package.json, when it has one, holds its test tools.
	if site, found, readErr := readSSGSite(contextDir); readErr != nil || found {
		if readErr != nil {
			return "", true, readErr
		}
		name, err = writeSSGPlan(contextDir, site)
		return name, true, err
	}
	if site, found := readStaticSiteBuild(contextDir); found {
		name, err = writeStaticSitePlan(contextDir, site)
		return name, true, err
	}
	// A site somebody chose to build rather than serve as committed
	// (static_source: run-build). Its directory is already known: it is
	// where the committed copy was found. It had reached the planner with
	// nothing to start, and been refused (issue #55).
	if req.StaticDir == "" || !safeRelative(req.StaticDir) || !fileExists(contextDir, "package.json") {
		return "", false, nil
	}
	name, err = writeStaticSitePlan(contextDir, staticSite{
		Framework: "static",
		OutDir:    path.Clean(req.StaticDir),
		Install:   installCommand(contextDir),
		Node:      nodeMajor(contextDir, nil),
	})
	return name, true, err
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
	// on Heroku, and the app exited 127 (issue #55). Railpack reads a Procfile
	// itself and lets it replace the start command, so the launcher would
	// still be run; its argument is the document root, which Railpack's PHP
	// server takes as a setting (railpackDefaults), and the start command
	// becomes that server's.
	if _, ok := herokuPHPRoot(declared.Start); ok && req.StartCommand == "" {
		declared.Start = railpackPHPStart
	} else if _, ok := herokuPHPRoot(procfileWeb(contextDir)); ok && declared.Start == "" {
		declared.Start = railpackPHPStart
	}
	if req.StartCommand == "" {
		declared = withProcfileProgram(contextDir, declared)
	}
	// What the framework implies, where nothing declared a start: a server for
	// a Python app that does not serve itself (python.go), Puma for a Rack 3
	// app without rackup, Composer's start script for PHP (starts.go, php.go).
	for _, implied := range []func(string) string{pythonAppStart, rackStart, composerStart} {
		if declared.Start != "" {
			break
		}
		declared.Start = implied(contextDir)
	}
	// Not when the start command collects them itself: Railway's Django
	// template runs collectstatic at start, where its settings can read the
	// database variables they need at import (os.environ["PGDATABASE"]), and
	// the same command run at build stopped it on a KeyError (issue #90).
	if declared.Build == "" && declared.Before == "" && !strings.Contains(declared.Start, "collectstatic") &&
		djangoCollectsStatic(contextDir) {
		declared.Build = "python manage.py collectstatic --noinput"
	}
	return declared
}

// djangoCollectsStatic reports a Django project that gathers its static files
// into a STATIC_ROOT.
//
// Collecting them is a build step every platform runs for Django, and neither
// nixpacks nor Railpack does: an app using a manifest storage then answered
// every page with "Missing staticfiles manifest entry" (issue #55). Only when
// the settings name a STATIC_ROOT, because without one the command fails, and
// failing a build that would otherwise have run is worse than not collecting.
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

func readFile(dir, name string) string {
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(body)
}

// planner generates a plan and returns where it was written, relative to the
// context directory.
type planner func(contextDir string, env []string) (string, error)

// planTwice asks the planner once, and again when the first answer is what the
// second question needs.
//
// A declaration that names the build replaces what Railpack would have chosen,
// and one call settles it. A declaration that names an *ordering* does not:
// `//go:embed dist` beside a client that builds into that directory says the
// client comes first and says nothing about what it comes before. So: plan,
// read the chosen build command back out of the plan, plan again with the
// client build ahead of it.
//
// Two subprocesses rather than one, which is the honest cost of not
// reimplementing the thing R-095 says to wrap: the alternative to asking
// Railpack what it would have chosen is being Railpack.
//
// The planner is a parameter so this can be tested without one. The composition
// is where the bug was last time — `cd web` outliving the client build — and
// subprocess plumbing is a poor place to hide logic worth checking.
func planTwice(contextDir string, declared declaredBuild, plan planner) (string, error) {
	env := append(declared.railpackEnv(""), railpackDefaults(contextDir)...)
	name, err := plan(contextDir, env)
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
	// first plan stands. A repository whose build Railpack could not work out
	// is one this has nothing to add to.
	chosen := railpackBuildCommand(body)
	if chosen == "" {
		return name, nil
	}

	env = append(declared.railpackEnv(chosen), railpackDefaults(contextDir)...)
	return plan(contextDir, env)
}

// railpackDefaults are the Railpack settings Pando supplies for what Railpack
// would otherwise get wrong.
func railpackDefaults(contextDir string) []string {
	if root, ok := herokuPHPRoot(procfileWeb(contextDir)); ok && root != "" && safeRelative(root) {
		return []string{"RAILPACK_PHP_ROOT_DIR=/app/" + root}
	}
	if root := phpServedFrom(contextDir); root != "" {
		return []string{"RAILPACK_PHP_ROOT_DIR=/app/" + root}
	}
	return railpackNodeDefaults(contextDir)
}

// railpackPHPStart is Railpack's own start command for a PHP app: FrankenPHP,
// from the script its plan writes into the image.
const railpackPHPStart = "/start-container.sh"

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

// phpServedFrom is the directory a Composer PHP app's front controller is
// in, when it is not the repository root: public/, the layout Slim, Laravel
// and Symfony share, or another web root (phpWebRoot). Railpack serves public/
// only for a Laravel app, and the repository root otherwise, which answered
// 403 (issue #55), or 404 for Nette's www/ (issue #90).
func phpServedFrom(contextDir string) string {
	if _, err := os.Stat(filepath.Join(contextDir, "composer.json")); err != nil {
		return ""
	}
	return phpWebRoot(contextDir)
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
// nixpacks read the Ruby version from .ruby-version and nowhere else, and a
// plan it made reads it again at build time (issue #55). Railpack reads the
// Gemfile itself; this is kept for plans stored before it. The checkout is a
// throwaway clone.
func ensureRubyVersion(contextDir string) {
	if !statContext(contextDir, "Gemfile") || statContext(contextDir, ".ruby-version") {
		return
	}
	_ = writeContextFile(contextDir, ".ruby-version", []byte(gemfileRubyVersion(contextDir)+"\n"))
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

// writeInto materializes generated build inputs in the checkout.
//
// Into the checkout because they have to be in the build context: a Dockerfile
// Pando wrote references them with COPY. The checkout is a throwaway clone, so
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

		if err := writeContextFile(contextDir, filepath.FromSlash(rel), []byte(content)); err != nil {
			return errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
		}
	}
	return nil
}

// Plan makes a build plan without building anything, for detection.
//
// The same generator the build path uses, so what somebody reviews is what runs
// (R-102). It writes into a copy of nothing: the source view's directory is the
// detection checkout, which is discarded when detection finishes.
func (a *Adapter) Plan(_ context.Context, src api.SourceView) (api.BuildPlan, error) {
	dir, ok := src.(interface{ Root() string })
	if !ok {
		return api.BuildPlan{}, errs.New(errs.BuildFailed, "Planning needs the source on disk.")
	}
	root := dir.Root()

	name, err := buildpackPlan(api.BuildRequest{}, root)
	if err == nil {
		// Collected with the plan, so the spec carries it and the owner sees it.
		err = writePlanIgnore(root, name)
	}
	if err != nil {
		return api.BuildPlan{}, err
	}

	files, err := collectPlan(root)
	if err != nil {
		return api.BuildPlan{}, err
	}
	plan := api.BuildPlan{
		Files:    files,
		Entry:    filepath.ToSlash(name),
		Declared: readDeclaredBuild(root).asPlanDeclaration(),
	}
	if crate, ok := readRustPathCrate(root); ok {
		plan.Context = crate.Context()
	}
	// A Railpack plan says how the app starts in its own terms; a Dockerfile
	// is left for detection to read the way it reads any Dockerfile.
	if isRailpackPlan(name) {
		plan.Start = railpackStartCommand([]byte(files[plan.Entry]))
		// A start command that runs a script the repository does not have is
		// a guess, not an answer, and detection asks instead. Railpack starts
		// a Node app with no start script as `node <main>`, and main defaults
		// to an index.js the app may keep elsewhere (src/index.js), so the
		// app exited with "Cannot find module '/app/index.js'" (issue #172).
		if runsMissingScript(root, plan.Start) {
			plan.Start = ""
		}
		// A framework whose own port is not Python's usual 8000: Streamlit
		// serves 8501, a Reflex frontend 3000 (issue #90).
		if port := pythonAppPort(root); port != 0 {
			plan.Ports = []int{port}
		}
	}
	return plan, nil
}

// scriptRunners are the commands whose first argument is a script in the
// repository.
var scriptRunners = map[string]bool{
	"node": true, "bun": true, "deno": true, "python": true, "python3": true, "ruby": true, "php": true,
}

// runsMissingScript reports a start command of the form `node path/to/x.js`
// whose script is not in the repository.
func runsMissingScript(root, start string) bool {
	fields := strings.Fields(start)
	if len(fields) < 2 || !scriptRunners[fields[0]] {
		return false
	}
	for _, arg := range fields[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if path.Ext(arg) == "" || strings.ContainsAny(arg, "$`") {
			return false
		}
		return !fileExists(root, arg)
	}
	return false
}

// collectPlan reads back everything the generator wrote, as content.
//
// The spec carries the plan itself rather than a pointer to a directory that
// will not exist next time — so what this misses, the build does not have.
// Subdirectories included: a plan that serves static files puts its web
// server's configuration in a subdirectory, and the Dockerfile it writes
// alongside copies it. Reading only the top level dropped that directory, and
// the build failed on the COPY — a plan Pando generated, refusing itself.
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

	err = fs.WalkDir(dir.FS(), planDir, func(name string, e fs.DirEntry, err error) error {
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

// planArgs are the build arguments a stored nixpacks plan says its Dockerfile
// needs.
//
// nixpacks wrote its Dockerfile with ARG lines and recorded the values beside
// it, in .nixpacks/build.sh, as the docker build command it would have run.
// Pando stored that file and never read it, so every ARG defaulted to empty —
// and a plan that serves a built single-page app resolved its web root to
// `../app/` instead of `../app/dist`, served the repository's source
// index.html, and the site came up blank with a 200 and nothing in the logs.
//
// Plans are no longer made by nixpacks (issue #172), but a spec stored before
// then is still built from the plan it carries. Read from that plan, not from
// the repository (R-020), and overridden by anything the spec sets itself.
func planArgs(files map[string]string) map[string]string {
	args := map[string]string{}
	body, ok := files[path.Join(legacyPlanDir, "build.sh")]
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
		return "", "", errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
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
		return "", "", errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
	}
	// The directory served is the one the spec names, whatever the
	// repository's .dockerignore says (dockerignore.go).
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile.dockerignore"), []byte(planIgnore), 0o644); err != nil { //nolint:gosec
		_ = os.RemoveAll(dir)
		return "", "", errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
	}
	return dir, "Dockerfile", nil
}
