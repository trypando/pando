package buildkit

import (
	"encoding/json"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Start commands a repository declares outside a Procfile, and the ones its
// framework implies.

// safeStart reports whether a start command read out of a repository can be
// handed to Railpack as the image's command.
//
// Looser than safeCommand, which guards build commands: a build command is
// wrapped in `sh -c '…'` inside the plan, and a quote would break out of it.
// A start command is stored whole as the image's command and run by a shell
// when the container starts, so `$PORT` is expanded where it should be — at
// start, with the port Pando gives the app — and a quoted argument stays
// quoted. A Procfile's `gunicorn -b :$PORT app:app` was refused for its `$`,
// and the app started on Flask's development server on 127.0.0.1 instead
// (issue #90). Command substitution, backticks and line breaks are still
// refused: nothing a platform's start command needs, and nothing to run
// unread.
func safeStart(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 512 {
		return false
	}
	return !strings.ContainsAny(s, "`\n\r") && !strings.Contains(s, "$(")
}

// --- platform manifests -----------------------------------------------------

// readPlatformBuild reads the start and build commands a hosting platform's
// manifest declares: railway.json or railway.toml, and render.yaml.
//
// R-094 ranks them with the Procfile: the author saying how the app runs, for
// a platform that, like Pando, builds from source. railway.json's
// `hypercorn main:app --bind "[::]:$PORT"` and render.yaml's `uvicorn main:app
// --host 0.0.0.0 --port $PORT` were ignored, and the apps were started as
// `python main.py`, which serves nothing (issue #90).
func readPlatformBuild(contextDir string) declaredBuild {
	for _, read := range []func(string) (string, string, string){readRailwayJSON, readRailwayTOML, readRenderYAML} {
		source, build, start := read(contextDir)
		if source == "" || (build == "" && start == "") {
			continue
		}
		d := declaredBuild{Rank: rankProcfile, Source: source}
		if build != "" && safeCommand(build) {
			d.Build = build
		}
		if start != "" && safeStart(start) {
			d.Start = start
		}
		if d.Build == "" && d.Start == "" {
			continue
		}
		d.Why = source + " declares how this app is started, and the plan uses it rather than guessing"
		return d
	}
	return declaredBuild{}
}

func readRailwayJSON(contextDir string) (source, build, start string) {
	var manifest struct {
		Build struct {
			BuildCommand string `json:"buildCommand"`
		} `json:"build"`
		Deploy struct {
			StartCommand string `json:"startCommand"`
		} `json:"deploy"`
	}
	for _, name := range []string{"railway.json", "railway.jsonc"} {
		body := readFile(contextDir, name)
		if body == "" || json.Unmarshal([]byte(body), &manifest) != nil {
			continue
		}
		return name, strings.TrimSpace(manifest.Build.BuildCommand), strings.TrimSpace(manifest.Deploy.StartCommand)
	}
	return "", "", ""
}

var (
	tomlStart = regexp.MustCompile(`(?m)^\s*startCommand\s*=\s*"((?:[^"\\]|\\.)*)"`)
	tomlBuild = regexp.MustCompile(`(?m)^\s*buildCommand\s*=\s*"((?:[^"\\]|\\.)*)"`)
)

// readRailwayTOML reads railway.toml's two commands, which are one line each.
// Enough TOML for that and no more: a command spread over a multi-line string
// is not recognized, and the repository is planned by convention.
func readRailwayTOML(contextDir string) (source, build, start string) {
	body := readFile(contextDir, "railway.toml")
	if body == "" {
		return "", "", ""
	}
	unquote := func(s string) string { return strings.ReplaceAll(s, `\"`, `"`) }
	if m := tomlStart.FindStringSubmatch(body); m != nil {
		start = unquote(m[1])
	}
	if m := tomlBuild.FindStringSubmatch(body); m != nil {
		build = unquote(m[1])
	}
	return "railway.toml", build, start
}

// readRenderYAML reads the web service of a render.yaml blueprint. Only when
// there is exactly one: a blueprint of several web services is several apps,
// and which one this is is not something to guess (R-021).
func readRenderYAML(contextDir string) (source, build, start string) {
	for _, name := range []string{"render.yaml", "render.yml"} {
		body := readFile(contextDir, name)
		if body == "" {
			continue
		}
		var blueprint struct {
			Services []struct {
				Type         string `yaml:"type"`
				RootDir      string `yaml:"rootDir"`
				BuildCommand string `yaml:"buildCommand"`
				StartCommand string `yaml:"startCommand"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal([]byte(body), &blueprint) != nil {
			continue
		}
		var web []int
		for i, s := range blueprint.Services {
			if s.Type == "web" && (s.RootDir == "" || s.RootDir == ".") {
				web = append(web, i)
			}
		}
		if len(web) != 1 {
			continue
		}
		s := blueprint.Services[web[0]]
		return name, renderBuild(s.BuildCommand), strings.TrimSpace(s.StartCommand)
	}
	return "", "", ""
}

// renderBuild is a render.yaml build command, less the dependency install
// Railpack already runs: `pip install -r requirements.txt` on its own is not
// a build step, and running it again outside Railpack's virtualenv would
// install into the wrong place.
func renderBuild(command string) string {
	command = strings.TrimSpace(command)
	var kept []string
	for _, part := range strings.Split(command, "&&") {
		p := strings.TrimSpace(part)
		if p == "" || installsDependencies(p) {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, " && ")
}

var dependencyInstall = regexp.MustCompile(`^(pip3? install|python -m pip install|poetry install|npm (ci|install)|yarn( install)?|pnpm (i|install)|bundle( install)?|composer install|uv sync|pipenv install)\b`)

func installsDependencies(command string) bool {
	return dependencyInstall.MatchString(command)
}

// --- Deno -------------------------------------------------------------------

// readDenoTasks reads deno.json's tasks, the runner a Deno project names its
// commands with. Railpack runs `deno run main.ts`, and Fresh 2's main.ts only
// exports the app for `deno serve`, so the process exited 0 at once (issue
// #90); its `start` task is what serves it, after its `build` task.
func readDenoTasks(contextDir string) declaredBuild {
	for _, name := range []string{"deno.json", "deno.jsonc"} {
		body := readFile(contextDir, name)
		if body == "" {
			continue
		}
		var manifest struct {
			Tasks map[string]json.RawMessage `json:"tasks"`
		}
		if json.Unmarshal([]byte(stripJSONComments(body)), &manifest) != nil {
			continue
		}
		d := declaredBuild{Rank: rankRunner, Source: name}
		if _, ok := manifest.Tasks["start"]; ok {
			d.Start = "deno task start"
		}
		if _, ok := manifest.Tasks["build"]; ok {
			// Dependencies first: a task that runs an npm package's binary,
			// Vite's, needs node_modules, which `deno install` makes.
			d.Build = "deno install && deno task build"
		}
		if d.Start == "" && d.Build == "" {
			return declaredBuild{}
		}
		d.Why = name + " declares the tasks that build and start this app, and the plan runs them"
		return d
	}
	return declaredBuild{}
}

var jsonLineComment = regexp.MustCompile(`(?m)^\s*//.*$`)

// stripJSONComments drops whole-line // comments, the JSONC Deno's own
// templates use. A comment after a value on the same line is left, and the
// file then fails to parse and is ignored.
func stripJSONComments(body string) string {
	return jsonLineComment.ReplaceAllString(body, "")
}

// --- Ruby -------------------------------------------------------------------

var (
	lockedRack3  = regexp.MustCompile(`(?m)^    rack \(3\.`)
	lockedRackup = regexp.MustCompile(`(?m)^    rackup \(`)
	lockedPuma   = regexp.MustCompile(`(?m)^    puma \(`)
)

// rackStart is how a bare Rack app on Rack 3 starts, when it has Puma and not
// the rackup gem. Railpack starts a config.ru with `bundle exec rackup`, and
// Rack 3 moved rackup into a gem of its own: "bundler: command not found:
// rackup" (issue #90). Puma reads config.ru itself.
func rackStart(contextDir string) string {
	if !fileExists(contextDir, "config.ru") || fileExists(contextDir, "bin/rails") {
		return ""
	}
	lock := readFile(contextDir, "Gemfile.lock")
	if !lockedRack3.MatchString(lock) || lockedRackup.MatchString(lock) || !lockedPuma.MatchString(lock) {
		return ""
	}
	return "bundle exec puma -b tcp://0.0.0.0:${PORT:-9292}"
}
