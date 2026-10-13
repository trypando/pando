package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// writeTree is writeFiles for files in subdirectories.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	return root
}

// planFor plans a repository the way a build with no answers does and returns
// the Dockerfile written.
func planFor(t *testing.T, files map[string]string) string {
	t.Helper()
	root := writeTree(t, files)
	name, err := buildpackPlan(api.BuildRequest{}, root)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	return string(body)
}

// TestR094_EachGeneratorsOutputDirectoryIsTheOneServed asserts R-094's static
// tier: a known generator is built, and what is served is where that
// generator writes.
//
// Read as Vite's dist/ for all of them, SvelteKit's adapter-static build
// succeeded and the image failed on `COPY /app/dist/: not found`; VitePress,
// Eleventy, Docusaurus and a generated Nuxt app were taken for servers with
// nothing to start (issue #90).
func TestR094_EachGeneratorsOutputDirectoryIsTheOneServed(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		out   string
		build string
	}{
		"sveltekit adapter-static, pages named": {
			files: map[string]string{
				"package.json":     `{"scripts":{"dev":"vite dev","build":"vite build"},"devDependencies":{"@sveltejs/adapter-static":"^3","@sveltejs/kit":"^2","vite":"^6"}}`,
				"svelte.config.js": "import adapter from '@sveltejs/adapter-static';\nexport default { kit: { adapter: adapter({ pages: 'site', assets: 'site' }) } };\n",
			},
			out: "site",
		},
		"sveltekit adapter-static, defaults": {
			files: map[string]string{
				"package.json":     `{"scripts":{"build":"vite build"},"devDependencies":{"@sveltejs/adapter-static":"^3","@sveltejs/kit":"^2","vite":"^6"}}`,
				"svelte.config.js": "import adapter from '@sveltejs/adapter-static';\nexport default { kit: { adapter: adapter() } };\n",
			},
			out: "build",
		},
		"vitepress at the root": {
			files: map[string]string{"package.json": `{"scripts":{"dev":"vitepress","build":"vitepress build"},"devDependencies":{"vitepress":"^1"}}`},
			out:   ".vitepress/dist",
		},
		"vitepress in docs/ with its own outDir": {
			files: map[string]string{
				"package.json":               `{"scripts":{"build":"vitepress build docs"},"devDependencies":{"vitepress":"^1","vite":"^5"}}`,
				"docs/.vitepress/config.mts": "export default { outDir: '../public-site' }\n",
			},
			out: "public-site",
		},
		"docusaurus, whose start is the dev server": {
			files: map[string]string{"package.json": `{"scripts":{"start":"docusaurus start","build":"docusaurus build","serve":"docusaurus serve"},"dependencies":{"@docusaurus/core":"3.10.2"}}`},
			out:   "build",
		},
		"eleventy with its output in the config": {
			files: map[string]string{
				"package.json":       `{"scripts":{"build":"eleventy","serve":"eleventy --serve"},"devDependencies":{"@11ty/eleventy":"^3"}}`,
				"eleventy.config.js": "export const config = { dir: { input: \"src\", output: \"public\" } };\n",
			},
			out: "public",
		},
		"eleventy started as its own server, output as a flag": {
			files: map[string]string{"package.json": `{"scripts":{"build":"npx @11ty/eleventy --output=www","start":"npx @11ty/eleventy --serve"},"devDependencies":{"@11ty/eleventy":"^3"}}`},
			out:   "www",
		},
		"eleventy by default": {
			files: map[string]string{"package.json": `{"scripts":{"build":"eleventy"},"devDependencies":{"@11ty/eleventy":"^3"}}`},
			out:   "_site",
		},
		"hexo, beside a _config.yml": {
			files: map[string]string{
				"package.json": `{"scripts":{"build":"hexo generate","server":"hexo server"},"dependencies":{"hexo":"^7"}}`,
				"_config.yml":  "title: Hexo\ntheme: landscape\npublic_dir: out\n",
			},
			out: "out",
		},
		"nuxt generated, by its config": {
			files: map[string]string{
				"package.json":   `{"scripts":{"build":"nuxt build","generate":"nuxt generate"},"dependencies":{"nuxt":"^3"}}`,
				"nuxt.config.ts": "export default defineNuxtConfig({ ssr: true, nitro: { prerender: { routes: ['/'] } } })\n",
			},
			out: ".output/public", build: "npm run generate",
		},
		"vite with its own outDir": {
			files: map[string]string{
				"package.json":   `{"scripts":{"build":"vite build","server":"vite preview"},"devDependencies":{"vite":"^6"}}`,
				"vite.config.js": "export default { build: { outDir: 'build' } }\n",
			},
			out: "build",
		},
		"astro with its own outDir": {
			files: map[string]string{
				"package.json":     `{"scripts":{"build":"astro build"},"dependencies":{"astro":"^5"}}`,
				"astro.config.mjs": "export default defineConfig({ outDir: './site' })\n",
			},
			out: "site",
		},
		"gatsby served after a banner, through another script": {
			files: map[string]string{"package.json": `{"scripts":{"build":"node banner.js; gatsby build","start":"node banner.js; npm run serve","serve":"gatsby serve -p ${PORT:-8080}"},"dependencies":{"gatsby":"^5"}}`},
			out:   "public",
		},
		"create react app served by serve, options first": {
			files: map[string]string{"package.json": `{"scripts":{"build":"react-scripts build && mv build _static","start":"node banner.js; serve -s _static -l tcp://0.0.0.0:$PORT -n"},"dependencies":{"react-scripts":"5","serve":"^14"}}`},
			out:   "_static",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			site, ok := readStaticSiteBuild(writeTree(t, tc.files))
			require.True(t, ok)
			require.Equal(t, tc.out, site.OutDir)
			require.Equal(t, tc.build, site.Build)

			body := planFor(t, tc.files)
			require.Contains(t, body, "COPY --from=build /app/"+tc.out+"/ /usr/share/nginx/html/")
		})
	}
}

// TestR094_AFrameworkThatBuildsAServerIsStartedAsOne asserts R-094: the same
// build command makes a server when the project says so, and that server is
// started the way the framework documents.
//
// A SvelteKit app on adapter-node, planned as a Vite site, built its server
// into build/ and then failed copying a dist/ that was never written; one on
// adapter-auto has no server of its own and SvelteKit's template previews it
// (issue #90).
func TestR094_AFrameworkThatBuildsAServerIsStartedAsOne(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		start string
	}{
		"sveltekit adapter-node": {
			files: map[string]string{
				"package.json":     `{"scripts":{"dev":"vite dev","build":"vite build","preview":"vite preview"},"devDependencies":{"@sveltejs/adapter-node":"^5","@sveltejs/kit":"^2","vite":"^6"}}`,
				"svelte.config.js": "import adapter from '@sveltejs/adapter-node';\nexport default { kit: { adapter: adapter() } };\n",
			},
			start: `CMD ["node build"]`,
		},
		"sveltekit adapter-node writing elsewhere": {
			files: map[string]string{
				"package.json":     `{"scripts":{"build":"vite build"},"devDependencies":{"@sveltejs/adapter-node":"^5","@sveltejs/kit":"^2","vite":"^6"}}`,
				"svelte.config.js": "import adapter from '@sveltejs/adapter-node';\nexport default { kit: { adapter: adapter({ out: 'server' }) } };\n",
			},
			start: `CMD ["node server"]`,
		},
		"sveltekit adapter-auto": {
			files: map[string]string{"package.json": `{"scripts":{"build":"vite build","preview":"vite preview"},"devDependencies":{"@sveltejs/adapter-auto":"^3","@sveltejs/kit":"^2","vite":"^6"}}`},
			start: `CMD ["npx vite preview --host 0.0.0.0 --port ${PORT:-4173}"]`,
		},
		"nuxt rendered on request": {
			files: map[string]string{
				"package.json":   `{"scripts":{"build":"nuxt build","dev":"nuxt dev","generate":"nuxt generate"},"dependencies":{"nuxt":"^3"}}`,
				"nuxt.config.ts": "export default defineNuxtConfig({})\n",
			},
			start: `CMD ["node .output/server/index.mjs"]`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, static := readStaticSiteBuild(writeTree(t, tc.files))
			require.False(t, static)
			body := planFor(t, tc.files)
			require.Contains(t, body, "# A Node server")
			require.Contains(t, body, "RUN npm run build")
			require.Contains(t, body, tc.start)
		})
	}
}

// A package.json is not the project when another toolchain's build file sits
// beside it: a JHipster app's Maven or Gradle build runs its front end's npm
// build, and a Deno app's scripts run under Deno (issue #90).
func TestAPackageJSONBesideAnotherToolchainsBuildIsNotANodeProject(t *testing.T) {
	for name, other := range map[string]string{
		"maven": "pom.xml", "gradle": "build.gradle", "gradle kotlin": "build.gradle.kts", "deno": "deno.json",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"package.json": `{"scripts":{"build":"vite build","start":"npm run serve"},"devDependencies":{"vite":"^6"}}`,
				other:          "",
			})
			_, static := readStaticSiteBuild(root)
			require.False(t, static)
			_, server := readNodeServer(root, "")
			require.False(t, server)
		})
	}
}

// A page at the root, a build script, and no dependency that could bundle
// anything: the build edits the files in place, and they are the site. Render's
// static sample was asked for a start command (issue #90).
func TestR094_ASiteBuiltInPlaceIsServedFromTheRoot(t *testing.T) {
	files := map[string]string{
		"package.json": `{"scripts":{"build":"node compile.js"}}`,
		"index.html":   "<h1>{$AccountName}</h1>\n",
		"compile.js":   "",
	}
	body := planFor(t, files)
	require.Contains(t, body, "RUN npm run build && rm -rf .git .pando-plan node_modules")
	require.Contains(t, body, "COPY --from=build /app/ /usr/share/nginx/html/")

	// With a dependency, the root page is source for a bundler, not the site.
	files["package.json"] = `{"scripts":{"build":"webpack"},"devDependencies":{"webpack":"^5"}}`
	_, ok := readStaticSiteBuild(writeTree(t, files))
	require.False(t, ok)
}

// TestR095_ASiteIsBuiltOnDebianWithItsPackageManager asserts R-095's reason for
// Pando's own plans: the toolchain the project uses, at the version it uses.
//
// Gatsby's rendering engine needs glibc and failed on Alpine; a Bun project's
// site was installed with npm, which crashed on a tree npm never wrote
// (issue #90).
func TestR095_ASiteIsBuiltOnDebianWithItsPackageManager(t *testing.T) {
	body := planFor(t, map[string]string{
		"package.json":      `{"scripts":{"build":"gatsby build","start":"gatsby develop"},"dependencies":{"gatsby":"^5"}}`,
		"package-lock.json": "{}",
	})
	require.Contains(t, body, "FROM node:22-slim AS build")
	require.NotContains(t, body, "alpine AS build")

	body = planFor(t, map[string]string{
		"package.json": `{"scripts":{"build":"ng build","start":"ng serve"},"dependencies":{"@angular/core":"^20"}}`,
		"bun.lock":     "{}",
	})
	require.Contains(t, body, "RUN npm install -g bun && bun install --frozen-lockfile")
	require.Contains(t, body, "RUN bun run build")
}

// TestR095_PnpmIsTheVersionTheProjectUses asserts R-095.
//
// corepack fetched the newest pnpm for a project whose package.json names
// none. That pnpm refused a lockfile pnpm 8 wrote, and stopped on dependency
// build scripts nobody had approved (issue #90).
func TestR095_PnpmIsTheVersionTheProjectUses(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"lockfile 9.0": {
			files: map[string]string{"package.json": `{}`, "pnpm-lock.yaml": "lockfileVersion: '9.0'\n"},
			want:  "npm install -g pnpm@9 && pnpm install --frozen-lockfile",
		},
		"lockfile 6.0": {
			files: map[string]string{"package.json": `{}`, "pnpm-lock.yaml": "lockfileVersion: '6.0'\n\nsettings:\n"},
			want:  "npm install -g pnpm@8 && pnpm install --frozen-lockfile",
		},
		"lockfile 5.4": {
			files: map[string]string{"package.json": `{}`, "pnpm-lock.yaml": "lockfileVersion: 5.4\n"},
			want:  "npm install -g pnpm@7 && pnpm install --frozen-lockfile",
		},
		"lockfile 5.3": {
			files: map[string]string{"package.json": `{}`, "pnpm-lock.yaml": "lockfileVersion: 5.3\n"},
			want:  "npm install -g pnpm@6 && pnpm install --frozen-lockfile",
		},
		"packageManager outranks the lockfile": {
			files: map[string]string{"package.json": `{"packageManager":"pnpm@9.8.0+sha512.abc"}`, "pnpm-lock.yaml": "lockfileVersion: '6.0'\n"},
			want:  "npm install -g pnpm@9.8.0 && pnpm install --frozen-lockfile",
		},
		"pnpm 10 runs dependency build scripts": {
			files: map[string]string{"package.json": `{"packageManager":"pnpm@10.33.0"}`, "pnpm-lock.yaml": "lockfileVersion: '9.0'\n"},
			want:  "npm install -g pnpm@10.33.0 && pnpm install --frozen-lockfile --config.dangerously-allow-all-builds=true",
		},
		"a lockfile that says nothing": {
			files: map[string]string{"package.json": `{}`, "pnpm-lock.yaml": ""},
			want:  "npm install -g pnpm@9 && pnpm install --frozen-lockfile",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, installCommand(writeTree(t, tc.files)))
		})
	}
}

// A start script that runs pnpx has pnpm in the image, whatever installed the
// dependencies: TanStack's examples exited 127 with "pnpx: not found"
// (issue #90).
func TestR095_AStartScriptThatRunsPnpmHasPnpm(t *testing.T) {
	server, ok := readNodeServer(writeTree(t, map[string]string{
		"package.json": `{"scripts":{"build":"vite build","start":"pnpx srvx --prod -s ../client dist/server/server.js"},"dependencies":{"@tanstack/react-start":"^1","vite":"^7"}}`,
	}), "")
	require.True(t, ok)
	require.Equal(t, "npm install -g pnpm@9 && npm install", server.Install)

	server, ok = readNodeServer(writeTree(t, map[string]string{
		"package.json": `{"scripts":{"start":"node server.js"}}`,
	}), "")
	require.True(t, ok)
	require.Equal(t, "npm install", server.Install)
}

// TestR095_ANodeRangeGetsTheLineItAllows asserts R-095.
//
// ">= 7.6" was read as Node 7 — by Railpack's mise, which installed the oldest
// release the range allowed, and `npm install` failed on every dependency
// written since (issue #90). A range gets the default line when it allows it,
// and otherwise the newest line it allows.
func TestR095_ANodeRangeGetsTheLineItAllows(t *testing.T) {
	for spec, want := range map[string]string{
		">= 7.6":     "22",
		">=18.0":     "22",
		">=20.11.1":  "22",
		"^20.11":     "20",
		"18.x":       "18",
		"^18 || ^20": "20",
		">=20 <21":   "20",
		">=23":       "24",
		">=30":       "30",
		"16 - 18":    "18",
		"*":          "22",
	} {
		require.Equal(t, want, nodeMajorFor(spec), spec)
	}

	root := writeTree(t, map[string]string{"package.json": `{"engines":{"node":">= 7.6"}}`})
	require.Equal(t, []string{"RAILPACK_NODE_VERSION=22"}, railpackDefaults(root))

	// A single major, and a version file, are Railpack's to read as they are.
	for _, files := range []map[string]string{
		{"package.json": `{"engines":{"node":"20.x"}}`},
		{"package.json": `{"engines":{"node":">=18"}}`, ".nvmrc": "20\n"},
	} {
		require.Empty(t, railpackDefaults(writeTree(t, files)))
	}
}

// TestR094_HugoZolaAndMkDocsSitesAreBuiltWithTheirGenerator asserts R-094's
// static tier for generators that are not Node packages. With no manifest, a
// Hugo or Zola site was "no recognizable project layout"; MkDocs'
// requirements.txt made it a Python app on port 8000 (issue #90).
func TestR094_HugoZolaAndMkDocsSitesAreBuiltWithTheirGenerator(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  []string
	}{
		"hugo": {
			files: map[string]string{"hugo.toml": "baseURL = 'https://example.org/'\ntitle = 'Notes'\n", "content/_index.md": ""},
			want: []string{"ENV HUGO_VERSION=" + hugoDefault, "hugo_extended_${HUGO_VERSION}_linux-${arch}.tar.gz",
				"RUN hugo --baseURL / --destination /tmp/site"},
		},
		"hugo with a config.toml and the version Netlify pins": {
			files: map[string]string{
				"config.yaml":                     "baseURL: https://example.netlify.app/\ntheme: basic\n",
				"themes/basic/layouts/index.html": "",
				"netlify.toml":                    "[build]\n  command = \"hugo\"\n[build.environment]\n  HUGO_VERSION = \"0.146.5\"\n",
				"package.json":                    `{"devDependencies":{"cypress":"^13"}}`,
			},
			want: []string{"ENV HUGO_VERSION=0.146.5"},
		},
		"an old hugo, whose archives were named by platform": {
			files: map[string]string{
				"config.toml":                 "baseurl = \"/\"\n",
				"content/post.md":             "",
				".github/workflows/pages.yml": "env:\n  HUGO_VERSION: 0.92.0\n",
			},
			want: []string{"ENV HUGO_VERSION=0.92.0", "hugo_extended_${HUGO_VERSION}_Linux-${arch}.tar.gz", "arch=64bit"},
		},
		"zola": {
			files: map[string]string{"config.toml": "base_url = \"/\"\ntitle = \"Workshop\"\n", "templates/index.html": ""},
			want: []string{"ENV ZOLA_VERSION=" + zolaDefault, "zola-v${ZOLA_VERSION}-${arch}-unknown-linux-gnu.tar.gz",
				"RUN zola build --base-url / --output-dir /tmp/site"},
		},
		"mkdocs with its requirements": {
			files: map[string]string{"mkdocs.yml": "site_name: Runbook\n", "requirements.txt": "mkdocs==1.6.1\n", "docs/index.md": ""},
			want:  []string{"FROM python:3.12-slim AS build", "RUN pip install --no-cache-dir -r requirements.txt", "RUN mkdocs build --site-dir /tmp/site"},
		},
		"mkdocs material with no requirements": {
			files: map[string]string{"mkdocs.yml": "site_name: Runbook\ntheme:\n  name: material\n", "docs/index.md": ""},
			want:  []string{"RUN pip install --no-cache-dir mkdocs mkdocs-material"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := planFor(t, tc.files)
			for _, want := range tc.want {
				require.Contains(t, body, want)
			}
			require.Contains(t, body, "COPY --from=build /tmp/site/ /usr/share/nginx/html/")
			require.Contains(t, body, "EXPOSE 80")
		})
	}
}

// An mkdocs.yml in a Python app's repository documents the app; the app is
// what is deployed.
func TestAnAppsDocumentationIsNotTheApp(t *testing.T) {
	_, ok, err := readSSGSite(writeTree(t, map[string]string{
		"mkdocs.yml": "site_name: Docs\n", "manage.py": "", "requirements.txt": "django\n",
	}))
	require.NoError(t, err)
	require.False(t, ok)

	// A config.toml is many tools' file; without Hugo's directories it is not
	// a Hugo site.
	_, ok, err = readSSGSite(writeTree(t, map[string]string{"config.toml": "baseURL = 'x'\n"}))
	require.NoError(t, err)
	require.False(t, ok)
}

// TestR021_AHugoThemeLeftAsASubmoduleIsNamed asserts R-021. Pando does not
// fetch submodules, so a theme kept as one is an empty directory in the
// checkout, and Hugo would build a site of empty pages. The site is refused
// with what to do instead.
func TestR021_AHugoThemeLeftAsASubmoduleIsNamed(t *testing.T) {
	root := writeTree(t, map[string]string{
		"config.toml":       "theme = \"beautifulhugo\"\nbaseURL = \"/\"\n",
		"content/_index.md": "",
		".gitmodules":       "[submodule \"themes/beautifulhugo\"]\n\tpath = themes/beautifulhugo\n\turl = https://github.com/halogenica/beautifulhugo.git\n",
	})
	require.NoError(t, os.MkdirAll(filepath.Join(root, "themes", "beautifulhugo"), 0o755))
	_, err := buildpackPlan(api.BuildRequest{}, root)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "beautifulhugo")
	require.Contains(t, err.Error(), "does not fetch submodules")

	// The same theme committed into the repository builds.
	require.NoError(t, os.WriteFile(filepath.Join(root, "themes", "beautifulhugo", "theme.toml"), nil, 0o644))
	_, err = buildpackPlan(api.BuildRequest{}, root)
	require.NoError(t, err)
}

// Hexo keeps a _config.yml naming a theme, which is what a Jekyll site looks
// like; built with Jekyll, it failed on a Gemfile it never had (issue #90).
func TestR094_AHexoSiteIsNotReadAsJekyll(t *testing.T) {
	body := planFor(t, map[string]string{
		"package.json":                 `{"scripts":{"build":"hexo generate"},"dependencies":{"hexo":"^7","hexo-theme-landscape":"^1"}}`,
		"_config.yml":                  "title: Hexo\ntheme: landscape\n",
		"source/_posts/hello-world.md": "",
	})
	require.NotContains(t, body, "jekyll")
	require.Contains(t, body, "COPY --from=build /app/public/ /usr/share/nginx/html/")
}

// TestR095_AMeteorAppIsBuiltIntoABundle asserts R-095: a Meteor app is built
// by the release it names into the bundle Meteor deploys, rather than started
// with `npm start`, which runs the meteor command a Node image does not have
// (issue #90).
func TestR095_AMeteorAppIsBuiltIntoABundle(t *testing.T) {
	body := planFor(t, map[string]string{
		".meteor/release":   "METEOR@3.5.2\n",
		"package.json":      `{"scripts":{"start":"meteor run --settings settings.json"},"dependencies":{"meteor-node-stubs":"^1"}}`,
		"package-lock.json": "{}",
		"settings.json":     `{"public":{}}`,
	})
	for _, want := range []string{
		`curl -fsSL "https://install.meteor.com/?release=3.5.2" | sh`,
		"RUN meteor npm ci",
		"RUN meteor build --server-only --directory /out",
		"COPY --from=build /app/settings.json /app/meteor-settings.json",
		"EXPOSE 3000",
		`export METEOR_SETTINGS=\"$(cat /app/meteor-settings.json)\"; exec node main.js`,
	} {
		require.Contains(t, body, want)
	}
}
