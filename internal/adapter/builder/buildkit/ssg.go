package buildkit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// Static site generators that are not Node packages: Hugo, Zola and MkDocs.
//
// Each reads a config file and a content directory and writes a site. None has
// a package.json for the Node static path to read, and a Hugo or Zola site has
// no manifest of any kind, so detection found "no recognizable project layout"
// and asked how to build it; MkDocs' requirements.txt made it a Python app on
// port 8000 with a start command to supply (issue #90). R-094 counts a known
// generator's config as static-site evidence, and what it means is a build
// first. Each is built with the generator's own release and served by the same
// nginx and config every other static site gets.

// hugoDefault and zolaDefault are the releases a site that names none is built
// with. [P] The current release of each.
const (
	hugoDefault = "0.167.0"
	zolaDefault = "0.23.6"
)

// mkdocsPython is the Python an MkDocs site is built with. [P] A current
// release every MkDocs plugin supports.
const mkdocsPython = "3.12"

// hugoBuildImage is what a Hugo site is built on. [P] The Go image: it has
// git and curl, and a site using Hugo Modules needs Go to fetch them.
const hugoBuildImage = "golang:1.25"

// zolaBuildImage is what a Zola site is built on: Debian with curl.
const zolaBuildImage = "buildpack-deps:bookworm-curl"

// ssgSite is what reading the repository said about a generator's site.
type ssgSite struct {
	Generator string // "Hugo", "Zola", "MkDocs"
	Version   string
	Install   string // MkDocs: what pip installs
}

var (
	hugoBaseURL  = regexp.MustCompile(`(?im)^\s*["']?baseurl["']?\s*[:=]`)
	zolaBaseURL  = regexp.MustCompile(`(?m)^\s*base_url\s*=`)
	hugoTheme    = regexp.MustCompile(`(?m)^\s*["']?theme["']?\s*[:=]\s*["']?([A-Za-z0-9._-]+)`)
	gitmoduleDir = regexp.MustCompile(`(?m)^\s*path\s*=\s*(\S+)\s*$`)
)

// readSSGSite reports a Hugo, Zola or MkDocs site. An error is a site that
// cannot be built from what the repository holds.
func readSSGSite(contextDir string) (ssgSite, bool, error) {
	if site, ok, err := readHugoSite(contextDir); ok || err != nil {
		return site, ok, err
	}
	if site, ok := readZolaSite(contextDir); ok {
		return site, true, nil
	}
	if site, ok := readMkDocsSite(contextDir); ok {
		return site, true, nil
	}
	return ssgSite{}, false, nil
}

// readHugoSite reports a Hugo site: a hugo.toml (or .yaml, .json), or a
// config.toml naming a baseURL beside one of the directories Hugo reads.
func readHugoSite(contextDir string) (ssgSite, bool, error) {
	config, found := hugoNamedConfig(contextDir)
	if !found {
		config, found = hugoGenericConfig(contextDir)
	}
	if !found {
		return ssgSite{}, false, nil
	}
	if err := hugoThemeIsHere(contextDir, config); err != nil {
		return ssgSite{}, false, err
	}
	return ssgSite{Generator: "Hugo", Version: pinnedVersion(contextDir, "HUGO_VERSION|hugo-version", hugoDefault)}, true, nil
}

// hugoNamedConfig is the body of a configuration file only Hugo uses, and
// whether there is one.
func hugoNamedConfig(contextDir string) (string, bool) {
	for _, name := range []string{"hugo.toml", "hugo.yaml", "hugo.yml", "hugo.json",
		"config/_default/hugo.toml", "config/_default/hugo.yaml", "config/_default/config.toml"} {
		if fileExists(contextDir, name) {
			return readFile(contextDir, name), true
		}
	}
	return "", false
}

// hugoGenericConfig is the body of a config.toml (or .yaml, .json) naming a
// Hugo baseURL beside one of the directories Hugo reads, and whether there is
// one.
func hugoGenericConfig(contextDir string) (string, bool) {
	marked := false
	for _, dir := range []string{"layouts", "content", "themes", "archetypes"} {
		if info, err := os.Stat(filepath.Join(contextDir, dir)); err == nil && info.IsDir() {
			marked = true
		}
	}
	if !marked {
		return "", false
	}
	for _, name := range []string{"config.toml", "config.yaml", "config.yml", "config.json"} {
		body := readFile(contextDir, name)
		if hugoBaseURL.MatchString(body) && !zolaBaseURL.MatchString(body) {
			return body, true
		}
	}
	return "", false
}

// hugoThemeIsHere refuses a site whose theme is a git submodule.
//
// Pando does not fetch submodules (design 03 §10, R-021), so the theme
// directory is empty in the checkout. Built anyway, Hugo found no layouts and
// wrote a site of empty pages, which deploys and looks broken.
func hugoThemeIsHere(contextDir, config string) error {
	m := hugoTheme.FindStringSubmatch(config)
	if m == nil {
		return nil
	}
	dir := "themes/" + m[1]
	modules := readFile(contextDir, ".gitmodules")
	submodule := false
	for _, p := range gitmoduleDir.FindAllStringSubmatch(modules, -1) {
		if strings.Trim(p[1], `"/`) == dir {
			submodule = true
		}
	}
	if !submodule {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(contextDir, filepath.FromSlash(dir)))
	if err == nil && len(entries) > 0 {
		return nil
	}
	return errs.Newf(errs.ValidInvalid,
		"This Hugo site's theme, %s, is a git submodule at %s, and Pando builds only the files in the "+
			"repository itself: it does not fetch submodules. Without the theme, Hugo has no page layouts "+
			"and builds a site of empty pages.", m[1], dir).
		WithRemedy("Commit the theme's files into the repository in place of the submodule, or import the " +
			"theme as a Hugo Module.")
}

// readZolaSite reports a Zola site: a config.toml naming a base_url beside
// templates/ or content/.
func readZolaSite(contextDir string) (ssgSite, bool) {
	if !zolaBaseURL.MatchString(readFile(contextDir, "config.toml")) {
		return ssgSite{}, false
	}
	for _, dir := range []string{"templates", "content"} {
		if info, err := os.Stat(filepath.Join(contextDir, dir)); err == nil && info.IsDir() {
			return ssgSite{Generator: "Zola", Version: pinnedVersion(contextDir, "ZOLA_VERSION|zola-version", zolaDefault)}, true
		}
	}
	return ssgSite{}, false
}

var mkdocsMaterial = regexp.MustCompile(`(?m)^\s*(theme:\s*material\b|name:\s*["']?material\b)`)

// readMkDocsSite reports an MkDocs site: an mkdocs.yml in a repository that is
// not also a Python app, whose documentation it would only be.
func readMkDocsSite(contextDir string) (ssgSite, bool) {
	config := readFile(contextDir, "mkdocs.yml")
	if config == "" {
		config = readFile(contextDir, "mkdocs.yaml")
	}
	if config == "" {
		return ssgSite{}, false
	}
	for _, app := range []string{"manage.py", "app.py", "main.py", "wsgi.py", "asgi.py", "server.py", "Procfile"} {
		if fileExists(contextDir, app) {
			return ssgSite{}, false
		}
	}
	install := "mkdocs"
	if mkdocsMaterial.MatchString(config) {
		install = "mkdocs mkdocs-material"
	}
	for _, requirements := range []string{"requirements.txt", "docs/requirements.txt"} {
		if fileExists(contextDir, requirements) {
			install = "-r " + requirements
			break
		}
	}
	return ssgSite{Generator: "MkDocs", Version: mkdocsPython, Install: install}, true
}

// pinnedVersion is the release a site's other build configuration pins —
// Netlify's and Vercel's environment, a GitHub Actions workflow — or the
// default.
func pinnedVersion(contextDir, keys, fallback string) string {
	pattern := regexp.MustCompile(`(?i)(?:` + keys + `)["']?\s*[:=]\s*["']?v?(\d+\.\d+(?:\.\d+)?)\b`)
	files := []string{"netlify.toml", "vercel.json"}
	workflows, _ := filepath.Glob(filepath.Join(contextDir, ".github", "workflows", "*.y*ml"))
	for _, w := range workflows {
		files = append(files, filepath.Join(".github", "workflows", filepath.Base(w)))
	}
	for _, name := range files {
		if m := pattern.FindStringSubmatch(readFile(contextDir, name)); m != nil {
			return m[1]
		}
	}
	return fallback
}

// hugoArchive is the name of a Hugo extended release archive, with the
// architecture left to the shell. Releases before 0.103 named their archives
// by platform rather than by architecture.
func hugoArchive(version string) (archive, arch string) {
	parts := strings.Split(version, ".")
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	if parts[0] == "0" && minor < 103 {
		return "hugo_extended_${HUGO_VERSION}_Linux-${arch}.tar.gz",
			`case "$TARGETARCH" in arm64) arch=ARM64;; *) arch=64bit;; esac`
	}
	return "hugo_extended_${HUGO_VERSION}_linux-${arch}.tar.gz",
		`case "$TARGETARCH" in arm64) arch=arm64;; *) arch=amd64;; esac`
}

// writeSSGPlan writes a plan that builds the site with its generator and
// serves what the generator writes.
//
// The output goes outside the source, and the site is built for the root of
// the app's own address, which is where Pando serves every app: a baseURL
// naming the author's own domain would otherwise send every stylesheet link
// there.
func writeSSGPlan(contextDir string, site ssgSite) (string, error) {
	var build string
	switch site.Generator {
	case "Hugo":
		archive, arch := hugoArchive(site.Version)
		build = fmt.Sprintf(`FROM %s AS build
ARG TARGETARCH
ENV HUGO_VERSION=%s HUGO_ENVIRONMENT=production
RUN %s && \
    curl -fsSL "https://github.com/gohugoio/hugo/releases/download/v${HUGO_VERSION}/%s" | tar -xz -C /usr/local/bin hugo
WORKDIR /site
COPY . .
RUN hugo --baseURL / --destination /tmp/site
`, hugoBuildImage, site.Version, arch, archive)
	case "Zola":
		build = fmt.Sprintf(`FROM %s AS build
ARG TARGETARCH
ENV ZOLA_VERSION=%s
RUN case "$TARGETARCH" in arm64) arch=aarch64;; *) arch=x86_64;; esac && \
    curl -fsSL "https://github.com/getzola/zola/releases/download/v${ZOLA_VERSION}/zola-v${ZOLA_VERSION}-${arch}-unknown-linux-gnu.tar.gz" | tar -xz -C /usr/local/bin zola
WORKDIR /site
COPY . .
RUN zola build --base-url / --output-dir /tmp/site
`, zolaBuildImage, site.Version)
	case "MkDocs":
		build = fmt.Sprintf(`FROM python:%s-slim AS build
WORKDIR /site
COPY . .
RUN pip install --no-cache-dir %s
RUN mkdocs build --site-dir /tmp/site
`, site.Version, site.Install)
	default:
		return "", errs.Newf(errs.Internal, "No plan for a %s site.", site.Generator)
	}
	content := fmt.Sprintf(`# A %s site, built with %s %s and served by nginx.
%s
FROM %s
COPY --from=build /tmp/site/ /usr/share/nginx/html/
RUN printf %s > /etc/nginx/conf.d/default.conf
EXPOSE 80
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec nginx -g 'daemon off;'"]
`, site.Generator, generatorTool(site), site.Version, build, staticServerImage, printfFormat(staticConfig))
	return writePlan(contextDir, content)
}

// generatorTool names what the version in a plan's header is the version of.
func generatorTool(site ssgSite) string {
	if site.Generator == "MkDocs" {
		return "MkDocs on Python"
	}
	return site.Generator
}
