package detect_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/builder/buildkit"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// checkoutOf writes files into a checkout on disk, for the real planner.
func checkoutOf(t *testing.T, files map[string]string) api.SourceView {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	return (&source.Checkout{Dir: root}).View("")
}

// TestR103_LanguagesWithNoDetectorAreReadWithoutAQuestion asserts R-103:
// repositories in Crystal, R, Clojure, Scala, Haskell and Perl reached the
// generic "no recognizable project layout" question, whose answers were all
// refused (issue #90). Through the auction with the real builder as planner,
// each is a buildpack app with a plan and a start command, and nobody is
// asked anything. Railpack is not on PATH: none of these asks it.
func TestR103_LanguagesWithNoDetectorAreReadWithoutAQuestion(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for name, files := range map[string]map[string]string{
		"Crystal with a public/ directory": {
			"shard.yml":   "name: blog\ntargets:\n  blog:\n    main: src/blog.cr\ndependencies:\n  kemal:\n    github: kemalcr/kemal\n",
			"src/blog.cr": "require \"kemal\"\n", "public/app.css": "",
		},
		"an R plumber API":        {"plumber.R": "#* @get /tail\nfunction() { 1 }\n"},
		"an R Shiny app":          {"app.R": "library(shiny)\nshinyApp(fluidPage(), function(input, output) {})\n"},
		"a Leiningen project":     {"project.clj": "(defproject x \"0.1.0\"\n  :main x.core)\n"},
		"a Clojure CLI project":   {"deps.edn": "{:aliases {:run {:main-opts [\"-m\" \"x.main\"]}}}\n"},
		"an sbt project":          {"build.sbt": "", "project/plugins.sbt": `addSbtPlugin("com.github.sbt" % "sbt-native-packager" % "1.11.1")`},
		"a Stack project":         {"stack.yaml": "resolver: lts-18.3\n", "package.yaml": "executables:\n  server:\n    main: Main.hs\n"},
		"a Perl app":              {"cpanfile": "requires 'Dancer2';\n", "app.psgi": "use Dancer2;\n"},
		"PHP with no composer":    {"public/index.php": "<?php echo 1;"},
		"a setup.py-only package": {"setup.py": "setup(entry_points={'console_scripts': ['hello=hello.cli:main']})\n"},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := detect.NewAuction(
				detect.StaticDetector{},
				detect.BuildpackDetector{Planner: buildkit.New()},
			).Run(context.Background(), checkoutOf(t, files))
			require.NoError(t, err)
			require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
			require.Empty(t, detect.Asked(result.Questions), "nothing is asked")
			build := result.Winner.Draft.Build
			require.Contains(t, build.GeneratedFiles, build.Dockerfile, "the plan is in the proposal")
		})
	}
}

// A setup.py package whose start Pando cannot read is asked about, rather
// than going unrecognized: the plan installs it, and the start is a question.
func TestR105_ASetupPyPackageWithNoScriptAsksForItsStart(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	result, err := detect.NewAuction(detect.BuildpackDetector{Planner: buildkit.New()}).
		Run(context.Background(), checkoutOf(t, map[string]string{"setup.py": "setup(name='x')\n", "x/__init__.py": ""}))
	require.NoError(t, err)
	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
	asked := detect.Asked(result.Questions)
	require.Len(t, asked, 1)
	require.Equal(t, detect.KeyStartCommand, asked[0].Key)
	require.NoError(t, asked[0].Validate())
	require.Contains(t, asked[0].Prompt, "Python")
}

// TestR132_APlayAppNeedsItsSecretBeforeItCanStart asserts R-132: a Play app in
// production refuses to start without its application secret, which the
// person deploying owns, so it is a required value rather than an invention.
func TestR132_APlayAppNeedsItsSecretBeforeItCanStart(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	result, err := detect.NewAuction(detect.BuildpackDetector{Planner: buildkit.New()}).
		Run(context.Background(), checkoutOf(t, map[string]string{
			"build.sbt":           "enablePlugins(PlayScala)\n",
			"project/plugins.sbt": `addSbtPlugin("org.playframework" % "sbt-plugin" % "3.0.12")`,
		}))
	require.NoError(t, err)
	draft := result.Winner.Draft
	var slot *spec.Slot
	for i := range draft.Slots {
		if draft.Slots[i].Key == "APPLICATION_SECRET" {
			slot = &draft.Slots[i]
		}
	}
	require.NotNil(t, slot)
	require.True(t, slot.Required)
	require.Contains(t, slot.Evidence[0], "Valid answer")
	require.Equal(t, 9000, draft.Workloads[0].Ports[0].Number, "Play's port, from the plan")

	// The length the evidence names is one the slot's own check accepts: it
	// said 32 while random_long refuses anything under 64.
	m := regexp.MustCompile(`at least (\d+) characters`).FindStringSubmatch(slot.Evidence[0])
	require.NotNil(t, m)
	n, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	require.NoError(t, slot.Format.Check(slot.Key, strings.Repeat("a", n)))
	require.Error(t, slot.Format.Check(slot.Key, strings.Repeat("a", n-1)))
}

// TestR020_TheProposalCarriesThePlanItsBuildSteered asserts R-020: when
// detection's build steers the plan to make it build, the proposal — and so
// the spec a person accepts — carries the plan that was built, not the one
// that failed.
func TestR020_TheProposalCarriesThePlanItsBuildSteered(t *testing.T) {
	t.Parallel()
	steered := map[string]string{
		".pando-plan/railpack-plan.json": `{"deploy":{"startCommand":"gunicorn app:app"},"steps":[{"name":"x"}]}`,
		".pando-plan/steering.txt":       "rust-version:1.90.0: why\n",
	}
	job := &detect.Job{Auction: detect.NewAuction(detect.BuildpackDetector{Planner: plannerReportingStart{}}), Runtime: &recordingTrial{}}
	ctx := detect.WithBuild(context.Background(), func(ctx context.Context, partial *detect.Proposal) (detect.BuildOutcome, error) {
		require.False(t, detect.Steers(ctx), "steering is asked for by whoever builds, not assumed")
		require.True(t, detect.Steers(detect.WithSteer(ctx)))
		return detect.BuildOutcome{Built: &detect.Built{Inputs: "in", Source: "abc", Image: builtImage}, Plan: steered}, nil
	})

	p, err := job.Run(ctx, "app_1", spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"},
		memSource{"requirements.txt": "flask\n", "app.py": ""})
	require.NoError(t, err)
	require.Equal(t, steered, p.DraftSpec.Build.GeneratedFiles)
	require.Equal(t, steered, p.Winner.Draft.Build.GeneratedFiles)
}
