package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

const builtImage = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

// dockerfileRepo is a repository a Dockerfile builds, with no EXPOSE: the port
// is the trial run's to find.
var dockerfileRepo = memSource{"Dockerfile": "FROM nginx:alpine\nCOPY . /usr/share/nginx/html\n"}

// TestR097_TheTrialRunStartsTheImageDetectionBuilt asserts R-097 for an app
// Pando builds: detection builds the image (R-311), and the trial run starts
// that image and watches it, so the port is observed rather than asked. Before
// detection built anything there was nothing to start, and the question went
// to the person.
func TestR097_TheTrialRunStartsTheImageDetectionBuilt(t *testing.T) {
	t.Parallel()
	rt := &recordingTrial{}
	job := &detect.Job{Auction: detect.NewAuction(detect.DockerfileDetector{}), Runtime: rt}

	var stages []string
	ctx := detect.WithProgress(context.Background(), func(stage string, _ *detect.Proposal) { stages = append(stages, stage) })
	auth := &api.RegistryAuth{Registry: "registry.pando.internal", Username: "pando"}
	ctx = detect.WithBuild(ctx, func(ctx context.Context, partial *detect.Proposal) (detect.BuildOutcome, error) {
		detect.Report(ctx, detect.StageBuilding, partial)
		return detect.BuildOutcome{Built: &detect.Built{Inputs: "in", Source: "abc", Image: builtImage}, PullAuth: auth}, nil
	})

	p, err := job.Run(ctx, "app_1", spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"}, dockerfileRepo)
	require.NoError(t, err)

	require.Equal(t, builtImage, rt.req.Image, "the trial ran what detection built")
	require.Equal(t, auth, rt.req.PullAuth)
	require.True(t, p.Trial.Ran && p.Trial.Started, "the trial ran and saw it start")
	require.NotNil(t, p.Built)
	require.Equal(t, builtImage, p.Built.Image)
	for _, w := range p.DraftSpec.Workloads {
		require.Empty(t, w.Image, "the spec says how the app is built; the image belongs to the build")
	}
	require.Equal(t, []string{detect.StageDetecting, detect.StageBuilding, detect.StageTrying}, stages)
}

// TestR107_ABuildThatFailsStopsDetection asserts that a build failing at
// detection stops it with the build's error, as a trial that crashes shows
// its log and stops (R-107): nothing is trial-run and nothing is proposed.
func TestR107_ABuildThatFailsStopsDetection(t *testing.T) {
	t.Parallel()
	rt := &recordingTrial{}
	job := &detect.Job{Auction: detect.NewAuction(detect.DockerfileDetector{}), Runtime: rt}
	ctx := detect.WithBuild(context.Background(), func(context.Context, *detect.Proposal) (detect.BuildOutcome, error) {
		return detect.BuildOutcome{}, errs.New(errs.BuildFailed, "The build failed.")
	})

	_, err := job.Run(ctx, "app_1", spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"}, dockerfileRepo)
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	require.Empty(t, rt.req.Image, "nothing was trial-run")
}

// A build that waits — nothing built — leaves the trial with nothing to run,
// as before.
func TestR311_NothingBuiltLeavesTheTrialNothingToRun(t *testing.T) {
	t.Parallel()
	rt := &recordingTrial{}
	job := &detect.Job{Auction: detect.NewAuction(detect.DockerfileDetector{}), Runtime: rt}
	ctx := detect.WithBuild(context.Background(), func(context.Context, *detect.Proposal) (detect.BuildOutcome, error) {
		return detect.BuildOutcome{}, nil
	})

	p, err := job.Run(ctx, "app_1", spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"}, dockerfileRepo)
	require.NoError(t, err)
	require.Nil(t, p.Built)
	require.Empty(t, rt.req.Image)
}

func TestR311_ABuildWaitsOnlyOnAnswersThatChangeWhatIsBuilt(t *testing.T) {
	t.Parallel()
	p := detect.Proposal{Questions: []detect.Question{
		{Key: detect.KeyPrimaryPort},
		{Key: detect.BuildArgKeyPrefix + "TOKEN"},
		{Key: "start_command", Deferred: true},
		{Key: detect.BuildArgKeyPrefix + "SUGGESTED", Suggested: &detect.Suggestion{Value: "x"}},
	}}
	require.Equal(t, []string{"build_arg.TOKEN"}, p.BuildWaitsOn(nil))
	require.Empty(t, p.BuildWaitsOn(map[string]string{"build_arg.TOKEN": "t"}))
}

func TestR311_WhatNeedsABuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		spec spec.AppSpec
		want bool
	}{
		{"dockerfile", spec.AppSpec{Build: spec.Build{Strategy: spec.BuildDockerfile}}, true},
		{"published image", spec.AppSpec{Build: spec.Build{Strategy: spec.BuildPrebuilt}}, false},
		{"image app", spec.AppSpec{Source: spec.Source{Type: spec.SourceImage}, Build: spec.Build{Strategy: spec.BuildDockerfile}}, false},
		{"no strategy", spec.AppSpec{}, false},
		{"compose with a build", spec.AppSpec{Build: spec.Build{Strategy: spec.BuildCompose},
			Workloads: []spec.Workload{{Name: "db", Image: "postgres"}, {Name: "web", Build: &spec.WorkloadBuild{}}}}, true},
		{"compose of images", spec.AppSpec{Build: spec.Build{Strategy: spec.BuildCompose},
			Workloads: []spec.Workload{{Name: "db", Image: "postgres"}}}, false},
	} {
		require.Equal(t, tc.want, detect.NeedsBuild(&tc.spec), tc.name)
	}
}

func TestR311_ABuildIsMatchedByItsInputsAndSource(t *testing.T) {
	t.Parallel()
	s := &spec.AppSpec{Build: spec.Build{Strategy: spec.BuildDockerfile, Dockerfile: "Dockerfile"},
		Workloads: []spec.Workload{{Name: "web", Primary: true}}}
	b := &detect.Built{Inputs: spec.BuildInputs(s), Source: "abc", Image: builtImage}

	require.True(t, b.Matches(s, "abc"))
	require.False(t, b.Matches(s, "def"), "another commit")
	require.False(t, b.Matches(s, ""), "no source to compare")
	changed := *s
	changed.Build.Target = "prod"
	require.False(t, b.Matches(&changed, "abc"), "another build")
	require.False(t, (*detect.Built)(nil).Matches(s, "abc"))

	require.Equal(t, builtImage, b.Scanned(s))
	require.Empty(t, (*detect.Built)(nil).Scanned(s))
	compose := &detect.Built{Images: map[string]string{"web": "reg/web@sha256:1", "worker": "reg/worker@sha256:2"}}
	require.Equal(t, "reg/web@sha256:1", compose.Scanned(s), "a compose app's primary service")
	require.Empty(t, compose.Scanned(nil))
	require.Empty(t, compose.Scanned(&spec.AppSpec{Workloads: []spec.Workload{{Name: "a"}, {Name: "b"}}}))
}
