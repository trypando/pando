package buildkit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// pagesSite is the shape of the repository in issue #67: a GitHub Pages site
// with a remote theme, a layout, a hand-written index.html at the root and
// under solutions/, and pages written in Markdown with no front matter, which
// GitHub Pages renders anyway. No Gemfile.
//
// detect's TestR094_AJekyllSiteIsBuiltRatherThanServedAsCommitted runs the same
// shape through the auction with this planner.
func pagesSite(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"_config.yml":                           "remote_theme: pages-themes/midnight@v0.2.0\nplugins:\n  - jekyll-remote-theme\n",
		"_layouts/solution.html":                "---\nlayout: default\n---\n{{ content }}\n",
		"index.html":                            "---\nlayout: default\n---\n<a href=\"solutions\">Solutions</a>\n",
		"solutions/index.html":                  "<!doctype html><p>list</p>\n",
		"solutions/copy-role-and-acls/index.md": "# Copy role and ACLs\n",
		"assets/js/get-solutions.js":            "console.log(1)\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	return root
}

// TestR094_AJekyllSiteIsBuiltBeforeItIsServed asserts R-094.
//
// A known static site generator's config is the static tier's evidence, and
// for Jekyll that evidence means a build. Served as committed, the site in
// issue #67 answered every Markdown page with a 403 or the Markdown itself.
func TestR094_AJekyllSiteIsBuiltBeforeItIsServed(t *testing.T) {
	root := pagesSite(t)

	plan, err := New().Plan(context.Background(), view{root})
	require.NoError(t, err)

	body := plan.Files[plan.Entry]
	require.Contains(t, body, "FROM ruby:"+defaultRubyVersion+" AS build")
	require.Contains(t, body,
		"RUN bundle exec jekyll build --config _config.yml,"+jekyllServedConfigPath+" --destination /tmp/_site")
	require.Contains(t, body, "COPY --from=build /tmp/_site/ /usr/share/nginx/html/")
	require.Contains(t, body, "EXPOSE 80")

	// With no Gemfile of its own the site is built the way GitHub Pages builds
	// it, and the Gemfile that says so travels with the plan.
	//
	// At ./Gemfile, because Jekyll loads the github-pages gem and the defaults
	// Pages turns on only from there. Pointed at with BUNDLE_GEMFILE, a page
	// with no front matter was copied out as Markdown.
	require.Contains(t, body, "RUN cp "+jekyllGemfilePath+" Gemfile && rm -f Gemfile.lock\nRUN bundle install")
	require.NotContains(t, body, "BUNDLE_GEMFILE")
	require.Equal(t, jekyllGemfile, plan.Files[jekyllGemfilePath])

	// Served at the root of the app's address, not at a guess at where GitHub
	// would serve it, and built the same way whether or not the GitHub API
	// answered.
	require.Equal(t, jekyllServedConfig, plan.Files[jekyllServedConfigPath])
	require.Contains(t, jekyllServedConfig, "baseurl: \"\"")
	require.Contains(t, body, "PAGES_DISABLE_NETWORK=1")
	require.Contains(t, body, "BUNDLE_WITHOUT=development:test", "a site's test gems are not installed (R-011)")
}

// A site with its own Gemfile is built with it, on the Ruby it names.
func TestR094_AJekyllSitesOwnGemfileAndRubyAreUsed(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"_config.yml":   "title: Blog\n",
		"Gemfile":       "source 'https://rubygems.org'\ngem 'jekyll', '~> 4.3'\n",
		".ruby-version": "ruby-3.2.4\n",
	})
	site, ok := readJekyllSite(root)
	require.True(t, ok)
	require.Equal(t, jekyllSite{OwnGemfile: true, Ruby: "3.2.4", Config: "_config.yml"}, site)

	name, err := writeJekyllPlan(root, site)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "FROM ruby:3.2.4 AS build")
	require.NotContains(t, string(body), "RUN cp ", "the site's own Gemfile is not replaced")
	_, err = os.Stat(filepath.Join(root, filepath.FromSlash(jekyllGemfilePath)))
	require.True(t, os.IsNotExist(err), "a site with a Gemfile gets no second one")
}

// What is not a Jekyll site is left to the planners that were reading it.
func TestAJekyllSiteIsNotReadIntoWhatIsNotOne(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"a _config.yml alone": {"_config.yml": "title: x\n", "index.html": "<p>hi</p>"},
		"a Ruby app":          {"_config.yml": "theme: x\n", "Gemfile": "gem 'rails'\n"},
		"no config":           {"_layouts/default.html": "{{ content }}"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for n, body := range files {
				full := filepath.Join(root, filepath.FromSlash(n))
				require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
				require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
			}
			_, ok := readJekyllSite(root)
			require.False(t, ok)
		})
	}
}

// A .ruby-version that is not a version is not put in a FROM line.
func TestAJekyllSitesRubyVersionIsCheckedBeforeItIsUsed(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"_config.yml":   "",
		"Gemfile":       "gem \"jekyll\"\n",
		".ruby-version": "3.3 AS x\nRUN evil\n",
	})
	site, ok := readJekyllSite(root)
	require.True(t, ok)
	require.Equal(t, defaultRubyVersion, site.Ruby)
	require.False(t, strings.Contains(site.Ruby, " "))
}

// TestR160_AStaticSitesDirectoryRedirectIsRelative asserts R-160.
//
// Behind a port-mode address, nginx's absolute redirect to add a directory's
// trailing slash dropped the port: /solutions on localhost:9001 went to
// http://localhost/solutions/ (issue #67). The integration test
// TestR160_AStaticSitesDirectoryRedirectKeepsItsPort runs the real server.
func TestR160_AStaticSitesDirectoryRedirectIsRelative(t *testing.T) {
	require.Contains(t, staticConfig, "absolute_redirect off;")
}

// A plan that cannot be written is a build failure that says so, not a plan
// with a piece missing.
func TestAJekyllPlanThatCannotBeWrittenFailsTheBuild(t *testing.T) {
	for name, tc := range map[string]struct {
		site  jekyllSite
		block string // made a file, where the plan needs a directory
		dir   string // made a directory, where the plan writes a file
	}{
		"gemfile directory": {site: jekyllSite{Ruby: "3.3.6"}, block: ".pando-plan/jekyll"},
		"gemfile":           {site: jekyllSite{Ruby: "3.3.6"}, dir: jekyllGemfilePath},
		"dockerfile folder": {site: jekyllSite{OwnGemfile: true, Ruby: "3.3.6"}, block: ".pando-plan"},
		"dockerfile":        {site: jekyllSite{OwnGemfile: true, Ruby: "3.3.6"}, dir: ".pando-plan/Dockerfile"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if tc.block != "" {
				full := filepath.Join(root, filepath.FromSlash(tc.block))
				require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
				require.NoError(t, os.WriteFile(full, nil, 0o644))
			}
			if tc.dir != "" {
				require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.FromSlash(tc.dir)), 0o755))
			}
			_, err := writeJekyllPlan(root, tc.site)
			require.Equal(t, errs.BuildFailed, errs.CodeOf(err), "got %v", err)
		})
	}
}

// The repository a site was cloned from reaches the plan, because the Pages
// themes read site.github and jekyll-github-metadata stops a production build
// that cannot name it (issue #67).
func TestR094_AJekyllSiteIsBuiltAsTheRepositoryItCameFrom(t *testing.T) {
	root := pagesSite(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte(
		"[core]\n\tbare = false\n[remote \"origin\"]\n\turl = https://github.com/ben-meeker/servicenow-solutions\n"+
			"\tfetch = +refs/heads/*:refs/remotes/origin/*\n"), 0o644))

	plan, err := New().Plan(context.Background(), view{root})
	require.NoError(t, err)
	require.Contains(t, plan.Files[plan.Entry], " PAGES_REPO_NWO=ben-meeker/servicenow-solutions\n")
}

func TestAGitHubRepositoryIsReadFromTheOriginAndNothingElse(t *testing.T) {
	for config, want := range map[string]string{
		"[remote \"origin\"]\n\turl = https://github.com/acme/site.git\n":                    "acme/site",
		"[remote \"origin\"]\n\turl = git@github.com:acme/site.github.io.git\n":              "acme/site.github.io",
		"[remote \"origin\"]\n\turl = ssh://git@github.com/acme/site\n":                      "acme/site",
		"[remote \"origin\"]\n\turl = https://gitlab.com/acme/site.git\n":                    "",
		"[remote \"origin\"]\n\turl = https://github.com/acme/site;rm -rf /\n":               "",
		"[remote \"upstream\"]\n\turl = https://github.com/acme/site\n":                      "",
		"[remote \"origin\"]\n\tfetch = x\n[remote \"b\"]\n\turl = https://github.com/a/b\n": "",
		"[core]\n\tbare = false\n": "",
	} {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte(config), 0o644))
		require.Equal(t, want, githubRepository(root), config)
	}
	require.Empty(t, githubRepository(t.TempDir()), "an uploaded source has no .git")
}

// A site's Gemfile brings Jekyll in however it likes: by name, through the
// theme gem a starter names, through a theme repository's gemspec, or as
// locked. Each was a Ruby app before (issue #67).
func TestR094_AJekyllSiteIsRecognizedByWhatItsGemfileBringsIn(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"jekyll":    {"Gemfile": "gem 'jekyll', '~> 4.3'\n"},
		"theme gem": {"Gemfile": "source \"https://rubygems.org\"\ngem \"jekyll-theme-chirpy\", \"~> 7.6\"\n"},
		"gemspec":   {"Gemfile": "gemspec\n", "theme.gemspec": "spec.add_runtime_dependency \"jekyll\", \">= 3.9\"\n"},
		"lockfile":  {"Gemfile": "gem 'my-site-deps'\n", "Gemfile.lock": "GEM\n  specs:\n    jekyll (4.3.4)\n"},
	} {
		t.Run(name, func(t *testing.T) {
			files["_config.yml"] = "title: x\n"
			site, ok := readJekyllSite(writeFiles(t, files))
			require.True(t, ok)
			require.True(t, site.OwnGemfile, "built with the site's own Gemfile")
		})
	}

	_, ok := readJekyllSite(writeFiles(t, map[string]string{
		"_config.yml": "", "Gemfile": "gemspec\n", "app.gemspec": "spec.add_dependency 'rack'\n",
	}))
	require.False(t, ok, "a gemspec that does not depend on Jekyll is a Ruby library's")
}

// A Gemfile naming github-pages is built the way GitHub builds it: with the
// current github-pages gem, not with what the Gemfile resolves to.
func TestR094_APagesSitesGemfileIsReplacedAsGitHubReplacesIt(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"_config.yml":   "remote_theme: mmistakes/minimal-mistakes\n",
		"Gemfile":       "gem \"github-pages\", group: :jekyll_plugins\ngem \"jekyll-algolia\"\n",
		"Gemfile.lock":  "GEM\n  specs:\n    github-pages (222)\n",
		".ruby-version": "2.7.1\n",
	})
	site, ok := readJekyllSite(root)
	require.True(t, ok)
	require.False(t, site.OwnGemfile)
	require.Equal(t, defaultRubyVersion, site.Ruby)

	name, err := writeJekyllPlan(root, site)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "RUN cp "+jekyllGemfilePath+" Gemfile && rm -f Gemfile.lock\n")
}
