package buildkit

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// How a Python web app is started, when the repository does not say.
//
// Railpack starts `python main.py` (or app.py, server.py…) unless the module
// is main.py and the framework FastAPI or Flask. A module that only creates
// its app and leaves serving it to a server — the usual shape for FastAPI,
// BlackSheep, Litestar, Starlette and Flask — imported the app and exited 0,
// and the deploy reported success with nothing listening (issues #55, #90). A
// Flask app that runs itself with app.run() serves on 127.0.0.1, which nothing
// outside its container reaches. A Streamlit script runs only under `streamlit
// run`, and says so instead of serving.
//
// The server named has to be one the app installs: naming one it does not
// would fail the same way, later.

var (
	// appConstructed is a module-level `name = Framework(`.
	appConstructed = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(?:[A-Za-z_][A-Za-z0-9_]*\.)?(FastAPI|Flask|Application|Litestar|Starlette|Quart|Bottle|Falcon|App)\(`)

	// appImported is a module-level `from x import app`, the app made
	// elsewhere and named here for a server to find.
	appImported = regexp.MustCompile(`(?m)^from\s+[A-Za-z_.][A-Za-z0-9_.]*\s+import\s+(?:[^\n#]*,\s*)?(app|application)\b`)

	runsItself = regexp.MustCompile(`__name__\s*==\s*['"]__main__['"]`)

	// servesItself is a call that runs a server from the module itself.
	servesItself = regexp.MustCompile(`uvicorn\.run\(|hypercorn\.|\.run\(|run_app\(|serve\(|web\.run_app|waitress`)

	// allInterfaces is a server told to listen beyond the loopback address.
	allInterfaces = regexp.MustCompile(`["'](?:0\.0\.0\.0|::)["']`)

	importsStreamlit = regexp.MustCompile(`(?m)^\s*(?:import streamlit|from streamlit)`)
)

// asgiFrameworks are the frameworks whose app an ASGI server serves, as the
// package name a project depends on.
var asgiFrameworks = []string{"fastapi", "starlette", "blacksheep", "litestar", "quart"}

// wsgiFrameworks are the frameworks whose app a WSGI server serves.
var wsgiFrameworks = []string{"flask", "bottle", "falcon"}

// pythonDeps is a project's dependency declarations, lowercased, for matching
// package names.
func pythonDeps(contextDir string) string {
	return strings.ToLower(readFile(contextDir, "requirements.txt") + "\n" +
		readFile(contextDir, "pyproject.toml") + "\n" + readFile(contextDir, "Pipfile"))
}

// hasPythonDep reports a dependency on pkg in deps, which pythonDeps read.
func hasPythonDep(deps, pkg string) bool {
	return regexp.MustCompile(`(^|[^a-z0-9_\-])` + regexp.QuoteMeta(pkg) + `($|[^a-z0-9_\-])`).MatchString(deps)
}

// pythonTOMLLockfiles are the lockfiles that name every package a project
// installs, its dependencies' dependencies included, one `[[package]]` table
// each.
var pythonTOMLLockfiles = []string{"poetry.lock", "uv.lock", "pdm.lock"}

// lockedPackageName is a `[[package]]` table's name.
var lockedPackageName = regexp.MustCompile(`(?m)^name\s*=\s*"([^"]+)"`)

// pythonLocked is the names of the packages the project's lockfiles install,
// one per line, lowercased: the server a framework brings is installed without
// the project naming it. FastAPI 0.111 brings uvicorn, and
// fly-apps/hello-fastapi, which depends on FastAPI alone, was started as
// `python app.py`, which exited without serving (issue #90). Only package
// names count: an older FastAPI lists `uvicorn[standard]` among its extras,
// which nothing installs.
func pythonLocked(contextDir string) string {
	var b strings.Builder
	for _, name := range pythonTOMLLockfiles {
		for _, m := range lockedPackageName.FindAllStringSubmatch(readFile(contextDir, name), -1) {
			b.WriteString(strings.ToLower(m[1]) + "\n")
		}
	}
	var pipfile struct {
		Default map[string]json.RawMessage `json:"default"`
	}
	if json.Unmarshal([]byte(readFile(contextDir, "Pipfile.lock")), &pipfile) == nil {
		for name := range pipfile.Default {
			b.WriteString(strings.ToLower(name) + "\n")
		}
	}
	return b.String()
}

// asgiServer is the ASGI server command the project installs, or "".
// FastAPI's [all] and [standard] extras bring uvicorn.
func asgiServer(deps string) string {
	switch {
	case hasPythonDep(deps, "uvicorn"), strings.Contains(deps, "fastapi[all]"), strings.Contains(deps, "fastapi[standard]"):
		return "uvicorn %s:%s --host 0.0.0.0 --port ${PORT:-8000}"
	case hasPythonDep(deps, "hypercorn"):
		return "hypercorn %s:%s --bind 0.0.0.0:${PORT:-8000}"
	}
	return ""
}

// pythonAppStart is the command a Python web app starts with, when its module
// does not serve it itself or serves it only on the loopback address, or ""
// to leave the start to Railpack.
func pythonAppStart(contextDir string) string {
	deps := pythonDeps(contextDir)
	if deps == "\n\n" {
		return ""
	}
	if start := reflexStart(contextDir, deps); start != "" {
		return start
	}
	if script := streamlitScript(contextDir, deps); script != "" {
		return "streamlit run " + script + " --server.address 0.0.0.0 --server.port ${PORT:-8501}"
	}
	for _, module := range []string{"main", "app", "server", "asgi", "wsgi"} {
		if start := pythonModuleStart(contextDir, module, deps); start != "" {
			return start
		}
	}
	return ""
}

// pythonModuleStart is the command that starts the app in module.py, or ""
// when that module does not settle it and the next is looked at.
func pythonModuleStart(contextDir, module, deps string) string {
	body := readFile(contextDir, module+".py")
	if body == "" {
		return ""
	}
	name, kind := pythonAppVar(body, deps)
	installed := deps + "\n" + pythonLocked(contextDir)

	// A module that starts its own server is run as it is, unless the
	// server it starts listens only on the loopback address and there is
	// an app a server can be pointed at instead. Said rather than left
	// to Railpack, which serves any Flask app it finds with gunicorn as
	// main:app, and an app.py has no module main ("ModuleNotFoundError:
	// No module named 'main'", issue #172).
	if runsItself.MatchString(body) && servesItself.MatchString(body) {
		if allInterfaces.MatchString(body) || name == "" {
			return "python " + module + ".py"
		}
		if start := serveApp(module, name, kind, installed); start != "" {
			return start
		}
		return "python " + module + ".py"
	}
	if runsItself.MatchString(body) || name == "" {
		return ""
	}
	return serveApp(module, name, kind, installed)
}

// serveApp is the command that serves module:name with a server the project
// installs, or "".
func serveApp(module, name, kind string, deps string) string {
	switch kind {
	case "asgi":
		if server := asgiServer(deps); server != "" {
			return fmt.Sprintf(server, module, name)
		}
	case "wsgi":
		if hasPythonDep(deps, "gunicorn") {
			return fmt.Sprintf("gunicorn %s:%s --bind 0.0.0.0:${PORT:-8000}", module, name)
		}
		// Flask's own server, on every interface: what the app would have
		// run, reachable. Flask installs its CLI.
		if hasPythonDep(deps, "flask") {
			return fmt.Sprintf("flask --app %s:%s run --host 0.0.0.0 --port ${PORT:-5000}", module, name)
		}
	}
	return ""
}

// pythonAppVar is the module-level name of the app in body, and whether an
// ASGI or a WSGI server serves it; "" when the module has none.
func pythonAppVar(body, deps string) (name, kind string) {
	if m := appConstructed.FindStringSubmatch(body); m != nil {
		switch m[2] {
		case "Flask", "Bottle", "Falcon":
			return m[1], "wsgi"
		case "Application", "App":
			// Generic names: BlackSheep's Application, or aiohttp's, which
			// runs itself. Which one is in the dependencies.
			if k := frameworkKind(deps); k != "" {
				return m[1], k
			}
			return "", ""
		default:
			return m[1], "asgi"
		}
	}
	if m := appImported.FindStringSubmatch(body); m != nil {
		if k := frameworkKind(deps); k != "" {
			return m[1], k
		}
	}
	return "", ""
}

// frameworkKind is the server kind of the web framework the project depends
// on, or "".
func frameworkKind(deps string) string {
	for _, f := range asgiFrameworks {
		if hasPythonDep(deps, f) {
			return "asgi"
		}
	}
	for _, f := range wsgiFrameworks {
		if hasPythonDep(deps, f) {
			return "wsgi"
		}
	}
	return ""
}

// streamlitScript is the script a Streamlit app runs, when the project
// depends on Streamlit and one of the usual entry points imports it.
func streamlitScript(contextDir, deps string) string {
	if !hasPythonDep(deps, "streamlit") {
		return ""
	}
	for _, name := range []string{"streamlit_app.py", "app.py", "main.py", "Home.py", "home.py"} {
		if importsStreamlit.MatchString(readFile(contextDir, name)) {
			return name
		}
	}
	return ""
}

// reflexStart is how a Reflex app starts: `reflex run`, in production mode,
// from the directory holding rxconfig.py.
func reflexStart(contextDir, deps string) string {
	if !hasPythonDep(deps, "reflex") || !fileExists(contextDir, "rxconfig.py") {
		return ""
	}
	return "reflex run --env prod"
}

// pythonAppPort is the port a Python app's framework serves on by default,
// when it is not the language's usual 8000: Streamlit's 8501, Reflex's
// frontend on 3000. Zero otherwise.
func pythonAppPort(contextDir string) int {
	deps := pythonDeps(contextDir)
	switch {
	case reflexStart(contextDir, deps) != "":
		return 3000
	case streamlitScript(contextDir, deps) != "":
		return 8501
	}
	return 0
}
