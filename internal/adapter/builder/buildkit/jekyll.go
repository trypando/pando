package buildkit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// A Jekyll site.
//
// GitHub Pages builds a repository with Jekyll before it serves it, so a Pages
// site's repository is the source for a site: Markdown pages, layouts, and a
// _config.yml that may name a theme fetched at build time. Served as committed
// it deployed, reached running, and answered every page written in Markdown
// with a 403 or the raw Markdown (issue #67). R-094 counts a known static site
// generator's config as the static tier's evidence; what that evidence means
// is a build first.
//
// Planned here rather than by the general-purpose planner, which reads the
// Gemfile as a Ruby app and asks what starts it. The site is built on the official Ruby image and
// served by the same nginx and config every other static site gets.

// jekyllGemfile is the Gemfile a site that has none is built with.
//
// GitHub Pages builds such a site with the github-pages gem, which pins Jekyll,
// its plugins and GitHub's defaults. Building with the same gem is building the
// site the way its author saw it, and it carries jekyll-remote-theme, which a
// Pages site naming a remote_theme needs.
const jekyllGemfile = `source "https://rubygems.org"
gem "github-pages", group: :jekyll_plugins
`

// jekyllGemfilePath is where that Gemfile is written: under .pando-plan/ so the
// plan carries it, and in a dot-directory so Jekyll leaves it out of the site.
//
// The build copies it to ./Gemfile rather than pointing BUNDLE_GEMFILE at it.
// Jekyll requires the :jekyll_plugins group only when a file named Gemfile is
// in the directory it runs in, and that group is what loads github-pages and
// the defaults Pages turns on. Pointed at from BUNDLE_GEMFILE, the gems were
// installed and never loaded: jekyll-optional-front-matter was off, and every
// Markdown page without front matter was copied out as Markdown (issue #67).
const jekyllGemfilePath = planDir + "/jekyll/Gemfile"

// jekyllServedConfig is read after the site's own _config.yml, and says where
// the site is served: at the root of the app's own address, which is where
// Pando serves every app.
//
// Without it, jekyll-github-metadata sets an unset url and baseurl to the
// site's GitHub Pages address in a production build. Built by Pando, that was a
// guess at a project page — /pages/<owner>/<repo> — and every stylesheet the
// theme linked through it was answered by the index.html fallback (issue #67).
// An empty value is set, and so is left alone; a baseurl the site sets itself
// is overridden too, because it names where GitHub serves the site, not Pando.
const jekyllServedConfig = `# Pando serves the site at the root of the app's own address.
url: ""
baseurl: ""
`

// jekyllServedConfigPath is where jekyllServedConfig is written, beside the
// generated Gemfile.
const jekyllServedConfigPath = planDir + "/jekyll/_config.pando.yml"

// jekyllMarkers are the directories only a Jekyll site has.
var jekyllMarkers = []string{"_layouts", "_includes", "_posts", "_sass", "_data"}

var (
	jekyllTheme = regexp.MustCompile(`(?m)^\s*(remote_)?theme\s*:`)
	jekyllGem   = regexp.MustCompile(`(?m)^\s*gem\s+['"](jekyll[A-Za-z0-9_-]*|github-pages)['"]`)
	gemspecLine = regexp.MustCompile(`(?m)^\s*gemspec\b`)
	gemspecDep  = regexp.MustCompile(`add_(?:runtime_)?dependency\s*\(?\s*['"]jekyll['"]`)
	lockedGem   = regexp.MustCompile(`(?m)^ {4}(jekyll|github-pages) \(`)
	pagesGem    = regexp.MustCompile(`(?m)^\s*gem\s+['"]github-pages['"]`)
)

// jekyllSite is what reading the repository said about a Jekyll site.
type jekyllSite struct {
	// OwnGemfile is true when the site is built with its own Gemfile. False
	// means it is built with jekyllGemfile, as GitHub Pages builds it: the site
	// has no Gemfile, or has one naming github-pages.
	OwnGemfile bool
	Ruby       string

	// Repository is the GitHub owner/name the site's origin names, or empty.
	Repository string

	// Config is the site's own configuration file: _config.yml or
	// _config.yaml.
	Config string
}

// readJekyllSite reports whether the repository is a Jekyll site: a _config.yml,
// and either a Gemfile that brings in Jekyll or something else only a Jekyll
// site has — a theme, or one of its underscore directories. A _config.yml
// alone is not enough; other tools use the name. A Gemfile that does not bring
// in Jekyll is a Ruby app's, whatever else is beside it.
func readJekyllSite(contextDir string) (jekyllSite, bool) {
	var config []byte
	var configName string
	for _, name := range []string{"_config.yml", "_config.yaml"} {
		if body, err := os.ReadFile(filepath.Join(contextDir, name)); err == nil {
			config, configName = body, name
			break
		}
	}
	if config == nil {
		return jekyllSite{}, false
	}
	// Hexo keeps its configuration in a _config.yml too, naming a theme, and
	// was built with `jekyll build` on a site with no Gemfile (issue #90). A
	// package.json that brings in Hexo says which generator the file is for.
	if pkg, ok := readPackageJSON(contextDir); ok && pkg.has("hexo") {
		return jekyllSite{}, false
	}

	if gemfile, err := os.ReadFile(filepath.Join(contextDir, "Gemfile")); err == nil {
		if !gemfileBuildsJekyll(contextDir, gemfile) {
			return jekyllSite{}, false
		}
		// A Gemfile naming github-pages is a Pages site's, and GitHub builds a
		// Pages site with its own current github-pages gem whatever the Gemfile
		// says. Resolved from the Gemfile instead, one naming jekyll-algolia
		// beside it got github-pages 222 and Jekyll 3.9, which do not run on a
		// current Ruby (issue #67).
		if pagesGem.Match(gemfile) {
			return jekyllSite{Ruby: defaultRubyVersion, Repository: githubRepository(contextDir), Config: configName}, true
		}
		return jekyllSite{OwnGemfile: true, Ruby: jekyllRuby(contextDir),
			Repository: githubRepository(contextDir), Config: configName}, true
	}

	marked := jekyllTheme.Match(config)
	for _, dir := range jekyllMarkers {
		if info, err := os.Stat(filepath.Join(contextDir, dir)); err == nil && info.IsDir() {
			marked = true
		}
	}
	if !marked {
		return jekyllSite{}, false
	}
	return jekyllSite{Ruby: defaultRubyVersion, Repository: githubRepository(contextDir), Config: configName}, true
}

var (
	originSection = regexp.MustCompile(`(?m)^\s*\[remote "origin"\]\s*$`)
	gitURL        = regexp.MustCompile(`^\s*url\s*=\s*(\S+)\s*$`)
	githubRemote  = regexp.MustCompile(`^(?:https://|ssh://git@|git@)github\.com[/:]([A-Za-z0-9-]+/[A-Za-z0-9._-]+?)(?:\.git)?/?$`)
)

// githubRepository is the owner/name of the GitHub repository the checkout was
// cloned from, or empty when it was not cloned from GitHub.
//
// jekyll-github-metadata, which GitHub Pages turns on for every site, needs it
// the moment a layout reads site.github — the Pages themes all do — and in a
// production build reads it only from PAGES_REPO_NWO or the site's own
// config, never from the git remote. GitHub sets PAGES_REPO_NWO for the sites
// it builds; without it the build stopped at "No repo name found" (issue #67).
// Read when planning, so the plan shows the value it will build with.
func githubRepository(contextDir string) string {
	body, err := os.ReadFile(filepath.Join(contextDir, ".git", "config"))
	if err != nil {
		return ""
	}
	loc := originSection.FindIndex(body)
	if loc == nil {
		return ""
	}
	for _, line := range strings.Split(string(body[loc[1]:]), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			break
		}
		if m := gitURL.FindStringSubmatch(line); m != nil {
			if repo := githubRemote.FindStringSubmatch(m[1]); repo != nil {
				return repo[1]
			}
			return ""
		}
	}
	return ""
}

// gemfileBuildsJekyll reports a Gemfile that brings in Jekyll, directly or
// through what it names.
//
// A starter site names its theme gem and nothing else — jekyll-theme-chirpy,
// which depends on Jekyll — and a theme's own repository brings Jekyll in
// through its gemspec. Read as naming Jekyll or not, both were Ruby apps
// (issue #67). A Gemfile.lock that locks Jekyll settles it either way.
func gemfileBuildsJekyll(contextDir string, gemfile []byte) bool {
	if jekyllGem.Match(gemfile) {
		return true
	}
	if lock, err := os.ReadFile(filepath.Join(contextDir, "Gemfile.lock")); err == nil && lockedGem.Match(lock) {
		return true
	}
	if !gemspecLine.Match(gemfile) {
		return false
	}
	specs, _ := filepath.Glob(filepath.Join(contextDir, "*.gemspec"))
	for _, spec := range specs {
		if body, err := os.ReadFile(spec); err == nil && gemspecDep.Match(body) {
			return true
		}
	}
	return false
}

// jekyllRuby is the Ruby a site with its own Gemfile is built on: what
// .ruby-version says, or what the Gemfile or its lockfile says, or the default.
func jekyllRuby(contextDir string) string {
	version := gemfileRubyVersion(contextDir)
	if body, err := os.ReadFile(filepath.Join(contextDir, ".ruby-version")); err == nil {
		version = strings.TrimPrefix(strings.TrimSpace(string(body)), "ruby-")
	}
	if version == "" || !safeVersion(version) {
		return defaultRubyVersion
	}
	return version
}

// writeJekyllPlan writes a plan that builds the site and serves what Jekyll
// writes.
//
// Into .pando-plan/, beside where a Railpack plan goes, so detection collects it
// and the build replays it as it would any other plan (R-020).
func writeJekyllPlan(contextDir string, site jekyllSite) (string, error) {
	// PAGES_DISABLE_NETWORK keeps jekyll-github-metadata off the GitHub API.
	// Unauthenticated, that is sixty requests an hour per address, and a build
	// that got an answer and one that did not wrote different pages from the
	// same commit.
	//
	// BUNDLE_WITHOUT leaves out what a site keeps for testing and working on
	// it. Pando builds the site and does not test it (R-011), and a test group
	// can need more than Ruby: choosealicense.com's pulls in rugged, whose
	// native extension needs CMake, and the build failed installing a gem the
	// site never uses (issue #67).
	env := "ENV JEKYLL_ENV=production PAGES_DISABLE_NETWORK=1 BUNDLE_WITHOUT=development:test"
	if site.Repository != "" {
		env += " PAGES_REPO_NWO=" + site.Repository
	}
	files := map[string]string{jekyllServedConfigPath: jekyllServedConfig}
	gemfile := ""
	if !site.OwnGemfile {
		files[jekyllGemfilePath] = jekyllGemfile
		// Replacing a github-pages Gemfile, and the lockfile resolved from it,
		// is what GitHub does; a site with neither loses nothing.
		gemfile = "RUN cp " + jekyllGemfilePath + " Gemfile && rm -f Gemfile.lock\n"
	}
	name := filepath.Join(planDir, "Dockerfile")
	// The destination is outside the source, so nothing Jekyll writes can be
	// read back as a page on the next build.
	content := fmt.Sprintf(`# A Jekyll site, built with Jekyll and served by nginx.
FROM ruby:%s AS build
WORKDIR /site
COPY . .
%s
%sRUN bundle install
RUN bundle exec jekyll build --config %s,%s --destination /tmp/_site

FROM %s
COPY --from=build /tmp/_site/ /usr/share/nginx/html/
RUN printf %s > /etc/nginx/conf.d/default.conf
EXPOSE 80
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec nginx -g 'daemon off;'"]
`, site.Ruby, env, gemfile, site.Config, jekyllServedConfigPath, staticServerImage, printfFormat(staticConfig))

	files[filepath.ToSlash(name)] = content
	for name, body := range files {
		if err := writeContextFile(contextDir, filepath.FromSlash(name), []byte(body)); err != nil {
			return "", errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
		}
	}
	return name, nil
}
