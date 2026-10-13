package detect_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/builder/buildkit"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

// detectOnDisk runs the auction over files written to disk, with the real
// builder as the planner.
func detectOnDisk(t *testing.T, files map[string]string) detect.Result {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	result, err := detect.NewAuction(
		detect.DockerfileDetector{},
		detect.ComposeDetector{},
		detect.StaticDetector{},
		detect.BuildpackDetector{Planner: buildkit.New()},
		detect.MonorepoDetector{},
	).Run(context.Background(), (&source.Checkout{Dir: root}).View(""))
	require.NoError(t, err)
	return result
}

// TestR094_AGeneratorsSiteIsBuiltAndServedWithoutAQuestion asserts R-094's
// static tier: a known generator's config is evidence enough, with or without
// a manifest beside it. Hugo and Zola sites found "no recognizable project
// layout", and a Hugo site whose package.json held only Cypress was asked for
// a start command (issue #90).
func TestR094_AGeneratorsSiteIsBuiltAndServedWithoutAQuestion(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"hugo, no manifest": {
			"hugo.toml": "baseURL = '/'\ntitle = 'Field notes'\n", "content/_index.md": "# Hi\n",
			"layouts/index.html": "{{ .Title }}",
		},
		"hugo beside a test-only package.json": {
			"config.yaml": "baseURL: https://example.org/\ntheme: basic\n", "themes/basic/layouts/index.html": "",
			"package.json": `{"devDependencies":{"cypress":"^13"}}`, "package-lock.json": "{}",
		},
		"zola": {
			"config.toml": "base_url = \"/\"\ntitle = \"Workshop\"\n", "templates/index.html": "", "content/_index.md": "",
		},
		"mkdocs": {
			"mkdocs.yml": "site_name: Runbook\n", "requirements.txt": "mkdocs==1.6.1\n", "docs/index.md": "# Runbook\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := detectOnDisk(t, files)
			require.Nil(t, result.Blocked)
			require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
			require.Empty(t, detect.Asked(result.Questions))
			build := result.Winner.Draft.Build
			require.Contains(t, build.GeneratedFiles[build.Dockerfile], "COPY --from=build /tmp/site/")
			require.Equal(t, 80, result.Winner.Draft.Workloads[0].Ports[0].Number)
		})
	}
}

// TestR021_ASiteMissingASubmoduleSaysSo asserts R-021: Pando does not fetch
// submodules, and a Hugo site whose theme is one is refused with what to do,
// rather than built into empty pages or met with a question nobody can answer
// usefully.
func TestR021_ASiteMissingASubmoduleSaysSo(t *testing.T) {
	result := detectOnDisk(t, map[string]string{
		"config.toml":       "theme = \"beautifulhugo\"\nbaseURL = \"/\"\n",
		"content/_index.md": "",
		".gitmodules":       "[submodule \"themes/beautifulhugo\"]\n\tpath = themes/beautifulhugo\n\turl = https://example.com/t.git\n",
	})
	require.Equal(t, detect.StatusBlocked, result.Status)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(result.Blocked))
	require.Contains(t, result.Blocked.Error(), "submodule")
}

// TestR094_ACommittedSiteIsFoundWhereBuildsWriteIt asserts R-094's static
// tier for build/ and Cordova's www/, which were asked how to build (issue
// #90).
func TestR094_ACommittedSiteIsFoundWhereBuildsWriteIt(t *testing.T) {
	for _, dir := range []string{"build", "www"} {
		t.Run(dir, func(t *testing.T) {
			bid, err := detect.StaticDetector{}.Bid(context.Background(), memSource{
				dir + "/index.html": "<h1>hi</h1>", "README.md": "",
			})
			require.NoError(t, err)
			require.Equal(t, dir, bid.Draft.Build.StaticDir)
			require.InDelta(t, 0.7, bid.Confidence, 0.001)
		})
	}
}

// TestR094_AnHTMLTemplateIsNotABuiltSite asserts R-094. Create React App keeps
// the template its build fills in public/index.html; read as committed output,
// it put a serve-or-build question in front of a project that only builds one
// way (issue #90).
func TestR094_AnHTMLTemplateIsNotABuiltSite(t *testing.T) {
	result := detectOnDisk(t, map[string]string{
		"package.json":      `{"scripts":{"build":"react-scripts build","start":"react-scripts start"},"dependencies":{"react-scripts":"5.0.1"}}`,
		"package-lock.json": "{}",
		"public/index.html": `<link rel="icon" href="%PUBLIC_URL%/favicon.ico" />`,
		"src/index.js":      "",
	})
	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
	require.Empty(t, detect.Asked(result.Questions))
	build := result.Winner.Draft.Build
	require.Contains(t, build.GeneratedFiles[build.Dockerfile], "COPY --from=build /app/build/")
}

// TestR094_AJVMBuildBesideAPackageJSONIsAJavaApp asserts R-094's manifest
// tier: a Maven or Gradle build that runs the front end's npm build through
// its plugins is the app's build. JHipster was built as a Node app (issue #90).
func TestR094_AJVMBuildBesideAPackageJSONIsAJavaApp(t *testing.T) {
	for _, build := range []string{"pom.xml", "build.gradle"} {
		t.Run(build, func(t *testing.T) {
			bid, err := detect.BuildpackDetector{}.Bid(context.Background(), memSource{
				build:          "<project/>",
				"package.json": `{"scripts":{"build":"npm run webapp:prod","start":"npm run app:start"}}`,
			})
			require.NoError(t, err)
			require.Contains(t, bid.Evidence[0], build+" — this looks like a Java project")
			require.Equal(t, 8080, bid.Draft.Workloads[0].Ports[0].Number)
		})
	}
}

// TestR132_AMeteorAppAsksForItsMongoDB asserts R-132: a Meteor server stops at
// start without MONGO_URL, and Pando has no MongoDB to supply, so the deploy
// waits for one rather than starting something that exits.
func TestR132_AMeteorAppAsksForItsMongoDB(t *testing.T) {
	result := detectOnDisk(t, map[string]string{
		".meteor/release":   "METEOR@3.5.2\n",
		"package.json":      `{"scripts":{"start":"meteor run"},"dependencies":{"meteor-node-stubs":"^1"}}`,
		"package-lock.json": "{}",
		"server/main.js":    "",
	})
	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
	require.Equal(t, 3000, result.Winner.Draft.Workloads[0].Ports[0].Number)

	var mongo *spec.Slot
	for i, s := range result.Winner.Draft.Slots {
		if s.Key == "MONGO_URL" {
			mongo = &result.Winner.Draft.Slots[i]
		}
	}
	require.NotNil(t, mongo)
	require.True(t, mongo.Required)
	require.Contains(t, mongo.Evidence[0], "Valid answer")
}
