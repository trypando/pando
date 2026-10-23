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
// Astro, Vite, Angular, Create React App, Gatsby, Docusaurus, VitePress,
// Eleventy, Hexo, a SvelteKit app on adapter-static and a Nuxt app that is
// generated rather than served all build a directory of HTML and assets. What
// runs afterwards is either nothing, or the framework's own development server,
// or a small static file server — none of which is how a built site should be
// served. Through nixpacks these failed three ways (issue #55): "No start
// command could be found" for a site with nothing to start; a Vite 8 build that
// could not find its native binding under nixpacks' Node; and an Angular build
// refused by the Node nixpacks pins. What such a site needs is what a committed
// static site gets: build it, then serve the output.
//
// So a site recognized here is planned by Pando rather than by Railpack, with
// the official Node image to build and the same nginx server and config a
// committed site gets.
//
// Where the output lands is each generator's own: SvelteKit's adapter-static
// writes build/, VitePress .vitepress/dist, Eleventy _site, Nuxt
// .output/public. Read as dist/ for all of them, the build succeeded and the
// image failed on `COPY /app/dist/: not found` (issue #90). A literal the
// project's config or build script states outranks the default.

// staticSiteNodeDefault is the Node major the site is built with when it names
// none. [P] 22, an active LTS: current Vite, Next.js, Angular and NestJS
// refuse 18 with EBADENGINE (issue #55).
const staticSiteNodeDefault = "22"

// staticFramework is a generator that builds to a directory.
type staticFramework struct {
	dependency string
	// outDir is where the build writes, relative to the project, read from
	// what the project states or the generator's default.
	outDir func(contextDir string, scripts map[string]string) string
	// static reports whether this project builds a site rather than a server.
	// Nil means it always does. A SvelteKit app on adapter-node and a Nuxt
	// app that renders on request use the same build commands and produce a
	// server; planned as a site, the build wrote build/ and the image copied a
	// dist/ that was never there (issue #90).
	static func(contextDir string, scripts map[string]string, has func(string) bool) bool
	// build is the command that builds the site, when it is not the build
	// script.
	build func(scripts map[string]string) string
}

// staticSiteFrameworks are tried in order, and the first whose package the
// project depends on decides. The specific generators come before Vite,
// because SvelteKit, VitePress and most of the others depend on it too.
var staticSiteFrameworks = []staticFramework{
	{dependency: "@sveltejs/kit", outDir: svelteKitStaticOutDir, static: isSvelteKitStatic},
	{dependency: "nuxt", outDir: fixedOutDir(".output/public"), static: isNuxtGenerated, build: nuxtGenerate},
	{dependency: "vitepress", outDir: vitePressOutDir},
	{dependency: "@docusaurus/core", outDir: fixedOutDir("build")},
	{dependency: "@11ty/eleventy", outDir: eleventyOutDir},
	{dependency: "hexo", outDir: hexoOutDir},
	{dependency: "astro", outDir: configOutDir(readAstroOutDir, "dist"),
		static: func(_ string, _ map[string]string, has func(string) bool) bool { return !hasAstroServerAdapter(has) }},
	{dependency: "@angular/core", outDir: func(dir string, _ map[string]string) string { return angularOutputDir(dir) }},
	{dependency: "gatsby", outDir: fixedOutDir("public")},
	{dependency: "react-scripts", outDir: fixedOutDir("build")},
	{dependency: "@vue/cli-service", outDir: vueCLIOutDir},
	{dependency: "vite", outDir: configOutDir(readViteOutDir, "dist")},
}

var vueCLIOutputDir = regexp.MustCompile(`\boutputDir\s*:\s*["']([^"']+)["']`)

// vueCLIOutDir is vue.config.js's outputDir, or dist. A Vue CLI project has no
// start script, only `vue-cli-service serve`, its development server, and
// detection asked how to start Render's Vue sample (issue #90).
func vueCLIOutDir(contextDir string, _ map[string]string) string {
	if _, config := readFileIn(contextDir, ".", "vue.config.js", "vue.config.cjs", "vue.config.mjs", "vue.config.ts"); config != nil {
		if out := submatch(vueCLIOutputDir, config); safeRelative(out) {
			return out
		}
	}
	return "dist"
}

func fixedOutDir(dir string) func(string, map[string]string) string {
	return func(string, map[string]string) string { return dir }
}

// configOutDir reads the output directory from the project's own config, as
// outdir.go reads it for an embedded client, or falls back to the default.
func configOutDir(read outDirReader, fallback string) func(string, map[string]string) string {
	return func(contextDir string, scripts map[string]string) string {
		if _, out := read(contextDir, ".", scripts); out != "" {
			return out
		}
		return fallback
	}
}

// devServers are commands that run a framework's development or preview
// server, which is a way to work on a site or look at a build of it rather
// than to serve one.
var devServers = regexp.MustCompile(`^(npx\s+)?(` +
	`vite(\s+(dev|serve|preview))?|astro\s+(dev|preview)|gatsby\s+(develop|serve)|ng\s+serve|` +
	`react-scripts\s+start|docusaurus\s+(start|serve)|vitepress(\s+(dev|serve|preview))?|` +
	`hexo\s+(server|s)|(@11ty/)?eleventy\s+(.*\s)?--serve|nuxi?\s+(dev|preview)|vue-cli-service\s+serve` +
	`)(\s|$)`)

// flagPort is the long port option the static servers share.
const flagPort = "--port"

// staticServers are commands that serve a directory as it is. The directory is
// the site; the server is replaced with nginx. sirv binds to localhost unless
// told otherwise, and a site started that way was unreachable.
//
// Each takes options before or after the directory, and the ones listed take a
// value: `serve -s _static -l tcp://0.0.0.0:$PORT` serves _static (issue #90).
var staticServers = map[string]map[string]bool{
	"serve": {"-l": true, "--listen": true, "-p": true, flagPort: true, "-c": true, "--config": true,
		"--ssl-cert": true, "--ssl-key": true, "--ssl-pass": true},
	"http-server": {"-p": true, flagPort: true, "-a": true, "-c": true, "-e": true, "--ext": true,
		"-P": true, "--proxy": true, "-C": true, "--cert": true, "-K": true, "--key": true, "-t": true, "--timeout": true},
	"sirv": {"-p": true, flagPort: true, "-H": true, "--host": true, "-m": true, "--maxage": true,
		"-k": true, "--key": true, "-C": true, "--cert": true, "--cacert": true, "--pass": true},
}

// servedDirectory is the directory a static file server command serves, and
// whether the command is one.
func servedDirectory(command string) (string, bool) {
	fields := strings.Fields(command)
	if len(fields) > 0 && fields[0] == "npx" {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return "", false
	}
	valued, ok := staticServers[fields[0]]
	if !ok {
		return "", false
	}
	for i := 1; i < len(fields); i++ {
		arg := fields[i]
		switch {
		case strings.HasPrefix(arg, "-") && strings.Contains(arg, "="):
		case valued[arg]:
			i++ // the option's value
		case strings.HasPrefix(arg, "-"):
		default:
			return arg, true
		}
	}
	// No directory: the server serves the one it was started in.
	return ".", true
}

// staticSite is what reading package.json said about a static site build.
type staticSite struct {
	Framework string
	OutDir    string
	Install   string
	// Build is the command that builds the site. Empty means `npm run build`.
	Build string
	Node  string
}

// packageJSON is the part of package.json the plans here read.
type packageJSON struct {
	Main            string            `json:"main"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Engines         map[string]any    `json:"engines"`
	PackageManager  string            `json:"packageManager"`
	Workspaces      json.RawMessage   `json:"workspaces"`
	Pnpm            map[string]any    `json:"pnpm"`
}

func readPackageJSON(contextDir string) (packageJSON, bool) {
	body, err := os.ReadFile(filepath.Join(contextDir, "package.json"))
	if err != nil {
		return packageJSON{}, false
	}
	var pkg packageJSON
	if json.Unmarshal(body, &pkg) != nil {
		return packageJSON{}, false
	}
	return pkg, true
}

func (p packageJSON) has(name string) bool {
	_, dep := p.Dependencies[name]
	_, dev := p.DevDependencies[name]
	return dep || dev
}

// notNodeProject names the files that make a package.json belong to something
// else.
//
// A server-side app that builds its assets with Vite — Laravel, Rails,
// Django — has a package.json with a build script too, and its build writes
// assets for that server rather than a site. Served as files, a Laravel app
// became nginx over a dist/ directory that was never written (issue #55). A
// Maven or Gradle build that runs the front end's npm build through its own
// plugins is a JVM app (JHipster); a deno.json makes it a Deno app whose
// scripts run under Deno; and a .meteor directory is a Meteor app, built with
// Meteor's own tool (issue #90).
var notNodeProject = []string{"composer.json", "artisan", "Gemfile", "requirements.txt",
	"pyproject.toml", "manage.py", "go.mod", "Cargo.toml", "pom.xml", "mix.exs",
	"build.gradle", "build.gradle.kts", "deno.json", "deno.jsonc", ".meteor/release"}

// belongsToAnotherToolchain reports a repository whose package.json is not the
// project.
func belongsToAnotherToolchain(contextDir string, names ...string) bool {
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(contextDir, filepath.FromSlash(name))); err == nil {
			return true
		}
	}
	return false
}

// readStaticSiteBuild reports whether the repository is a site that builds to
// static files: a build script, a framework that outputs a directory, and no
// start script other than a development server or a static file server. Any
// other start script means the author runs a server, and that server is the
// app.
func readStaticSiteBuild(contextDir string) (staticSite, bool) {
	pkg, ok := readPackageJSON(contextDir)
	if !ok {
		return staticSite{}, false
	}
	if strings.TrimSpace(pkg.Scripts["build"]) == "" {
		return staticSite{}, false
	}
	if belongsToAnotherToolchain(contextDir, notNodeProject...) {
		return staticSite{}, false
	}

	start := servingCommand(pkg.Scripts)
	servedDir := ""
	switch dir, isServer := servedDirectory(start); {
	case start == "", devServers.MatchString(start):
	case isServer:
		servedDir = dir
	default:
		return staticSite{}, false
	}

	site := staticSite{
		Install: installCommand(contextDir),
		Node:    nodeMajor(contextDir, pkg.Engines),
	}
	if usesBun(contextDir) {
		site.Build = "bun run build"
	}
	for _, f := range staticSiteFrameworks {
		if !pkg.has(f.dependency) {
			continue
		}
		if f.static != nil && !f.static(contextDir, pkg.Scripts, pkg.has) {
			return staticSite{}, false
		}
		out := f.outDir(contextDir, pkg.Scripts)
		// A flag in the build command that runs beats any config file.
		if _, flag := readScriptOutDir(contextDir, ".", pkg.Scripts); flag != "" {
			out = flag
		}
		if servedDir != "" {
			out = servedDir
		}
		if !safeRelative(out) {
			return staticSite{}, false
		}
		site.Framework, site.OutDir = f.dependency, path.Clean(out)
		if f.build != nil {
			site.Build = f.build(pkg.Scripts)
		}
		return site, true
	}

	// No known framework, but a start script that serves a directory the
	// build writes: the rollup-built Svelte template is this shape.
	if servedDir != "" && safeRelative(servedDir) {
		site.Framework, site.OutDir = "static", path.Clean(servedDir)
		return site, true
	}
	if start != "" {
		return staticSite{}, false
	}
	// No known framework and nothing that starts a server, but a committed
	// copy of the site in the directory builds conventionally write to: the
	// build makes that directory, and it is what is served. html5-boilerplate
	// builds with gulp into a committed dist/. nixpacks served such a
	// directory itself; Railpack has no start command for it (issue #172).
	for _, out := range []string{"dist", "build"} {
		if fileExists(contextDir, path.Join(out, "index.html")) {
			site.Framework, site.OutDir = "static", out
			return site, true
		}
	}
	// A page at the root and a build script with nothing to build it with: no
	// dependencies, so no bundler, and the build edits the files in place —
	// Render's static sample fills placeholders in its index.html. Railpack
	// read it as a Node server and asked what starts it (issue #90).
	if fileExists(contextDir, "index.html") && len(pkg.Dependencies)+len(pkg.DevDependencies) == 0 {
		site.Framework, site.OutDir = "static", "."
		return site, true
	}
	return staticSite{}, false
}

// commandSeparator splits a script into the commands it runs one after another.
// A line break separates commands as `;` does.
var commandSeparator = regexp.MustCompile(`[ \t]*(?:&&|\|\||;|\r?\n)[ \t]*`)

// lineContinuation is a backslash before a line break, which continues the
// command rather than ending it.
var lineContinuation = regexp.MustCompile(`\\\r?\n`)

// splitCommands is the commands a script runs one after another.
func splitCommands(script string) []string {
	return commandSeparator.Split(lineContinuation.ReplaceAllString(script, " "), -1)
}

// runsScript matches a command that runs another script of the same
// package.json.
var runsScript = regexp.MustCompile(`^(?:npm\s+(?:run(?:-script)?\s+)?|yarn\s+(?:run\s+)?|pnpm\s+(?:run\s+)?|bun\s+run\s+)([A-Za-z0-9:_.-]+)$`)

// servingCommand is the command the start script ends up running: the last
// of the commands it runs in turn, through any `npm run` of another script,
// and without the environment assignments in front of it.
//
// DigitalOcean's samples start with `node sammy.js; npm run serve`, which
// prints a banner and then runs `gatsby serve` (issue #90). The banner is not
// the server.
func servingCommand(scripts map[string]string) string {
	command := strings.TrimSpace(scripts["start"])
	for range 4 {
		parts := splitCommands(command)
		command = withoutEnvironment(parts[len(parts)-1])
		m := runsScript.FindStringSubmatch(command)
		if m == nil {
			return command
		}
		next, ok := scripts[m[1]]
		if !ok {
			return command
		}
		command = strings.TrimSpace(next)
	}
	return command
}

var envPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*$`)

// withoutEnvironment drops `NODE_ENV=production` and `cross-env FOO=1` from the
// front of a command.
func withoutEnvironment(command string) string {
	fields := strings.Fields(command)
	for len(fields) > 0 && (envPrefix.MatchString(fields[0]) || fields[0] == "cross-env" || fields[0] == "env") {
		fields = fields[1:]
	}
	return strings.Join(fields, " ")
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

// --- where each generator writes -------------------------------------------

// isSvelteKitStatic reports a SvelteKit app on adapter-static. Any other
// adapter builds a server (node.go starts it).
func isSvelteKitStatic(_ string, _ map[string]string, has func(string) bool) bool {
	return has("@sveltejs/adapter-static")
}

// svelteKitStaticOutDir is adapter-static's `pages` option, or its default.
func svelteKitStaticOutDir(contextDir string, scripts map[string]string) string {
	if _, out := readSvelteKitOutDir(contextDir, ".", scripts); out != "" {
		return out
	}
	return "build"
}

var (
	nuxtNoSSR        = regexp.MustCompile(`\bssr\s*:\s*false\b`)
	nuxtStaticPreset = regexp.MustCompile(`preset\s*:\s*["'](static|github[-_]pages)["']`)
	nuxtPrerender    = regexp.MustCompile(`\bprerender\s*:`)
)

// isNuxtGenerated reports a Nuxt app whose author generates it to files rather
// than running its server: its build script is `nuxt generate`, its config
// turns server rendering off or picks the static preset, or it prerenders its
// routes and has no server/ directory of API routes that would need a server.
func isNuxtGenerated(contextDir string, scripts map[string]string, _ func(string) bool) bool {
	if strings.Contains(scripts["build"], "generate") {
		return true
	}
	_, config := readFileIn(contextDir, ".", "nuxt.config.ts", "nuxt.config.js", "nuxt.config.mjs")
	if config == nil {
		return false
	}
	if nuxtNoSSR.Match(config) || nuxtStaticPreset.Match(config) {
		return true
	}
	info, err := os.Stat(filepath.Join(contextDir, "server"))
	hasServer := err == nil && info.IsDir()
	return nuxtPrerender.Match(config) && !hasServer
}

// nuxtGenerate is the command that writes a Nuxt app out as files.
func nuxtGenerate(scripts map[string]string) string {
	if strings.TrimSpace(scripts["generate"]) != "" {
		return "npm run generate"
	}
	return "npx nuxi generate"
}

var (
	vitePressBuild = regexp.MustCompile(`vitepress\s+build(?:\s+([^\s-][^\s;&|]*))?`)
	vitePressNames = []string{"config.mts", "config.ts", "config.mjs", "config.js", "config.cts", "config.cjs"}
)

// vitePressOutDir is where `vitepress build [root]` writes: the config's
// outDir, relative to the root, or .vitepress/dist under it.
func vitePressOutDir(contextDir string, scripts map[string]string) string {
	root := "."
	if m := vitePressBuild.FindStringSubmatch(scripts["build"]); m != nil && m[1] != "" {
		root = m[1]
	}
	_, config := readFileIn(contextDir, path.Join(root, ".vitepress"), vitePressNames...)
	if out := submatch(viteOutDirPattern, config); out != "" && config != nil {
		return path.Join(root, out)
	}
	return path.Join(root, ".vitepress", "dist")
}

var (
	eleventyOutputFlag   = regexp.MustCompile(`eleventy\S*\s+(?:.*\s)?--output(?:=|\s+)([^\s"';|&]+)`)
	eleventyOutputConfig = regexp.MustCompile(`\boutput\s*:\s*["']([^"']+)["']`)
)

// eleventyOutDir is Eleventy's --output, its config's dir.output, or _site.
func eleventyOutDir(contextDir string, scripts map[string]string) string {
	if m := eleventyOutputFlag.FindStringSubmatch(scripts["build"]); m != nil {
		return m[1]
	}
	_, config := readFileIn(contextDir, ".", "eleventy.config.js", "eleventy.config.mjs",
		"eleventy.config.cjs", "eleventy.config.ts", ".eleventy.js", ".eleventy.cjs")
	if out := submatch(eleventyOutputConfig, config); config != nil && out != "" {
		return out
	}
	return "_site"
}

var hexoPublicDir = regexp.MustCompile(`(?m)^public_dir:\s*["']?([^"'\s#]+)`)

// hexoOutDir is _config.yml's public_dir, or public.
func hexoOutDir(contextDir string, _ map[string]string) string {
	if _, config := readFileIn(contextDir, ".", "_config.yml", "_config.yaml"); config != nil {
		if out := submatch(hexoPublicDir, config); out != "" {
			return out
		}
	}
	return "public"
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

func hasAstroServerAdapter(has func(string) bool) bool {
	for _, adapter := range []string{"@astrojs/node", "@astrojs/vercel", "@astrojs/netlify", "@astrojs/cloudflare"} {
		if has(adapter) {
			return true
		}
	}
	return false
}

// writeStaticSitePlan writes a plan that builds the site and serves its output.
//
// Into .pando-plan/, beside where a Railpack plan goes, so detection collects it,
// the spec carries it and the build replays it exactly as it would a Railpack
// plan (R-020). The server is the one a committed static site gets.
//
// Built on Debian rather than Alpine: Gatsby's rendering engine and other
// native modules ship glibc binaries, and on Alpine the build stopped at
// "Error loading shared library ld-linux-x86-64.so.2" (issue #90).
func writeStaticSitePlan(contextDir string, site staticSite) (string, error) {
	build := site.Build
	if build == "" {
		build = "npm run build"
	}
	out := site.OutDir + "/"
	if site.OutDir == "." {
		// Built in place: what is served is the checkout, less what is not
		// part of the site.
		build += " && rm -rf .git " + planDir + " node_modules"
		out = ""
	}
	name := filepath.Join(planDir, "Dockerfile")
	content := fmt.Sprintf(`# A %s site that builds to static files, served by nginx.
FROM node:%s-slim AS build
WORKDIR /app
COPY . .
RUN %s
RUN %s

FROM %s
COPY --from=build /app/%s /usr/share/nginx/html/
RUN printf %s > /etc/nginx/conf.d/default.conf
EXPOSE 80
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec nginx -g 'daemon off;'"]
`, site.Framework, site.Node, site.Install, build, staticServerImage, out, printfFormat(staticConfig))

	if err := writeContextFile(contextDir, name, []byte(content)); err != nil {
		return "", errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
	}
	return name, nil
}
