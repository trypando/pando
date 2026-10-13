package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0o644))
	}
	return root
}

// TestR110_ASiteThatBuildsToStaticFilesIsBuiltAndServed asserts R-110.
//
// An Astro site has a build and nothing to start. nixpacks refused it with "No
// start command could be found", and detection asked for a start command the
// site does not have (issue #55).
func TestR110_ASiteThatBuildsToStaticFilesIsBuiltAndServed(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"package.json":      `{"scripts":{"build":"astro build","dev":"astro dev"},"dependencies":{"astro":"^5"}}`,
		"package-lock.json": `{}`,
	})
	site, ok := readStaticSiteBuild(root)
	require.True(t, ok)
	require.Equal(t, "dist", site.OutDir)
	require.Equal(t, "npm ci", site.Install)

	name, err := writeStaticSitePlan(root, site)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "RUN npm run build")
	require.Contains(t, string(body), "COPY --from=build /app/dist/ /usr/share/nginx/html/")
	require.Contains(t, string(body), "EXPOSE 80")
}

// The shapes a built site comes in: a development server as its start script,
// a static file server over the output directory, Angular's own output path,
// and a Node major the repository pins.
func TestR110_BuiltSitesAreRecognizedInTheShapesTheyComeIn(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		out   string
		node  string
	}{
		"vite dev server as start": {
			files: map[string]string{"package.json": `{"scripts":{"build":"vite build","start":"vite"},"devDependencies":{"vite":"^8"}}`},
			out:   "dist", node: "22",
		},
		"svelte template served by sirv": {
			files: map[string]string{"package.json": `{"scripts":{"build":"rollup -c","start":"sirv public --no-clear"},"devDependencies":{"rollup":"^3"}}`},
			out:   "public", node: "22",
		},
		"angular application builder": {
			files: map[string]string{
				"package.json": `{"scripts":{"build":"ng build","start":"ng serve"},"dependencies":{"@angular/core":"^21"}}`,
				"angular.json": `{"projects":{"shop":{"architect":{"build":{"builder":"@angular/build:application","options":{"outputPath":"dist/shop"}}}}}}`,
			},
			out: "dist/shop/browser", node: "22",
		},
		"vite served by a static server over its own directory": {
			files: map[string]string{"package.json": `{"scripts":{"build":"vite build","start":"serve build/"},"devDependencies":{"vite":"^6"}}`, "yarn.lock": ""},
			out:   "build", node: "22",
		},
		"angular browser builder, output path as an object": {
			files: map[string]string{
				"package.json": `{"scripts":{"build":"ng build"},"dependencies":{"@angular/core":"^18"}}`,
				"angular.json": `{"defaultProject":"shop","projects":{"shop":{"architect":{"build":{"options":{"outputPath":{"base":"out/shop"}}}}}}}`,
			},
			out: "out/shop/browser", node: "22",
		},
		"angular output path naming its browser directory": {
			files: map[string]string{
				"package.json": `{"scripts":{"build":"ng build"},"dependencies":{"@angular/core":"^18"}}`,
				"angular.json": `{"projects":{"shop":{"architect":{"build":{"options":{"outputPath":{"base":"out","browser":""}}}}}}}`,
			},
			out: "out", node: "22",
		},
		"angular browser builder with no output path": {
			files: map[string]string{
				"package.json": `{"scripts":{"build":"ng build"},"dependencies":{"@angular/core":"^17"}}`,
				"angular.json": `{"projects":{"admin":{"architect":{"build":{"builder":"@angular-devkit/build-angular:browser"}}}}}`,
			},
			out: "dist/admin", node: "22",
		},
		"angular with no angular.json": {
			files: map[string]string{"package.json": `{"scripts":{"build":"ng build"},"dependencies":{"@angular/core":"^17"}}`},
			out:   "dist", node: "22",
		},
		"angular with an unreadable angular.json": {
			files: map[string]string{
				"package.json": `{"scripts":{"build":"ng build"},"dependencies":{"@angular/core":"^17"}}`,
				"angular.json": `{"projects":`,
			},
			out: "dist", node: "22",
		},
		// Render's Vue sample: no start script, only Vue CLI's development
		// server, and detection asked how to start it (issue #90).
		"vue cli": {
			files: map[string]string{
				"package.json": `{"scripts":{"serve":"vue-cli-service serve","build":"vue-cli-service build"},"devDependencies":{"@vue/cli-service":"~5.0.8"}}`,
				"yarn.lock":    "",
			},
			out: "dist", node: "22",
		},
		"vue cli with an outputDir": {
			files: map[string]string{
				"package.json":  `{"scripts":{"build":"vue-cli-service build"},"devDependencies":{"@vue/cli-service":"^5"}}`,
				"vue.config.js": "module.exports = {\n  outputDir: 'public_html',\n}\n",
			},
			out: "public_html", node: "22",
		},
		"gatsby with a pinned node": {
			files: map[string]string{
				"package.json": `{"scripts":{"build":"gatsby build","start":"gatsby develop"},"dependencies":{"gatsby":"^5"}}`,
				".nvmrc":       "v20.11.0\n",
			},
			out: "public", node: "20",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			site, ok := readStaticSiteBuild(writeFiles(t, tc.files))
			require.True(t, ok)
			require.Equal(t, tc.out, site.OutDir)
			require.Equal(t, tc.node, site.Node)
		})
	}
}

// A server-side app that builds its assets with Vite is the server, not a site.
// Laravel was served as files from a dist/ directory that was never written
// (issue #55).
func TestAServerAppThatBuildsItsAssetsWithViteIsNotAStaticSite(t *testing.T) {
	_, ok := readStaticSiteBuild(writeFiles(t, map[string]string{
		"package.json":  `{"scripts":{"build":"vite build","dev":"vite"},"devDependencies":{"vite":"^7"}}`,
		"composer.json": `{"require":{"laravel/framework":"^12"}}`,
	}))
	require.False(t, ok)
}

func TestAServedDirectoryCannotLeaveTheBuild(t *testing.T) {
	_, ok := readStaticSiteBuild(writeFiles(t, map[string]string{
		"package.json": `{"scripts":{"build":"x","start":"serve ../../etc"}}`,
	}))
	require.False(t, ok)

	// The same with a known framework, whose own output directory would
	// otherwise have been used.
	_, ok = readStaticSiteBuild(writeFiles(t, map[string]string{
		"package.json": `{"scripts":{"build":"vite build","start":"serve ../../etc"},"devDependencies":{"vite":"^6"}}`,
	}))
	require.False(t, ok)
}

// A repository with no package.json, or one that does not parse, is not a
// site that builds.
func TestAStaticSiteNeedsAPackageJsonThatParses(t *testing.T) {
	_, ok := readStaticSiteBuild(t.TempDir())
	require.False(t, ok)
	_, ok = readStaticSiteBuild(writeFiles(t, map[string]string{"package.json": `{"scripts":`}))
	require.False(t, ok)
}

// safeRelative is what keeps a served or built directory inside the build
// context and out of the shell.
func TestASafeRelativeDirectoryStaysInsideTheContext(t *testing.T) {
	for _, dir := range []string{"dist", "dist/", "out/site", "./build"} {
		require.True(t, safeRelative(dir), dir)
	}
	for _, dir := range []string{"", ".", "..", "../x", "/abs", "a b", "x;y", "$HOME", "a/../.."} {
		require.False(t, safeRelative(dir), dir)
	}
}

func TestASiteWithItsOwnServerIsNotTreatedAsStatic(t *testing.T) {
	for name, pkg := range map[string]string{
		"start script":        `{"scripts":{"build":"vite build","start":"node server.js"},"devDependencies":{"vite":"^6"}}`,
		"no build":            `{"scripts":{},"devDependencies":{"vite":"^6"}}`,
		"astro server output": `{"scripts":{"build":"astro build"},"dependencies":{"astro":"^5","@astrojs/node":"^9"}}`,
		"not a site builder":  `{"scripts":{"build":"tsc"},"dependencies":{"express":"^4"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := readStaticSiteBuild(writeFiles(t, map[string]string{"package.json": pkg}))
			require.False(t, ok)
		})
	}
}
