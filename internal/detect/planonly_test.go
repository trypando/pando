package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// TestR103_ARepositoryThePlannerRecognizesIsNotAskedHowToBuild asserts R-103.
//
// A Deno module, a mix.exs, a .csproj or a bare main.py has none of the eight
// manifests the buildpack detector reads, and each got "Pando could not work
// out how to build and run this app" although the planner, asked, had a plan
// (issue #55).
func TestR103_ARepositoryThePlannerRecognizesIsNotAskedHowToBuild(t *testing.T) {
	result, err := detect.NewAuction(detect.BuildpackDetector{Planner: plannerWritingCMD{}}).
		Run(context.Background(), memSource{"main.py": "print('hi')\n"})
	require.NoError(t, err)

	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
	require.Empty(t, detect.Asked(result.Questions))
	require.NotEmpty(t, result.Winner.Draft.Build.GeneratedFiles)
}

// Without a plan that says how the app starts there is nothing to bid on, and
// the repository gets the honest question rather than a buildpack guess.
func TestAnUnrecognizedRepositoryIsStillAsked(t *testing.T) {
	for name, planner := range map[string]detect.BuildPlanner{
		"no planner":       nil,
		"planner fails":    failingPlanner{},
		"no start command": plannerWithoutCMD{},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := detect.NewAuction(detect.BuildpackDetector{Planner: planner}).
				Run(context.Background(), memSource{"README.md": "# notes\n"})
			require.NoError(t, err)
			require.NotEqual(t, spec.BuildBuildpack, result.Winner.Strategy)
		})
	}
}

// A plan that declares its port is believed over the language's usual one: a
// site built to static files and served by nginx is on 80, not Node's 3000.
func TestR097_APlansDeclaredPortBeatsTheLanguageDefault(t *testing.T) {
	result, err := detect.NewAuction(detect.BuildpackDetector{Planner: plannerExposing80{}}).
		Run(context.Background(), memSource{"package.json": `{"scripts":{"build":"astro build"}}`})
	require.NoError(t, err)
	require.Equal(t,
		[]spec.Port{{Number: 80, Protocol: "http", Source: spec.PortExpose}},
		result.Winner.Draft.Workloads[0].Ports)
}

// A buildpack image starts through its own login shell, which takes the command
// line as one argument. `sh -c <line>` given to it ran a bare `sh` that exited
// at once (issue #55).
func TestR104_AnAnsweredStartCommandRunsThroughTheBuildpackImagesShell(t *testing.T) {
	result, err := detect.NewAuction(detect.BuildpackDetector{}).
		Run(context.Background(), memSource{"requirements.txt": "flask\n"})
	require.NoError(t, err)

	proposal := detect.Proposal{Winner: result.Winner, DraftSpec: detect.Assemble("app_1", spec.Source{}, result.Winner.Draft)}
	s := proposal.WithAnswers(map[string]string{detect.KeyStartCommand: "python app.py --port 8000"})
	primary, ok := s.PrimaryWorkload()
	require.True(t, ok)
	require.Equal(t, []string{"python app.py --port 8000"}, primary.Command)
}

// An app created from a published image is proposed as that image, not sent
// through repository detection and asked how to build it (issue #55).
func TestR094_APublishedImageAppIsProposedAsTheImage(t *testing.T) {
	job := &detect.Job{Auction: detect.NewAuction(detect.BuildpackDetector{})}
	p, err := job.Run(context.Background(), "app_1",
		spec.Source{Type: spec.SourceImage, Image: "ghost:5-alpine"}, memSource{})
	require.NoError(t, err)

	require.Equal(t, spec.BuildPrebuilt, p.Winner.Strategy)
	primary, ok := p.DraftSpec.PrimaryWorkload()
	require.True(t, ok)
	require.Equal(t, "ghost:5-alpine", primary.Image)

	// No runtime to watch it start here, so the port is the one question left.
	asked := detect.Asked(p.Questions)
	require.Len(t, asked, 1)
	require.Equal(t, detect.KeyPrimaryPort, asked[0].Key)
	require.NoError(t, asked[0].Validate())
}

type failingPlanner struct{}

func (failingPlanner) Plan(context.Context, api.SourceView) (api.BuildPlan, error) {
	return api.BuildPlan{}, context.Canceled
}

type plannerWithoutCMD struct{}

func (plannerWithoutCMD) Plan(context.Context, api.SourceView) (api.BuildPlan, error) {
	return api.BuildPlan{Files: map[string]string{".pando-plan/Dockerfile": "FROM ubuntu:noble\nRUN true\n"}, Entry: ".pando-plan/Dockerfile"}, nil
}

type plannerExposing80 struct{}

func (plannerExposing80) Plan(context.Context, api.SourceView) (api.BuildPlan, error) {
	return api.BuildPlan{Files: map[string]string{".pando-plan/Dockerfile": "FROM nginx:1.27-alpine\nEXPOSE 80\nCMD [\"nginx\", \"-g\", \"daemon off;\"]\n"}, Entry: ".pando-plan/Dockerfile"}, nil
}
