package detect

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// QuestionKindChoice is a small indirection so question.go does not import api.
func QuestionKindChoice() api.QuestionKind { return api.QuestionChoice }

// --- Dockerfile ------------------------------------------------------------

// DockerfileDetector recognizes a repository that says how to build itself.
type DockerfileDetector struct{}

func (DockerfileDetector) Name() string { return "dockerfile" }

// dockerfileNames are the names a container build file goes by. Containerfile
// is Podman's and Buildah's, and means the same thing.
var dockerfileNames = []string{"Dockerfile", "Containerfile"}

func (d DockerfileDetector) Bid(_ context.Context, src api.SourceView) (Candidate, error) {
	var nested []string
	for _, name := range dockerfileNames {
		if root, err := src.Stat(name); err == nil && !root.IsDir {
			return d.bidForRoot(src, name)
		}
		found, _ := src.Glob(name)
		for _, p := range deployable(found) {
			if p != name {
				nested = append(nested, p)
			}
		}
	}

	// A Dockerfile among helper tools counts only when there is no other.
	var tools []string
	nested, tools = splitAuxiliary(nested)
	if len(nested) == 0 && len(tools) == 1 {
		c, err := d.bidForRoot(src, tools[0])
		c.Confidence = 0.5
		c.Evidence[0] = tools[0] + " is the only Dockerfile in the repository, and it sits among helper tools rather than the app"
		return c, err
	}
	if len(nested) == 0 {
		nested = tools
	}

	switch {
	case len(nested) == 1:
		// One Dockerfile, in a subdirectory. There is nothing to choose between,
		// and asking "which of these one files" is a question with one answer.
		// Built from the repository root, the way `docker build -f
		// docker/Dockerfile .` is written in nearly every README that has one.
		c, err := d.bidForRoot(src, nested[0])
		c.Confidence = 0.85
		c.Evidence[0] = nested[0] + " is the only Dockerfile in the repository"
		return c, err

	case len(nested) > 0:
		// A Dockerfile exists but not at the root. Pando does not pick one —
		// which service a monorepo means to deploy is not something the file
		// layout answers (R-021).
		sort.Strings(nested)
		return Candidate{
			Strategy:   spec.BuildDockerfile,
			Confidence: 0.45,
			Evidence:   []string{fmt.Sprintf("%d Dockerfiles, none at the repository root", len(nested))},
			Questions: []Question{{
				Key:     "dockerfile_path",
				Kind:    api.QuestionChoice,
				Options: nested,
				Prompt: fmt.Sprintf(
					"This repository contains %d Dockerfiles and none at the top level, so Pando could not "+
						"tell which one builds the app you want to deploy. The files are: %s. "+
						"Valid answer: one of those paths.",
					len(nested), strings.Join(truncate(nested, 8), ", ")),
				Why: "Pando builds one app per repository and needs to know which Dockerfile describes it.",
			}, {
				Key:  KeyPrimaryPort,
				Kind: api.QuestionPort,
				Prompt: "This app builds from a Dockerfile, and which of its Dockerfiles is chosen decides " +
					"which port the app listens on. Pando needs that port to send traffic to the app. " +
					"Valid answer: a port number, such as 3000 or 8080.",
				Why: "Pando needs to know where to send traffic once the app is running.",
			}},
			// A workload, so that answering the question produces something to
			// run. Without one the answer was recorded and accept refused the
			// result with "This app has no workloads" (issue #55). Which port
			// depends on which file is chosen, so it is asked alongside.
			Draft: Draft{
				Build:     spec.Build{Strategy: spec.BuildDockerfile},
				Workloads: []spec.Workload{{Name: "web", Primary: true, Exposed: true}},
			},
		}, nil

	default:
		// Either there are no Dockerfiles, or every one of them sits somewhere
		// that is not this repository's app. Both are "nothing to bid on":
		// offering a choice between five Dockerfiles that all build something
		// else is worse than saying nothing, because R-005 says the person
		// choosing may not know what a Dockerfile is.
		return Candidate{Confidence: 0}, nil
	}
}

// notDeployable are directories whose contents demonstrate, test, or develop
// something rather than being the thing this repository deploys.
//
// This exists because a file's presence is not evidence on its own — where it
// sits is part of what it means. vercel/turbo carries five Dockerfiles and not
// one of them builds turbo: two are sample projects under examples/, two are
// lockfile test fixtures, and one is a devcontainer. A detector that reads
// "5 Dockerfiles" as "this repository builds with a Dockerfile" has mistaken
// file-counting for understanding.
var notDeployable = []string{
	"example", "examples", "sample", "samples",
	"test", "tests", "testdata", "fixture", "fixtures", "__tests__",
	"doc", "docs", "template", "templates", "demo", "demos",
	".devcontainer", ".github", "vendor", "node_modules", "third_party",

	// Editor and cloud-workspace environments: a Dockerfile here describes
	// the machine somebody edits the code on. Astro's examples each carry a
	// `.codesandbox/Dockerfile` that is one `FROM node` line, and Pando
	// deployed it — a container that opened no port — instead of building
	// the site (issue #90).
	".codesandbox", ".gitpod", ".idx", ".vscode", ".circleci", ".gitlab",
	"e2e", "benchmark", "benchmarks",
}

// auxiliary are directories of helper tools beside the app: a Dockerfile
// there builds a tool, not usually the thing the repository deploys. Uptime
// Kuma's `extra/uptime-kuma-push/Dockerfile` builds a push client, and was
// offered on a near-even footing with the compose file that runs the app
// (issue #90). Unlike notDeployable it is not excluded — `contrib/docker` is
// some projects' real build — but it ranks below any other Dockerfile and
// bids low on its own.
var auxiliary = []string{"extra", "extras", "contrib", "tools", "tool", "hack", "scripts", "script", "utils", "util", "misc"}

// splitAuxiliary separates the paths under an auxiliary directory.
func splitAuxiliary(paths []string) (app, tools []string) {
	for _, p := range paths {
		if underAuxiliary(p) {
			tools = append(tools, p)
		} else {
			app = append(app, p)
		}
	}
	return app, tools
}

func underAuxiliary(p string) bool {
	for _, segment := range strings.Split(path.Dir(path.Clean(p)), "/") {
		lower := strings.ToLower(segment)
		for _, aux := range auxiliary {
			if lower == aux {
				return true
			}
		}
	}
	return false
}

// deployable filters out paths that live under a directory in notDeployable.
func deployable(paths []string) []string {
	var kept []string
	for _, p := range paths {
		if !underNotDeployable(p) {
			kept = append(kept, p)
		}
	}
	return kept
}

func underNotDeployable(p string) bool {
	for _, segment := range strings.Split(path.Clean(p), "/") {
		lower := strings.ToLower(segment)
		for _, skip := range notDeployable {
			if lower == skip {
				return true
			}
		}
	}
	return false
}

func (d DockerfileDetector) bidForRoot(src api.SourceView, name string) (Candidate, error) {
	c := Candidate{
		Strategy:   spec.BuildDockerfile,
		Confidence: 0.92,
		Evidence:   []string{name + " at repository root"},
		Draft:      Draft{Build: spec.Build{Strategy: spec.BuildDockerfile, Dockerfile: name}},
	}

	ports, cmd := readDockerfile(src, name)
	workload := spec.Workload{Name: "web", Primary: true, Exposed: true}

	for _, p := range ports {
		// EXPOSE is a declaration by the person who wrote the Dockerfile, which
		// is better evidence than a framework guess and worse than watching the
		// app bind. The source is recorded so the review UI can say which
		// (design 01 §2.3).
		workload.Ports = append(workload.Ports, spec.Port{
			Number: p, Protocol: "http", Source: spec.PortExpose,
		})
	}
	if len(ports) > 0 {
		c.Evidence = append(c.Evidence, fmt.Sprintf("EXPOSE %s", joinInts(ports)))
		c.Confidence = 0.95
	} else {
		// No EXPOSE. The trial run may still observe a bound port (R-097), so
		// this is a question the trial run can answer rather than one the user
		// must.
		c.Questions = append(c.Questions, Question{
			Key:      "primary_port",
			Kind:     api.QuestionPort,
			Deferred: true,
			Prompt: "This app builds from a Dockerfile that does not declare which port it listens on. " +
				"Pando will watch the app start and try to work it out, but if that does not " +
				"succeed it needs to be told. Valid answer: a port number, such as 3000 or 8080.",
			Why: "Pando needs to know where to send traffic once the app is running.",
		})
	}
	if cmd != "" {
		c.Evidence = append(c.Evidence, "CMD "+cmd)
	}
	required, empty := buildArgNeeds(src, name)
	for _, arg := range empty {
		c.Draft.Build.Args = append(c.Draft.Build.Args, spec.KV{Key: arg})
		c.Evidence = append(c.Evidence, fmt.Sprintf(
			"build argument %s is passed as empty: the build reads it under `set -u`, which stops on a value that was never set", arg))
	}
	for _, arg := range required {
		c.Questions = append(c.Questions, Question{
			Key:  BuildArgKeyPrefix + arg,
			Kind: api.QuestionText,
			Prompt: fmt.Sprintf(
				"This app builds from a Dockerfile that declares a build argument named %s with no default "+
					"value, and stops the build when it is not given one. Pando needs the value to build the "+
					"app. Valid answer: the value to pass as %s, exactly as it would follow "+
					"`docker build --build-arg %s=`.", arg, arg, arg),
			Why: "The build cannot run without it.",
		})
	}

	c.Draft.Workloads = []spec.Workload{workload}
	c.Draft.Slots, c.Draft.Workloads[0].Env = readEnvExample(src)
	var requiredVars []string
	c.Draft, requiredVars = withSourceRequirements(src, c.Draft, ".", name)
	c.Evidence = append(c.Evidence, requiredVars...)
	return c, nil
}

// --- Compose ---------------------------------------------------------------

// ComposeDetector recognizes a repository that says how its parts fit together.
type ComposeDetector struct{}

func (ComposeDetector) Name() string { return "compose" }

var composeNames = []string{
	"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml",
}

func (d ComposeDetector) Bid(_ context.Context, src api.SourceView) (Candidate, error) {
	var found string
	for _, name := range composeNames {
		if info, err := src.Stat(name); err == nil && !info.IsDir {
			found = name
			break
		}
	}
	if found == "" || legacyCompose(src, found) {
		return Candidate{Confidence: 0}, nil
	}

	c := Candidate{
		Strategy:   spec.BuildCompose,
		Confidence: 0.88,
		Evidence:   []string{found + " at repository root"},
	}

	// R-096: a compose file is a complete answer, not a hint. The import is the
	// bid — there is nothing left to infer once the author has said what runs.
	draft, err := ImportCompose(src, found)
	if err != nil {
		// The bid stands, carrying the reason. A compose file at the root is
		// still the right reading of this repository even when it cannot be
		// imported, and "here is the construct that cannot run" is a better
		// answer than quietly bidding buildpack instead (R-099).
		c.Blocked = err
		c.Draft = Draft{Build: spec.Build{Strategy: spec.BuildCompose, ComposeFile: found}}
		//nolint:nilerr // Returning the error here would make the auction drop
		// this detector, which is right for a detector that broke and wrong for
		// one that worked and found a reason. The reason travels on the
		// candidate instead, and the auction surfaces it as StatusBlocked.
		return c, nil
	}
	c.Draft = composeRequirements(src, draft)

	// A compose file of nothing but databases is not the app. It is what the
	// app needs while somebody works on it — Spring Petclinic's runs a MySQL
	// and a Postgres beside `./mvnw spring-boot:run` — and taking it as the
	// app deployed two databases and nothing to open (issue #55). It still
	// bids, low, so the review can show it was read.
	if onlyBackingServices(draft) {
		c.Confidence = 0.2
		c.Evidence = append(c.Evidence,
			"every service is a database, so this describes what the app needs rather than the app")
	}

	services := make([]string, 0, len(draft.Workloads))
	for _, w := range draft.Workloads {
		services = append(services, w.Name)
	}
	if len(services) > 0 {
		c.Evidence = append(c.Evidence,
			fmt.Sprintf("%d services: %s", len(services), strings.Join(truncate(services, 6), ", ")))
	}
	if len(draft.Volumes) > 0 {
		c.Evidence = append(c.Evidence, fmt.Sprintf("%d declared volume(s)", len(draft.Volumes)))
	}

	// A compose file describes several services; exactly one is the app's
	// canonical endpoint (R-026). Which one is not something the file says, so
	// Pando asks rather than picking the first.
	if len(services) > 1 {
		c.Questions = append(c.Questions, Question{
			Key:     "primary_service",
			Kind:    api.QuestionChoice,
			Options: services,
			Prompt: fmt.Sprintf(
				"This app is made of %d parts that run together: %s. Pando needs to know which one "+
					"serves the app's web interface — the one a person opens in a browser. "+
					"Valid answer: one of those names.",
				len(services), strings.Join(services, ", ")),
			Why: "Pando gives the app one address, and sends traffic there.",
		})
	}

	return c, nil
}

// onlyBackingServices reports a compose draft whose every service is one Pando
// recognizes as a backing service.
func onlyBackingServices(d Draft) bool {
	if len(d.Workloads) == 0 {
		return len(d.Slots) > 0
	}
	for _, w := range d.Workloads {
		if _, ok := backingService(w.Name, w.Image); !ok || w.Build != nil {
			return false
		}
	}
	return true
}

// --- Static ----------------------------------------------------------------

// StaticDetector recognizes a site with no server.
type StaticDetector struct{}

func (StaticDetector) Name() string { return "static" }

func (d StaticDetector) Bid(_ context.Context, src api.SourceView) (Candidate, error) {
	// An index.html at the root, or in one of the usual output directories.
	// docs/ is where GitHub Pages serves a site from, and a repository that
	// keeps its site there was asked how to build it (issue #55).
	// build/ is where Create React App and most generators write, and www/ is
	// Cordova's and Ionic's site; committed with no package.json, either was
	// asked how to build (issue #90).
	candidates := []string{"index.html", "public/index.html", "dist/index.html", "site/index.html",
		"docs/index.html", "build/index.html", "www/index.html"}

	var dir string
	for _, candidate := range candidates {
		if info, err := src.Stat(candidate); err == nil && !info.IsDir && !isPageTemplate(src, candidate) {
			dir = path.Dir(candidate)
			break
		}
	}
	if dir == "" {
		return Candidate{Confidence: 0}, nil
	}

	confidence := 0.7
	evidence := []string{"index.html in " + displayDir(dir)}
	var questions []Question

	// A package.json alongside means there is probably a build step. What that
	// implies depends entirely on where the index.html was found, and reading it
	// as one thing was wrong:
	//
	//   - at the repository root, the file is source. Serving the source
	//     directory ships an unbuilt site, so bid low and let buildpack win.
	//   - in dist/ or public/, the file is build output that is committed and
	//     already there. That is stronger evidence than no package.json at all,
	//     not weaker — the site is built and sitting in the repository.
	//
	// h5bp/html5-boilerplate is the second case, and the blanket penalty had it
	// losing to a buildpack that would have tried to npm-start a folder of HTML.
	if _, err := src.Stat("package.json"); err == nil {
		switch {
		case dir == "." && hasBuildScript(src):
			// A root index.html next to a declared build command is source, not
			// output — the file almost certainly references modules that only
			// resolve after the build. Serving it unbuilt produces a blank page,
			// so this bid should lose to buildpack outright rather than land
			// close enough to it to make the auction ask which.
			confidence = 0.2
			evidence = append(evidence,
				"package.json declares a build command, so the repository root is source rather than a built site")

		case dir == ".":
			// A package.json with no build command may just be a dependency
			// manifest beside a page that is already servable. Uncertain rather
			// than wrong.
			confidence = 0.35
			evidence = append(evidence, "package.json is also present, so this may need building first")

		case hasBuildScript(src):
			// Committed output plus a build script is a genuine fork: serve what
			// is in the repository, or run the build and serve what comes out.
			// The committed copy can be stale, and that failure is silent — the
			// site deploys and looks fine while being several commits behind.
			evidence = append(evidence,
				"package.json declares a build script, so "+dir+"/ may be older than the source")
			questions = append(questions, Question{
				Key:     "static_source",
				Kind:    api.QuestionChoice,
				Options: []string{"serve-committed", "run-build"},
				Prompt: fmt.Sprintf(
					"This looks like a static website. A built copy of it is already committed in the "+
						"%s/ directory, but the project also declares a build command, so that copy may be "+
						"older than the rest of the source. Pando can either publish the committed copy as "+
						"it is, or run the build first and publish what it produces. "+
						"Valid answer: either \"serve-committed\" or \"run-build\".",
					dir),
				Why: "Publishing a stale copy of a site usually looks like it worked, which is why Pando asks rather than choosing.",
			})
		}
	}

	// A Jekyll site at the root is source in the same way: GitHub Pages builds
	// it before serving it, and serving it unbuilt answers every Markdown page
	// with a 403 or the Markdown itself (issue #67). The buildpack bid builds
	// it, and this one steps aside for it.
	if dir == "." && isJekyllSource(src) {
		confidence = 0.2
		evidence = append(evidence,
			"_config.yml marks this as a Jekyll site, which is built before it is served")
	}

	return Candidate{
		Strategy:   spec.BuildStatic,
		Confidence: confidence,
		Evidence:   evidence,
		Questions:  questions,
		Draft: Draft{
			Build: spec.Build{Strategy: spec.BuildStatic, StaticDir: dir},
			Workloads: []spec.Workload{{
				Name: "web", Primary: true, Exposed: true,
				Ports: []spec.Port{{Number: 80, Protocol: "http", Source: spec.PortFramework}},
			}},
		},
	}, nil
}

// pageTemplateMarkers are what a build fills in an index.html it uses as a
// template: Create React App's %PUBLIC_URL%, and the EJS tags Vue CLI and
// html-webpack-plugin read.
var pageTemplateMarkers = []string{"%PUBLIC_URL%", "<%=", "<%-"}

// isPageTemplate reports an index.html that is the input to a build rather than
// a page. Create React App keeps one in public/, and read as a committed copy
// of the built site it put a serve-or-build question in front of a project
// that only builds one way (issue #90).
func isPageTemplate(src api.SourceView, name string) bool {
	f, err := src.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head, _ := io.ReadAll(io.LimitReader(f, 64<<10))
	for _, marker := range pageTemplateMarkers {
		if strings.Contains(string(head), marker) {
			return true
		}
	}
	return false
}

// --- Buildpack -------------------------------------------------------------

// BuildpackDetector recognizes a project by its language, for repositories that
// carry no deployment instructions at all.
//
// This is the case R-103 is really about: an app written by someone who never
// thought about deployment. The bid is deliberately modest, because a language
// is not a deployment — but modest is not the same as inquisitive, and what it
// can work out it does not ask. With a planner attached it asks nothing at all.
type BuildpackDetector struct {
	// Planner turns a repository with no deployment instructions into build
	// inputs. Supplied by the builder adapter, because what those inputs are is
	// the builder's vocabulary and core does not learn it (R-251): core asks
	// "how would you build this", stores whatever comes back, and interprets
	// none of it.
	//
	// Nil means detection still recognizes the language and asks its questions;
	// the plan simply is not part of the proposal, and the builder makes one at
	// build time instead. That is the behavior every install had before the
	// plan was reviewable.
	Planner BuildPlanner
}

// BuildPlanner produces the files a buildpack build needs.
//
// Planning reads the repository and decides; it runs nothing from it and builds
// nothing, so it is not a build and R-024's "never on the host" does not bite.
// The files it returns are opaque to core; what they mean for how the app runs
// comes back alongside them, in api.BuildPlan.
type BuildPlanner interface {
	// The plan's Declared is what in the repository dictated it, or nil when
	// convention-matching chose it. The builder knows which it was; detection
	// shows it, and cannot derive it without a second copy of the same reading.
	Plan(ctx context.Context, src api.SourceView) (api.BuildPlan, error)
}

func (BuildpackDetector) Name() string { return "buildpack" }

type languageSignal struct {
	file     string
	language string
	start    string
	port     int
}

var languages = []languageSignal{
	{"package.json", "Node.js", "npm start", 3000},
	{"requirements.txt", "Python", "python app.py", 8000},
	{"pyproject.toml", "Python", "python -m app", 8000},
	{"go.mod", "Go", "the compiled binary", 8080},
	// A Go workspace with no module at its root, built from the root when one
	// of its modules is a program (MonorepoDetector stands down for it).
	{"go.work", "Go", "the compiled binary", 8080},
	{"Gemfile", "Ruby", "bundle exec rails server", 3000},
	{"composer.json", "PHP", "php -S", 8000},
	{"Cargo.toml", "Rust", "the compiled binary", 8080},
	{"pom.xml", "Java", "java -jar", 8080},
}

// jvmBuildSignal is the signal for a Maven or Gradle build in the repository,
// or nil.
func jvmBuildSignal(src api.SourceView) *languageSignal {
	for _, file := range []string{"pom.xml", "build.gradle", "build.gradle.kts"} {
		if info, err := src.Stat(file); err == nil && !info.IsDir {
			return &languageSignal{file, "Java", "java -jar", 8080}
		}
	}
	return nil
}

// plannedOnly stands in for a language signal when no manifest Pando knows was
// found and the planner recognized the repository anyway.
//
// The manifest list is eight files, and the planner reads far more than that: a
// Deno module, a mix.exs, a .csproj, a Gradle build, a bare main.py or
// index.php. Each of those fell through to the generic "Pando could not work
// out how to build and run this app" question although the planner, asked,
// had the answer (issue #55). The port is a placeholder the deploy checks
// against the built image (R-097).
var plannedOnly = languageSignal{language: "recognized", start: "the command that starts it", port: 8080}

func (d BuildpackDetector) Bid(ctx context.Context, src api.SourceView) (Candidate, error) {
	var signal *languageSignal
	for i := range languages {
		if info, err := src.Stat(languages[i].file); err == nil && !info.IsDir {
			signal = &languages[i]
			break
		}
	}
	// A Laravel app has a package.json too, for its assets, and is a PHP app
	// all the same (issue #58).
	if isLaravelApp(src) {
		for i := range languages {
			if languages[i].file == "composer.json" {
				signal = &languages[i]
			}
		}
	}
	// So does a Maven or Gradle app whose build runs its front end's npm
	// build through a plugin: JHipster was built as a Node app and crashed in
	// `npm install` (issue #90). The JVM build is the app's build.
	if signal != nil && signal.file == "package.json" {
		if jvm := jvmBuildSignal(src); jvm != nil {
			signal = jvm
		}
	}
	if signal == nil {
		return d.bidFromPlanOnly(ctx, src)
	}
	if reason := libraryReason(src, signal); reason != nil {
		//nolint:nilerr // The reason travels on the candidate, as a refused
		// compose file's does: returning it would make the auction drop this
		// detector, which is right for one that broke and wrong for one that
		// worked and found a reason.
		return Candidate{
			Strategy:   spec.BuildBuildpack,
			Confidence: 0.6,
			Evidence:   []string{signal.file + " — this looks like a " + signal.language + " library"},
			Blocked:    reason,
		}, nil
	}
	return d.bidFor(ctx, src, signal), nil
}

// libraryReason says why a repository is a library rather than an app, or nil.
//
// A library has nothing to run. Flask, chi and a src-layout Python package were
// read as web apps by their manifests and deployed — as a buildpack guess that
// either failed to build or started nothing (issue #55). Pando deploys apps; a
// library is what apps are made from, and saying so is the honest answer.
func libraryReason(src api.SourceView, signal *languageSignal) error {
	switch signal.file {
	case "go.mod":
		if isGoApp, sawGo := hasGoMainPackage(src); isGoApp || !sawGo {
			return nil
		}
		return errs.New(errs.PlanCapabilityUnsupported,
			"This repository is a Go library: none of its Go files is `package main`, so there is no "+
				"program in it to run. Pando deploys apps, and a library is used by other programs rather "+
				"than run on its own.").
			WithRemedy("If an app in this repository uses the library, set the app's subdirectory to that " +
				"app's directory, or add a Dockerfile that builds it.")
	case "pyproject.toml", "requirements.txt":
		if !isPythonSrcLayoutWithoutEntryPoint(src) {
			return nil
		}
		return errs.New(errs.PlanCapabilityUnsupported,
			"This repository is a Python package meant to be installed and imported: its code is under "+
				"src/, and there is no app.py, main.py, manage.py, Procfile or other file that starts it. "+
				"Pando deploys apps, and a library is used by other programs rather than run on its own.").
			WithRemedy("If this repository is an app, add a Procfile or a Dockerfile saying how it starts.")
	case "package.json":
		return nodeLibraryReason(src)
	}
	return nil
}

// hasGoMainPackage reports whether any Go file that would be built declares
// package main. Directories the go tool itself ignores — testdata, and those
// starting with "_" or "." — are skipped, which is where chi keeps its runnable
// examples. sawGo is false when there were no Go files to judge by, which says
// nothing either way.
func hasGoMainPackage(src api.SourceView) (isMain, sawGo bool) {
	files, err := src.Glob("*.go")
	if err != nil {
		return true, true
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || goIgnores(f) {
			continue
		}
		sawGo = true
		if goPackageOf(src, f) == "main" {
			return true, true
		}
	}
	return false, sawGo
}

func goIgnores(p string) bool {
	for _, segment := range strings.Split(path.Dir(path.Clean(p)), "/") {
		if segment == "testdata" || segment == "vendor" ||
			(segment != "." && (strings.HasPrefix(segment, "_") || strings.HasPrefix(segment, "."))) {
			return true
		}
	}
	return false
}

var goPackageClause = regexp.MustCompile(`^package\s+([A-Za-z_][A-Za-z0-9_]*)`)

func goPackageOf(src api.SourceView, name string) string {
	f, err := src.Open(name)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	// Comments are skipped. A package's doc comment often shows how to use it
	// with an example program, and logrus's doc.go has `package main` in its
	// comment above `package logrus` — read as the package clause, it made a
	// library an app (issue #90).
	inBlock := false
	scanner := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if inBlock {
			end := strings.Index(line, "*/")
			if end < 0 {
				continue
			}
			inBlock = false
			line = strings.TrimSpace(line[end+2:])
		}
		if strings.HasPrefix(line, "/*") {
			if end := strings.Index(line[2:], "*/"); end >= 0 {
				line = strings.TrimSpace(line[2+end+2:])
			} else {
				inBlock = true
				continue
			}
		}
		if m := goPackageClause.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// pythonEntryPoints are the files a Python app is started from.
var pythonEntryPoints = []string{
	"main.py", "app.py", "manage.py", "wsgi.py", "asgi.py", "server.py", "run.py",
	"streamlit_app.py", "Procfile",
}

// isPythonSrcLayoutWithoutEntryPoint reports a package under src/ with nothing
// at the root that starts anything — the layout of a library, not of an app.
func isPythonSrcLayoutWithoutEntryPoint(src api.SourceView) bool {
	packages, err := src.Glob("src/*/__init__.py")
	if err != nil || len(packages) == 0 {
		return false
	}
	for _, name := range pythonEntryPoints {
		if info, err := src.Stat(name); err == nil && !info.IsDir {
			return false
		}
	}
	return !isPythonWebAppPackage(src)
}

// bidFromPlanOnly bids when no manifest matched but the planner can build and
// start the repository. A plan that cannot say how the app starts is not
// enough: asking for a start command on a repository nobody recognized is the
// generic question with a different label.
func (d BuildpackDetector) bidFromPlanOnly(ctx context.Context, src api.SourceView) (Candidate, error) {
	if d.Planner == nil {
		return Candidate{Confidence: 0}, nil
	}
	c := d.bidFor(ctx, src, &plannedOnly)
	if c.Blocked != nil {
		//nolint:nilerr // The reason travels on the candidate, as a library's
		// does (Bid), so the auction surfaces it rather than dropping the bid.
		return c, nil
	}
	if len(c.Draft.Build.GeneratedFiles) == 0 || len(c.Questions) > 0 {
		return Candidate{Confidence: 0}, nil
	}
	return c, nil
}

func (d BuildpackDetector) bidFor(ctx context.Context, src api.SourceView, signal *languageSignal) Candidate {
	evidence := signal.file + " — this looks like a " + signal.language + " project"
	if signal.file == "" {
		evidence = "no language manifest Pando reads, but the build planner recognized this repository"
	}
	c := Candidate{
		Strategy: spec.BuildBuildpack,
		// Modest on purpose. Knowing the language is not knowing how to run the
		// app, and a confident wrong answer is worse than an honest question.
		Confidence: 0.45,
		Evidence:   []string{evidence},
		Draft: Draft{
			Build: spec.Build{Strategy: spec.BuildBuildpack},
			Workloads: []spec.Workload{{
				Name: "web", Primary: true, Exposed: true,
				Ports: []spec.Port{{Number: signal.port, Protocol: "http", Source: spec.PortFramework}},
			}},
		},
	}

	// The plan, so the proposal shows how this app would be built rather than
	// only that it would be. A planner that fails leaves the bid standing: a
	// language Pando recognizes is still the right reading of the repository,
	// and the builder will try again at build time with a real error to show.
	var planned string
	if d.Planner != nil {
		plan, err := d.Planner.Plan(ctx, src)
		// A planner that recognized the repository and found it cannot be
		// built from what it holds — a Hugo theme left behind as a submodule
		// — says why, and that is the answer rather than a question nobody
		// can usefully answer.
		if errs.CodeOf(err) == errs.ValidInvalid {
			c.Blocked = err
			return c
		}
		if err == nil && len(plan.Files) > 0 {
			c.Draft.Build.GeneratedFiles = plan.Files
			c.Draft.Build.Dockerfile = plan.Entry
			c.Evidence = append(c.Evidence, "build plan generated, and editable before it runs")
			declared := plan.Declared

			// What the planner says about how the app runs. A plan that does
			// not say is a Dockerfile, read the way any Dockerfile is.
			exposed := plan.Ports
			planned = plan.Start
			if planned == "" && len(exposed) == 0 {
				exposed, planned = scanDockerfile(strings.NewReader(plan.Files[plan.Entry]))
			}

			// A plan that says which port it serves on is better evidence than
			// the language's usual port: a site built to static files and
			// served by nginx listens on 80, not on Node's 3000.
			if len(exposed) > 0 {
				ports := make([]spec.Port, 0, len(exposed))
				for _, p := range exposed {
					ports = append(ports, spec.Port{Number: p, Protocol: "http", Source: spec.PortExpose})
				}
				c.Draft.Workloads[0].Ports = ports
			}

			// R-094 is a ladder of evidence, and this is where a bid climbs
			// it. Convention-matching is the bottom rung: Railpack reads a
			// repository and infers, which is usually right and is still a
			// guess. Everything above it is the app's author having said
			// something — a Makefile target, a CI workflow, a client config and
			// an embed directive naming one directory — and the difference is
			// whether being wrong is a bug here or a mistake in the repository.
			//
			// The builder decides which reading applied, and says so. Detection
			// does not re-derive it: R-027 keeps the two packages apart, and two
			// implementations of the same reading is how they stop agreeing.
			if declared != nil {
				if declared.Confidence > c.Confidence {
					c.Confidence = declared.Confidence
				}
				if declared.Why != "" {
					c.Evidence = append(c.Evidence, declared.Why)
				}
			}
		}
	}

	// Asked only when nothing worked it out. The plan is the thing that works it
	// out: the planner reads the repository and writes a plan that says the
	// command that starts the app, so on a plain Go module the answer is already
	// in hand before the question would be put.
	//
	// It used to be asked either way, two lines after the plan was attached to
	// the draft — which made the prompt's own first clause ("it does not include
	// a Dockerfile or any other instructions for running it") false at the
	// moment it was shown. R-103 counts questions as the product metric and
	// R-104 says Pando asks only when it genuinely cannot proceed; it could.
	if planned == "" {
		c.Questions = append(c.Questions, Question{
			Key:  "start_command",
			Kind: api.QuestionText,
			Prompt: fmt.Sprintf(
				"This app appears to be a %s project, but it does not include a Dockerfile or any other "+
					"instructions for running it. Pando needs the command that starts it. "+
					"Valid answer: a shell command, such as %q.",
				signal.language, signal.start),
			Why: "Pando runs this command to start the app.",
		})
	} else {
		c.Evidence = append(c.Evidence, "starts with "+planned)
	}

	// The port is not asked at all, and the draft's framework default stands.
	//
	// R-097 says a port is observed rather than asked, and for an app that comes
	// with an image that is what happens. A source build has no image until it
	// is built, so the trial run has nothing to start (Job.trial returns early
	// on an empty image) — and a deferred question whose trial never runs is
	// promoted to one a person has to answer. That put a port question in front
	// of someone R-005 says may not know what a port is, on every repository
	// with no Dockerfile.
	//
	// R-104 settles it: anything with a reasonable default gets the default and
	// is changeable later. The default is already in the draft above, carried as
	// PortFramework so the review screen says where it came from, and editable
	// there before anything is pinned.
	if c.Draft.Workloads[0].Ports[0].Source == spec.PortFramework {
		c.Evidence = append(c.Evidence, fmt.Sprintf(
			"assumed to serve HTTP on %d until the built app is watched starting — change it below if it does not",
			signal.port))
	}

	c.Draft.Slots, c.Draft.Workloads[0].Env = readEnvExample(src)
	if isRailsApp(src) {
		c.Draft = asRailsProduction(c.Draft)
	}
	var required []string
	c.Draft, required = withSourceRequirements(src, c.Draft, ".", "")
	c.Evidence = append(c.Evidence, required...)
	if isMeteorApp(src) {
		c.Draft = withMeteorMongo(c.Draft)
		c.Evidence = append(c.Evidence,
			"a Meteor app: MONGO_URL, the MongoDB it keeps its data in, is needed before it can start")
	}
	return c
}

// isMeteorApp reports a Meteor application: a .meteor/release file.
func isMeteorApp(src api.SourceView) bool {
	info, err := src.Stat(".meteor/release")
	return err == nil && !info.IsDir
}

// meteorMongoEvidence says what MONGO_URL is, for a person who has never run
// Meteor in production (R-105).
const meteorMongoEvidence = "A Meteor app keeps its data in MongoDB, which `meteor run` starts for " +
	"development and a production server is given as MONGO_URL. Pando does not provide MongoDB. Valid " +
	"answer: a MongoDB connection string, such as \"mongodb://user:password@db.example.com:27017/app\", or " +
	"the \"mongodb+srv://\" address a hosted MongoDB gives."

// withMeteorMongo asks for MONGO_URL as a required value (R-132). A Meteor
// server stops at start without one, and Pando has no MongoDB to supply, so
// the person deploying names the one the app uses. A declared value stands.
func withMeteorMongo(d Draft) Draft {
	const key = "MONGO_URL"
	for _, s := range d.Slots {
		if s.Key == key {
			return d
		}
	}
	for _, e := range d.Workloads[0].Env {
		if e.Key == key && (e.Source != spec.EnvFromDetection || e.Value == nil || *e.Value != "") {
			return d
		}
	}
	env := d.Workloads[0].Env[:0:0]
	for _, e := range d.Workloads[0].Env {
		if e.Key != key {
			env = append(env, e)
		}
	}
	ref := key
	d.Workloads[0].Env = append(env, spec.EnvEntry{Key: key, SlotRef: &ref, Source: spec.EnvFromDetection})
	d.Slots = append(d.Slots, spec.Slot{
		Key: key, Type: spec.SlotUnknown, Required: true,
		Evidence: []string{meteorMongoEvidence},
	})
	return d
}

// isLaravelApp reports a Laravel application: an artisan script beside a
// composer.json that requires the framework. A package built on Laravel has no
// artisan.
func isLaravelApp(src api.SourceView) bool {
	if info, err := src.Stat("artisan"); err != nil || info.IsDir {
		return false
	}
	var manifest struct {
		Require map[string]string `json:"require"`
	}
	if !readJSON(src, "composer.json", &manifest) {
		return false
	}
	_, ok := manifest.Require["laravel/framework"]
	return ok
}

// laravelKeyEvidence says what APP_KEY is and how to make one, for a person
// who has never run artisan (R-105).
const laravelKeyEvidence = "Laravel encrypts sessions and cookies with APP_KEY and answers every page " +
	"with an error until it has one. Valid answer: \"base64:\" followed by 32 random bytes in base64, " +
	"which `php artisan key:generate --show` prints, as does `echo \"base64:$(openssl rand -base64 32)\"`. " +
	"Keep it: changing it signs everyone out."

// isRailsApp reports a Rails application rather than a gem that uses Rails.
func isRailsApp(src api.SourceView) bool {
	if info, err := src.Stat("config/application.rb"); err != nil || info.IsDir {
		return false
	}
	info, err := src.Stat("bin/rails")
	return err == nil && !info.IsDir
}

// asRailsProduction runs a Rails app the way a platform does: in production.
//
// Rails and the Puma config it generates listen on the loopback address in
// development — `host = … == "production" ? "::" : "::1"` in the Heroku sample —
// so run as it was, the app was reachable by nothing outside its container
// (issue #55). Production also needs SECRET_KEY_BASE, and that is a secret the
// person deploying supplies, so it is asked for as a required value (R-132)
// rather than invented — by withSourceRequirements, which asks it of a Rails
// app however it is built.
func asRailsProduction(d Draft) Draft {
	production := "production"
	env := d.Workloads[0].Env
	set := map[string]bool{}
	for _, e := range env {
		set[e.Key] = true
	}
	for _, key := range []string{"RAILS_ENV", "RACK_ENV"} {
		if !set[key] {
			env = append(env, spec.EnvEntry{Key: key, Value: &production, Source: spec.EnvFromDetection})
		}
	}
	d.Workloads[0].Env = env
	return d
}

var argDeclaration = regexp.MustCompile(`(?i)^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)\s*$`)

// buildArgNeeds sorts the ARGs a Dockerfile declares without a default into
// the ones it refuses to build without and the ones it only needs to be set.
//
// Required is an ARG the Dockerfile stops itself over when it is empty:
// `${NAME:?…}`, `test -n "$NAME" || …`, or a `-z` test on it that exits. That
// is the file saying, in as many words, that a value is needed; the build
// failed on exactly that and nobody had been asked (issue #55).
//
// Only those. An ARG with no default is usually optional — a version to stamp,
// a platform BuildKit fills in — and asking about every one puts questions in
// front of people that the build does not need answered (R-104). So is an ARG
// the file guards itself: `[ -z $ARCH ] || __ARCH=$ARCH` falls back to
// something else when it is empty, and asking for it made a question nobody
// could answer from the repository.
//
// Empty is the third kind: an ARG a RUN reads under `set -u`. The shell stops
// on a variable that was never set, and an ARG with no default and no value is
// exactly that — `EXTRA_ARGS: parameter not set` — while an empty value is all
// the build wanted. That is a default, not a question (R-104), so it is passed
// as empty and shown on the plan, where it can be changed.
func buildArgNeeds(src api.SourceView, name string) (required, empty []string) {
	f, err := src.Open(name)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(io.LimitReader(f, 256<<10))
	lines := joinContinuations(scanner)
	body := strings.Join(lines, "\n")

	for _, line := range lines {
		m := argDeclaration.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		arg := m[1]
		switch {
		case argAsserted(body, arg):
			required = append(required, arg)
		case argReadUnderNounset(lines, arg):
			empty = append(empty, arg)
		}
	}
	return required, empty
}

// argAsserted reports whether the Dockerfile stops the build when arg is empty.
func argAsserted(body, arg string) bool {
	q := regexp.QuoteMeta(arg)
	ref := `"?\$\{?` + q + `\}?"?`
	if strings.Contains(body, "${"+arg+":?") || strings.Contains(body, "${"+arg+"?") {
		return true
	}
	// `test -n "$X" || exit 1`, `[ -n "$X" ] || (echo …; exit 1)`: anything
	// after the `||` runs only when the value is missing, and it is there to
	// say so.
	if regexp.MustCompile(`-n\s+` + ref + `\s*\]{0,2}\s*\|\|`).MatchString(body) {
		return true
	}
	// `[ -z "$X" ] && exit 1`, `if [ -z "$X" ]; then echo …; exit 1; fi`. A
	// `-z` test followed by `||` is the opposite — a fallback when it is empty —
	// and so is one whose branch assigns something rather than exiting.
	for _, loc := range regexp.MustCompile(`-z\s+`+ref+`\s*\]{0,2}\s*(&&|;\s*then)`).FindAllStringIndex(body, -1) {
		rest := body[loc[1]:]
		if end := strings.IndexAny(rest, "\n"); end >= 0 {
			rest = rest[:end]
		}
		if end := regexp.MustCompile(`\bfi\b`).FindStringIndex(rest); end != nil {
			rest = rest[:end[0]]
		}
		if regexp.MustCompile(`\bexit\s+[1-9]|\bfalse\b`).MatchString(rest) {
			return true
		}
	}
	return false
}

var nounset = regexp.MustCompile(`\bset\s+(-[a-zA-Z]*u[a-zA-Z]*\b|-o\s+nounset\b)`)

// argReadUnderNounset reports a RUN that turns on `set -u` and reads arg with
// no default of its own.
func argReadUnderNounset(lines []string, arg string) bool {
	use := regexp.MustCompile(`\$(\{` + regexp.QuoteMeta(arg) + `\}|` + regexp.QuoteMeta(arg) + `\b)`)
	for _, line := range lines {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "RUN") {
			continue
		}
		if nounset.MatchString(line) && use.MatchString(line) {
			return true
		}
	}
	return false
}

// --- shared helpers --------------------------------------------------------

var exposePattern = regexp.MustCompile(`(?i)^\s*EXPOSE\s+(.+)`)
var cmdPattern = regexp.MustCompile(`(?i)^\s*(CMD|ENTRYPOINT)\s+(.+)`)

// readDockerfile pulls the declared ports and start command out of a Dockerfile.
//
// Deliberately shallow: this reads what the file states, and does not try to
// interpret the build. R-021 — Pando fills what is declared and does not invent.
func readDockerfile(src api.SourceView, name string) ([]int, string) {
	f, err := src.Open(name)
	if err != nil {
		return nil, ""
	}
	defer func() { _ = f.Close() }()
	return scanDockerfile(f)
}

// scanDockerfile is readDockerfile's parser, over content rather than a file.
//
// Separate because a generated build plan arrives as a string — the planner
// hands back the Dockerfile it wrote, and there is no second copy on disk to
// open.
//
// The last start instruction wins, not the first. A multi-stage build declares
// one per stage and only the final stage's survives into the image, so taking
// the first reports a builder stage's command as the app's. nixpacks wrote
// exactly that shape: `ENTRYPOINT ["/bin/bash", "-l", "-c"]` in the build stage
// and the real `CMD` at the end.
func scanDockerfile(r io.Reader) ([]int, string) {
	var ports []int
	var command string

	scanner := bufio.NewScanner(io.LimitReader(r, 256<<10))
	for _, line := range joinContinuations(scanner) {
		if m := exposePattern.FindStringSubmatch(line); m != nil {
			for _, field := range strings.Fields(m[1]) {
				if n, err := strconv.Atoi(strings.SplitN(field, "/", 2)[0]); err == nil {
					ports = append(ports, n)
				}
			}
		}
		if m := cmdPattern.FindStringSubmatch(line); m != nil {
			command = strings.TrimSpace(m[2])
		}
	}
	return ports, command
}

// joinContinuations folds backslash-continued lines into one.
//
// Without this a Dockerfile written the ordinary way lies to the reader:
//
//	HEALTHCHECK --interval=30s --timeout=3s --retries=3 \\
//	  CMD wget --spider http://localhost:3001/ || exit 1
//
//	CMD ["node", "dist/server/index.js"]
//
// The second physical line begins with CMD, so a line-at-a-time scan reports
// the health probe as the app's start command and never reaches the real one.
// This is shown to a user as the evidence for what Pando decided, and evidence
// that is confidently wrong is worse than none at all.
func joinContinuations(scanner *bufio.Scanner) []string {
	var lines []string
	var pending strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if trimmed := strings.TrimRight(line, " 	"); strings.HasSuffix(trimmed, "\\") {
			pending.WriteString(strings.TrimSuffix(trimmed, "\\"))
			pending.WriteString(" ")
			continue
		}
		if pending.Len() > 0 {
			pending.WriteString(line)
			lines = append(lines, pending.String())
			pending.Reset()
			continue
		}
		lines = append(lines, line)
	}
	if pending.Len() > 0 {
		lines = append(lines, pending.String())
	}
	return lines
}

// backingService reports whether a compose service is one Pando supplies.
//
// A service whose image is postgres or redis is a dependency the app declares,
// which is exactly what a slot is for (R-131). Anything unrecognized stays a
// workload — Pando does not guess at what an unfamiliar image is.
func backingService(service, image string) (spec.SlotType, bool) {
	// A database image with extensions built in is not the stock one Pando
	// provisions: ziit's migration stopped at `extension "timescaledb" is not
	// available`, and AdventureLog needs PostGIS (issue #90). It is run as the
	// compose file wrote it.
	for _, extended := range []string{"timescale", "postgis", "pgvector", "pgvecto", "paradedb", "citus", "supabase", "apache/age", "pgrouting"} {
		if strings.Contains(strings.ToLower(image), extended) {
			return "", false
		}
	}

	known := map[string]spec.SlotType{
		"postgres": spec.SlotPostgres,
		"mysql":    spec.SlotMySQL, "mariadb": spec.SlotMySQL,
		"redis": spec.SlotRedis, "valkey": spec.SlotRedis,
	}

	// Sorted, because two of these can match one service — a `postgres`
	// service running `mariadb` is nonsense, but map iteration order deciding
	// which nonsense wins is worse than nonsense.
	prefixes := make([]string, 0, len(known))
	for prefix := range known {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)

	for _, prefix := range prefixes {
		if strings.Contains(strings.ToLower(image), prefix) ||
			strings.Contains(strings.ToLower(service), prefix) {
			return known[prefix], true
		}
	}
	return "", false
}

var envAssignment = regexp.MustCompile(`^\s*([A-Z][A-Z0-9_]*)\s*=\s*(.*)$`)

// readEnvExample reads .env.example for declared configuration, and splits it
// into the two things it actually holds.
//
// R-130: a variable there is a hole with a type. `REDIS_URL` says the app needs
// a Redis — that is a dependency, and a dependency is a slot, resolved by
// running one, connecting to one, or pasting a connection string (R-131).
// `VAPID_PRIVATE_KEY` says the app needs a value. There is nothing to connect it
// to, and offering to is an interface asking a question with no true answer.
//
// Everything in the file used to become a slot, so an app with a `.env.example`
// arrived declaring eleven "dependencies" of type unknown, each offering to
// connect to something that already exists. Untyped keys are variables now, and
// land on the app's variables with nothing in them — which is what a hole is.
//
// O-4 is the remaining open question and it is narrower than it was: of the
// keys that are dependencies, which are required. The [P] fallback stands —
// default to optional and let the trial run promote what actually breaks
// (design 01 §2.5).
func readEnvExample(src api.SourceView) ([]spec.Slot, []spec.EnvEntry) {
	var slots []spec.Slot
	var env []spec.EnvEntry

	for _, name := range []string{".env.example", ".env.sample", ".env.template"} {
		f, err := src.Open(name)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(io.LimitReader(f, 64<<10))
		for scanner.Scan() {
			m := envAssignment.FindStringSubmatch(scanner.Text())
			if m == nil {
				continue
			}
			key, value := m[1], strings.TrimSpace(m[2])

			slotType := slotTypeFor(key, value)
			if slotType == spec.SlotUnknown {
				// A variable, declared with no value. The sample is not
				// carried: `POSTGRES_PASSWORD=changeme` filled in is worse
				// than empty, because it looks answered.
				empty := ""
				env = append(env, spec.EnvEntry{
					Key: key, Value: &empty, Source: spec.EnvFromDetection,
				})
				continue
			}

			slots = append(slots, spec.Slot{
				Key:  key,
				Type: slotType,
				// Required when the key names a service and the file gives no
				// value for it. A key with a sample value starts optional and
				// is promoted by the trial run if its absence breaks the app.
				Required: value == "",
				Evidence: []string{"declared in " + name},
			})
		}
		_ = f.Close()
		break
	}
	return slots, env
}

// slotTypeFor types a declared variable, or reports that it is not a dependency
// at all.
//
// R-130 names the three sources of a type, and this is two of them — the URL
// scheme in the sample value, and the variable name. (The third, a compose image
// name, is slotsFromComposeServices.) The value goes first because it is the
// direct evidence: `STORE=redis://localhost:6379` is a Redis whatever it is
// called.
//
// The name only types a variable that names a *connection*: `DATABASE_URL`,
// `REDIS_URI`, `PG_DSN`. `POSTGRES_PASSWORD` contains the word postgres and is
// not a database — it is a string, and treating it as one half of a connection
// produced a dialog offering to connect the password to a database. Matching on
// whole words rather than substrings is the other half of the same fix:
// `UPGRADE_URL` contains "PG".
func slotTypeFor(key, value string) spec.SlotType {
	if t := schemeType(value); t != spec.SlotUnknown {
		return t
	}
	if !namesAConnection(key) {
		return spec.SlotUnknown
	}
	return wordType(key)
}

// connectionSuffixes are the words that make a variable a place rather than a
// value. Deliberately short: a suffix that is merely often a connection, like
// HOST, is one an app pairs with a separate port, user and password, and no
// slot resolution can fill four variables from one answer.
var connectionSuffixes = []string{"URL", "URI", "DSN", "CONNECTION_STRING", "ENDPOINT"}

func namesAConnection(key string) bool {
	upper := strings.ToUpper(key)
	for _, suffix := range connectionSuffixes {
		if upper == suffix || strings.HasSuffix(upper, "_"+suffix) {
			return true
		}
	}
	// A cache named by itself — PAPERLESS_REDIS — is its address: a Redis has
	// nothing else a variable would be named after it for. A database is not
	// read this way, because MYSQL_DATABASE is the name of one, not the place.
	for _, service := range []string{"REDIS", "VALKEY"} {
		if strings.HasSuffix(upper, "_"+service) {
			return true
		}
	}
	return false
}

// wordType reads the service out of a variable's name, word by word.
func wordType(key string) spec.SlotType {
	for _, word := range strings.Split(strings.ToUpper(key), "_") {
		switch word {
		case "POSTGRES", "POSTGRESQL", "PG", "DATABASE", "DB":
			return spec.SlotPostgres
		case "MYSQL", "MARIADB":
			return spec.SlotMySQL
		case "REDIS", "VALKEY":
			return spec.SlotRedis
		case "S3", "MINIO":
			return spec.SlotS3
		case "SMTP", "MAIL", "MAILER":
			return spec.SlotSMTP
		}
	}
	return spec.SlotUnknown
}

// schemeType reads the service out of a sample value's URL scheme.
func schemeType(value string) spec.SlotType {
	scheme, _, found := strings.Cut(value, "://")
	if !found {
		return spec.SlotUnknown
	}
	switch strings.ToLower(scheme) {
	case "postgres", "postgresql":
		return spec.SlotPostgres
	case "mysql", "mariadb":
		return spec.SlotMySQL
	case "redis", "rediss":
		return spec.SlotRedis
	case "s3":
		return spec.SlotS3
	case "smtp", "smtps":
		return spec.SlotSMTP
	default:
		return spec.SlotUnknown
	}
}

func truncate(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(append([]string(nil), items[:n]...), "…")
}

func joinInts(ns []int) string {
	parts := make([]string, 0, len(ns))
	for _, n := range ns {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}

func displayDir(dir string) string {
	if dir == "." {
		return "the repository root"
	}
	return dir + "/"
}

func orUnknownImage(image string) string {
	if image == "" {
		return "an image Pando could not read"
	}
	return image
}

// --- Monorepo --------------------------------------------------------------

// MonorepoDetector recognizes a repository that holds many projects rather than
// one app.
//
// This bids StrategyUnknown, which is not a contradiction: being confident that
// a repository is not a single deployable app is a real detection result, and a
// more useful one than a low-confidence guess at which of its twenty packages
// was meant. R-021 is the rule — Pando fills what is declared and does not
// invent topology — and a workspace manifest is the repository declaring, in as
// many words, that it contains several things.
//
// It is a veto rather than a bid, so it stands down whenever the repository
// does say what to deploy: a Dockerfile or compose file at the root answers the
// question this detector would otherwise ask.
type MonorepoDetector struct{}

func (MonorepoDetector) Name() string { return "monorepo" }

// workspaceMarkers are files that declare a repository holds multiple packages.
var workspaceMarkers = []string{
	"pnpm-workspace.yaml", "pnpm-workspace.yml",
	"lerna.json", "nx.json", "rush.json", "turbo.json", "go.work",
}

// packageDirs are where multi-project repositories conventionally keep them.
var packageDirs = []string{"apps", "packages", "services", "cmd", "projects"}

func (d MonorepoDetector) Bid(_ context.Context, src api.SourceView) (Candidate, error) {
	// If the root says how to build the repository, there is nothing to veto.
	for _, name := range dockerfileNames {
		if info, err := src.Stat(name); err == nil && !info.IsDir {
			return Candidate{Confidence: 0}, nil
		}
	}
	for _, name := range composeNames {
		if info, err := src.Stat(name); err == nil && !info.IsDir {
			return Candidate{Confidence: 0}, nil
		}
	}

	// What the manifests declare, not that they exist (workspace.go).
	markers := declaredWorkspaceMarkers(src)
	if len(markers) == 0 {
		// No workspace, and perhaps no app at the root either: the projects
		// one level down are what there is to deploy.
		return nestedProjectsBid(src), nil
	}
	if workspaceResolved(src, markers) {
		return Candidate{Confidence: 0}, nil
	}

	// Name what was found. R-105 requires the question stand on its own, and
	// "which one do you want?" without a list is exactly the fragment it bans.
	found := workspaceMembers(src)

	evidence := []string{"workspace declared by " + strings.Join(markers, ", ")}
	if len(found) > 0 {
		evidence = append(evidence,
			fmt.Sprintf("%d projects under %s", len(found), strings.Join(packageDirsPresent(src), ", ")))
	}

	return Candidate{
		Strategy: StrategyUnknown,
		// High, because the evidence is a declaration rather than an inference.
		// The repository is not being read between the lines; it says so.
		Confidence: 0.8,
		Evidence:   evidence,
		Questions:  []Question{monorepoQuestion(markers, found)},
	}, nil
}

func monorepoQuestion(markers, found []string) Question {
	q := Question{
		Key:  "deployable_project",
		Kind: api.QuestionText,
		Why:  "Pando deploys one app at a time, and this repository contains several.",
	}

	if len(found) > 0 {
		q.Kind = api.QuestionChoice
		q.Options = found
		q.Prompt = fmt.Sprintf(
			"This repository holds several separate projects rather than one app — it declares a "+
				"workspace (%s) and contains %d projects: %s. Pando deploys one app at a time and "+
				"cannot tell which of these you mean. Valid answer: one of those paths.",
			strings.Join(markers, ", "), len(found), strings.Join(truncate(found, 12), ", "))
		return q
	}

	q.Prompt = fmt.Sprintf(
		"This repository declares a workspace (%s), which means it is meant to hold several "+
			"separate projects rather than one app. Pando deploys one app at a time and could not "+
			"find a single thing here to deploy. Valid answer: the path within the repository of "+
			"the project you want deployed, for example \"apps/web\".",
		strings.Join(markers, ", "))
	return q
}

func packageDirsPresent(src api.SourceView) []string {
	var present []string
	for _, dir := range packageDirs {
		if info, err := src.Stat(dir); err == nil && info.IsDir {
			present = append(present, dir+"/")
		}
	}
	return present
}

// hasBuildScript reports whether package.json declares a build command.
func hasBuildScript(src api.SourceView) bool {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if !readJSON(src, "package.json", &pkg) {
		return false
	}
	return strings.TrimSpace(pkg.Scripts["build"]) != ""
}

var jekyllTheme = regexp.MustCompile(`(?m)^\s*(remote_)?theme\s*:`)

// isJekyllSource reports a _config.yml at the root that names a theme, or sits
// beside a directory only a Jekyll site has. The builder reads the same
// repository more closely to plan the build; this only has to know the root is
// not a finished site.
func isJekyllSource(src api.SourceView) bool {
	var config []byte
	for _, name := range []string{"_config.yml", "_config.yaml"} {
		if f, err := src.Open(name); err == nil {
			config, _ = io.ReadAll(io.LimitReader(f, 64<<10))
			_ = f.Close()
			break
		}
	}
	if config == nil {
		return false
	}
	if jekyllTheme.Match(config) {
		return true
	}
	for _, dir := range []string{"_layouts", "_includes", "_posts", "_sass", "_data"} {
		if info, err := src.Stat(dir); err == nil && info.IsDir {
			return true
		}
	}
	return false
}

// hasCargoWorkspace reports whether Cargo.toml declares a [workspace] table.
func hasCargoWorkspace(src api.SourceView) bool {
	f, err := src.Open("Cargo.toml")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(io.LimitReader(f, 256<<10))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "[workspace]" || strings.HasPrefix(line, "[workspace.") {
			return true
		}
	}
	return false
}

func readJSON(src api.SourceView, name string, into any) bool {
	f, err := src.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, into) == nil
}
