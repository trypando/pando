package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// A Containerfile is a Dockerfile by another name (issue #55).
func TestR094_AContainerfileIsBuiltLikeADockerfile(t *testing.T) {
	result, err := detect.NewAuction(detect.DockerfileDetector{}).
		Run(context.Background(), memSource{"Containerfile": "FROM alpine\nEXPOSE 8080\n"})
	require.NoError(t, err)
	require.Equal(t, spec.BuildDockerfile, result.Winner.Strategy)
	require.Equal(t, "Containerfile", result.Winner.Draft.Build.Dockerfile)
	require.Equal(t, 8080, result.Winner.Draft.Workloads[0].Ports[0].Number)
}

// The only Dockerfile in the repository is the one to build, wherever it is.
// Asking which of one file to use was answered, recorded, and then refused at
// accept because the answer produced no workload (issue #55).
func TestR021_TheOnlyDockerfileIsUsedWithoutAsking(t *testing.T) {
	result, err := detect.NewAuction(detect.DockerfileDetector{}).
		Run(context.Background(), memSource{"docker/Dockerfile": "FROM alpine\nEXPOSE 8000\n", "app/main.py": ""})
	require.NoError(t, err)
	require.Equal(t, "docker/Dockerfile", result.Winner.Draft.Build.Dockerfile)
	require.Empty(t, detect.Asked(result.Questions))
	require.Len(t, result.Winner.Draft.Workloads, 1)
}

// With several, Pando still asks — and the answer now produces a workload.
func TestR021_ChoosingAmongDockerfilesProducesSomethingToRun(t *testing.T) {
	src := memSource{"api/Dockerfile": "FROM alpine\n", "web/Dockerfile": "FROM alpine\n"}
	result, err := detect.NewAuction(detect.DockerfileDetector{}).Run(context.Background(), src)
	require.NoError(t, err)

	keys := map[string]bool{}
	for _, q := range detect.Asked(result.Questions) {
		keys[q.Key] = true
		require.NoError(t, q.Validate())
	}
	require.True(t, keys[detect.KeyDockerfilePath])
	require.True(t, keys[detect.KeyPrimaryPort])

	proposal := detect.Proposal{Winner: result.Winner, DraftSpec: detect.Assemble("app_1", spec.Source{}, result.Winner.Draft)}
	s := proposal.WithAnswers(map[string]string{detect.KeyDockerfilePath: "web/Dockerfile", detect.KeyPrimaryPort: "3000"})
	primary, ok := s.PrimaryWorkload()
	require.True(t, ok)
	require.Equal(t, "web/Dockerfile", s.Build.Dockerfile)
	require.Equal(t, 3000, primary.Ports[0].Number)
}

// A build argument the Dockerfile refuses to build without is asked for, and
// the answer reaches the build. The build failed on exactly that and nobody had
// been asked (issue #55). One with no such check is left alone (R-104).
func TestR104_ARequiredBuildArgumentIsAskedForAndPassedToTheBuild(t *testing.T) {
	const dockerfile = "FROM node:22-alpine\nARG GREETING\nARG VERSION\n" +
		"RUN if [ -z \"${GREETING}\" ]; then \\\n    echo required >&2; exit 1; \\\n  fi\nEXPOSE 8080\n"
	result, err := detect.NewAuction(detect.DockerfileDetector{}).
		Run(context.Background(), memSource{"Dockerfile": dockerfile})
	require.NoError(t, err)

	asked := detect.Asked(result.Questions)
	require.Len(t, asked, 1)
	require.Equal(t, detect.BuildArgKeyPrefix+"GREETING", asked[0].Key)
	require.NoError(t, asked[0].Validate())

	proposal := detect.Proposal{Winner: result.Winner, DraftSpec: detect.Assemble("app_1", spec.Source{}, result.Winner.Draft)}
	s := proposal.WithAnswers(map[string]string{detect.BuildArgKeyPrefix + "GREETING": "hello"})
	require.Equal(t, []spec.KV{{Key: "GREETING", Value: "hello"}}, s.Build.Args)
}

// An answer to a question the adopted reading never asked does not apply to
// it. A port given for the Dockerfile reading was stamped on the compose app
// the tie-break adopted, and traffic went to 80 while the app was on 8000
// (issue #55).
func TestR102_AnAnswerForAReadingThatWasNotChosenIsNotApplied(t *testing.T) {
	src := memSource{
		"Dockerfile": "FROM python:3.12\nCMD [\"uvicorn\", \"main:app\"]\n",
		"compose.yaml": "services:\n  api:\n    build: .\n    environment:\n      PORT: 8000\n" +
			"    ports: [\"8000:8000\"]\n",
	}
	result, err := detect.NewAuction(detect.DockerfileDetector{}, detect.ComposeDetector{}).Run(context.Background(), src)
	require.NoError(t, err)

	p := detect.Proposal{
		Winner: result.Winner, RunnersUp: result.RunnersUp,
		DraftSpec: detect.Assemble("app_1", spec.Source{}, result.Winner.Draft),
	}
	s := p.WithAnswers(map[string]string{
		detect.KeyBuildStrategy: string(spec.BuildCompose),
		detect.KeyPrimaryPort:   "80",
	})
	require.Equal(t, spec.BuildCompose, s.Build.Strategy)
	primary, ok := s.PrimaryWorkload()
	require.True(t, ok)
	require.Equal(t, 8000, primary.Ports[0].Number, "the compose file's port, not the Dockerfile reading's answer")
}

// An answer that cannot become a spec is refused when it is given, not at
// accept as "This app has no workloads" (issue #55).
func TestR105_AnAnswerThatCannotBecomeASpecIsRefusedWhenGiven(t *testing.T) {
	nothing := detect.Proposal{Winner: detect.Candidate{Strategy: detect.StrategyUnknown}}
	err := nothing.CheckAnswers(map[string]string{detect.KeyBuildMethod: "serve the root with php -S 0.0.0.0:8080"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nothing in this repository")

	two := detect.Proposal{
		Winner:    detect.Candidate{Strategy: spec.BuildDockerfile, Draft: detect.Draft{Workloads: []spec.Workload{{Name: "web"}}}},
		RunnersUp: []detect.Candidate{{Strategy: spec.BuildCompose, Draft: detect.Draft{Workloads: []spec.Workload{{Name: "api"}}}}},
	}
	require.NoError(t, two.CheckAnswers(map[string]string{detect.KeyBuildStrategy: "compose"}))
	err = two.CheckAnswers(map[string]string{detect.KeyBuildStrategy: "static"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "dockerfile, compose")
}

// A site kept in docs/ is served from there, the way GitHub Pages serves it.
func TestR110_ASiteInDocsIsServed(t *testing.T) {
	result, err := detect.NewAuction(detect.StaticDetector{}).
		Run(context.Background(), memSource{"docs/index.html": "<h1>hi</h1>", "README.md": ""})
	require.NoError(t, err)
	require.Equal(t, spec.BuildStatic, result.Winner.Strategy)
	require.Equal(t, "docs", result.Winner.Draft.Build.StaticDir)
}

// TestR094_AnEditorEnvironmentDockerfileIsNotADeploymentArtifact asserts
// R-094 tier 2. A `.codesandbox/Dockerfile` describes the machine somebody
// edits on; Astro's examples carry a one-line `FROM node` there, and Pando
// deployed it instead of building the site (issue #90).
func TestR094_AnEditorEnvironmentDockerfileIsNotADeploymentArtifact(t *testing.T) {
	for _, dir := range []string{".codesandbox", ".devcontainer", ".gitpod", ".idx"} {
		result, err := detect.NewAuction(detect.DockerfileDetector{}).
			Run(context.Background(), memSource{dir + "/Dockerfile": "FROM node:24\n", "package.json": "{}"})
		require.NoError(t, err)
		require.Equal(t, detect.StatusUnknown, result.Status, dir)
	}
}

// TestR094_AHelperToolsDockerfileRanksBelowTheApp asserts R-094 tier 2. A
// Dockerfile among helper tools builds a tool: it loses to the compose file
// that runs the app without a question, and to any other Dockerfile.
func TestR094_AHelperToolsDockerfileRanksBelowTheApp(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yaml":                     "services:\n  app:\n    image: example/app:2\n    ports: [\"3001:3001\"]\n",
		"extra/uptime-push/Dockerfile":     "FROM node\nCOPY build.js build.js\n",
		"extra/latest-warning/Dockerfile2": "FROM alpine\n",
	})
	require.NoError(t, err)
	require.Equal(t, spec.BuildPrebuilt, result.Winner.Draft.Build.Strategy)
	require.Equal(t, "compose", result.Winner.Detector)
	require.Empty(t, detect.Asked(result.Questions), "a helper tool is not a close second")

	result, err = detect.NewAuction(detect.DockerfileDetector{}).Run(context.Background(), memSource{
		"tools/push/Dockerfile": "FROM alpine\n",
		"docker/Dockerfile":     "FROM alpine\nEXPOSE 8080\n",
	})
	require.NoError(t, err)
	require.Equal(t, "docker/Dockerfile", result.Winner.Draft.Build.Dockerfile)
	require.Empty(t, detect.Asked(result.Questions))
}
