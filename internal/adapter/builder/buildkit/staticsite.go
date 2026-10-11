package buildkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// A site that builds to static files.
//
// Astro, Vite, Angular, Create React App and Gatsby build a directory of HTML
// and assets. What runs afterwards is either nothing, or the framework's own
// development server, or a small static file server — none of which is how a
// built site should be served. Through nixpacks these failed three ways (issue
// #55): "No start command could be found" for a site with nothing to start; a
// Vite 8 build that could not find its native binding under nixpacks' Node; and
// an Angular build refused by the Node nixpacks pins. What such a site needs is
// what a committed static site gets: build it, then serve the output.
//
// So a site recognized here is planned by Pando rather than by nixpacks, with
// the official Node image to build and the same nginx server and config a
// committed site gets.

// staticSiteNodeDefault is the Node major the site is built with when it names
// none. [P] 22, an active LTS: current Vite, Next.js, Angular and NestJS
// refuse 18 with EBADENGINE (issue #55).
const staticSiteNodeDefault = "22"

// staticSiteFrameworks build to a directory. The directory is each framework's
// default output; Angular's is read from angular.json.
var staticSiteFrameworks = []struct {
	dependency string
	outDir     string
}{
	{"astro", "dist"},
	{"@angular/core", ""},
	{"gatsby", "public"},
	{"react-scripts", "build"},
	{"vite", "dist"},
}

// devServers are start scripts that run a framework's development server, which
// is a way to work on a site rather than to serve one.
var devServers = regexp.MustCompile(`^(npx\s+)?(vite(\s+(dev|serve))?|astro\s+dev|gatsby\s+develop|ng\s+serve|react-scripts\s+start)(\s|$)`)

// staticServers are start scripts that serve a directory as it is. The
// directory is the site; the server is replaced with nginx. sirv binds to
// localhost unless told otherwise, and a site started that way was unreachable.
var staticServers = regexp.MustCompile(`^(npx\s+)?(sirv|serve|http-server)\s+([^\s-][^\s]*)`)

// staticSite is what reading package.json said about a static site build.
type staticSite struct {
	Framework string
	OutDir    string
	Install   string
	Node      string
}

// readStaticSiteBuild reports whether the repository is a site that builds to
// static files: a build script, a framework that outputs a directory, and no
// start script other than a development server or a static file server. Any
// other start script means the author runs a server, and that server is the
// app.
func readStaticSiteBuild(contextDir string) (staticSite, bool) {
	body, err := os.ReadFile(filepath.Join(contextDir, "package.json"))
	if err != nil {
		return staticSite{}, false
	}
	var pkg struct {
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		Engines         map[string]any    `json:"engines"`
	}
	if json.Unmarshal(body, &pkg) != nil {
		return staticSite{}, false
	}
	if strings.TrimSpace(pkg.Scripts["build"]) == "" {
		return staticSite{}, false
	}
	// A server-side app that builds its assets with Vite — Laravel, Rails,
	// Django — has a package.json with a build script too, and its build
	// writes assets for that server rather than a site. Served as files, a
	// Laravel app became nginx over a dist/ directory that was never written
	// (issue #55).
	for _, backend := range []string{"composer.json", "artisan", "Gemfile", "requirements.txt",
		"pyproject.toml", "manage.py", "go.mod", "Cargo.toml", "pom.xml", "mix.exs"} {
		if _, err := os.Stat(filepath.Join(contextDir, backend)); err == nil {
			return staticSite{}, false
		}
	}

	start := strings.TrimSpace(pkg.Scripts["start"])
	servedDir := ""
	switch {
	case start == "", devServers.MatchString(start):
	case staticServers.MatchString(start):
		servedDir = staticServers.FindStringSubmatch(start)[3]
	default:
		return staticSite{}, false
	}

	has := func(name string) bool {
		_, dep := pkg.Dependencies[name]
		_, dev := pkg.DevDependencies[name]
		return dep || dev
	}
	for _, f := range staticSiteFrameworks {
		if !has(f.dependency) {
			continue
		}
		// An Astro site with a server adapter renders on request and is not a
		// directory of files.
		if f.dependency == "astro" && hasAstroServerAdapter(pkg.Dependencies, pkg.DevDependencies) {
			return staticSite{}, false
		}
		out := f.outDir
		if f.dependency == "@angular/core" {
			out = angularOutputDir(contextDir)
		}
		if servedDir != "" {
			out = servedDir
		}
		if !safeRelative(out) {
			return staticSite{}, false
		}
		return staticSite{
			Framework: f.dependency,
			OutDir:    path.Clean(out),
			Install:   installCommand(contextDir),
			Node:      nodeMajor(contextDir, pkg.Engines),
		}, true
	}

	// No known framework, but a start script that serves a directory the
	// build writes: the rollup-built Svelte template is this shape.
	if servedDir != "" && safeRelative(servedDir) {
		return staticSite{
			Framework: "static",
			OutDir:    path.Clean(servedDir),
			Install:   installCommand(contextDir),
			Node:      nodeMajor(contextDir, pkg.Engines),
		}, true
	}
	return staticSite{}, false
}

// safeRelative accepts a directory inside the build context and nothing that
// could climb out of it or be read as more than one path.
func safeRelative(dir string) bool {
	if dir == "" || strings.ContainsAny(dir, " \t'\"$`\\;&|") {
		return false
	}
	clean := path.Clean(dir)
	return !path.IsAbs(clean) && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

// angularOutputDir reads where `ng build` writes the browser bundle.
//
// angular.json names an output path per project; the application builder
// (Angular 17 and later) writes the site into a browser/ directory beneath it.
func angularOutputDir(contextDir string) string {
	body, err := os.ReadFile(filepath.Join(contextDir, "angular.json"))
	if err != nil {
		return "dist"
	}
	var cfg struct {
		DefaultProject string `json:"defaultProject"`
		Projects       map[string]struct {
			Architect struct {
				Build struct {
					Builder string `json:"builder"`
					Options struct {
						OutputPath json.RawMessage `json:"outputPath"`
					} `json:"options"`
				} `json:"build"`
			} `json:"architect"`
		} `json:"projects"`
	}
	if json.Unmarshal(body, &cfg) != nil || len(cfg.Projects) == 0 {
		return "dist"
	}
	name := cfg.DefaultProject
	if _, ok := cfg.Projects[name]; !ok {
		for n := range cfg.Projects {
			name = n
			break
		}
	}
	build := cfg.Projects[name].Architect.Build
	application := strings.HasSuffix(build.Builder, ":application")

	var out string
	var asObject struct {
		Base    string `json:"base"`
		Browser *string
	}
	switch {
	case json.Unmarshal(build.Options.OutputPath, &out) == nil && out != "":
	case json.Unmarshal(build.Options.OutputPath, &asObject) == nil && asObject.Base != "":
		browser := "browser"
		if asObject.Browser != nil {
			browser = *asObject.Browser
		}
		return path.Join(asObject.Base, browser)
	default:
		out = path.Join("dist", name)
	}
	if application {
		return path.Join(out, "browser")
	}
	return out
}

var leadingMajor = regexp.MustCompile(`^\D*(\d+)`)

// nodeMajor is the Node major the site is built with: what .nvmrc or
// .node-version says, or a single major engines.node names, or the default.
// A range wider than one major gets the default, which is inside nearly every
// range a current framework declares.
func nodeMajor(contextDir string, engines map[string]any) string {
	for _, name := range []string{".nvmrc", ".node-version"} {
		body, err := os.ReadFile(filepath.Join(contextDir, name))
		if err != nil {
			continue
		}
		if m := leadingMajor.FindStringSubmatch(strings.TrimSpace(string(body))); m != nil {
			return m[1]
		}
	}
	if v, ok := engines["node"].(string); ok {
		v = strings.TrimSpace(v)
		if regexp.MustCompile(`^[\^~]?\d+(\.[\dx*]+)*$`).MatchString(v) {
			return leadingMajor.FindStringSubmatch(v)[1]
		}
	}
	return staticSiteNodeDefault
}

func hasAstroServerAdapter(deps ...map[string]string) bool {
	for _, m := range deps {
		for name := range m {
			if strings.HasPrefix(name, "@astrojs/") && (strings.HasSuffix(name, "node") ||
				strings.HasSuffix(name, "vercel") || strings.HasSuffix(name, "netlify") ||
				strings.HasSuffix(name, "cloudflare")) {
				return true
			}
		}
	}
	return false
}

// installCommand installs dependencies the way the lockfile says.
func installCommand(contextDir string) string {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(contextDir, name))
		return err == nil
	}
	switch {
	case has("pnpm-lock.yaml"):
		return "corepack enable && pnpm install --frozen-lockfile"
	case has("yarn.lock"):
		return "corepack enable && yarn install --frozen-lockfile"
	case has("package-lock.json"):
		return "npm ci"
	default:
		return "npm install"
	}
}

// writeStaticSitePlan writes a plan that builds the site and serves its output.
//
// Into .pando-plan/, beside where a Railpack plan goes, so detection collects it,
// the spec carries it and the build replays it exactly as it would a Railpack
// plan (R-020). The server is the one a committed static site gets.
func writeStaticSitePlan(contextDir string, site staticSite) (string, error) {
	name := filepath.Join(planDir, "Dockerfile")
	content := fmt.Sprintf(`# A %s site that builds to static files, served by nginx.
FROM node:%s-alpine AS build
WORKDIR /app
COPY . .
RUN %s
RUN npm run build

FROM %s
COPY --from=build /app/%s/ /usr/share/nginx/html/
RUN printf %s > /etc/nginx/conf.d/default.conf
EXPOSE 80
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec nginx -g 'daemon off;'"]
`, site.Framework, site.Node, site.Install, staticServerImage, site.OutDir, printfFormat(staticConfig))

	full := filepath.Join(contextDir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}
	// G306: a generated build input in the build context, read by the rootless
	// builder as a different user. It holds no secret.
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil { //nolint:gosec
		return "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}
	return name, nil
}
