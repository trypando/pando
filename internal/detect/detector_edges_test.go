package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// When the checkout cannot list its Go files, whether the module is a library
// is unknown, and an unknown is not a refusal: the repository is read as the
// app it most likely is.
func TestAGoModuleWhoseFilesCannotBeListedIsNotCalledALibrary(t *testing.T) {
	src := faultySource{
		memSource: memSource{"go.mod": "module example.com/lib\n", "lib.go": "package lib\n"},
		globFails: map[string]bool{"*.go": true},
	}
	result, err := detect.NewAuction(detect.BuildpackDetector{}).Run(context.Background(), src)
	require.NoError(t, err)
	require.NotEqual(t, detect.StatusBlocked, result.Status)
}

// TestR021_AGoFileThatNamesNoPackageIsNotAProgram asserts R-021. A file with no
// package clause, or one the checkout cannot open, says nothing about a main
// package, so a module made only of those has no program in it to run.
func TestR021_AGoFileThatNamesNoPackageIsNotAProgram(t *testing.T) {
	src := faultySource{
		memSource: memSource{
			"go.mod":        "module example.com/lib\n",
			"doc.go":        "// Nothing but a comment.\n",
			"unreadable.go": "package main\n",
		},
		openFails: map[string]bool{"unreadable.go": true},
	}
	result, err := detect.NewAuction(detect.BuildpackDetector{}).Run(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, detect.StatusBlocked, result.Status)
	require.Contains(t, result.Blocked.Error(), "Go library")
}

// TestR132_ARailsAppKeepsTheValuesItAlreadyDeclares asserts R-132. A Rails app
// whose .env.example already names RAILS_ENV and SECRET_KEY_BASE is not given a
// second of either: what the repository declares stands, and Pando adds only
// what is missing.
func TestR132_ARailsAppKeepsTheValuesItAlreadyDeclares(t *testing.T) {
	result, err := detect.NewAuction(detect.BuildpackDetector{}).Run(context.Background(), memSource{
		"Gemfile": "gem 'rails'\n", "config/application.rb": "", "bin/rails": "",
		".env.example": "RAILS_ENV=staging\nSECRET_KEY_BASE=\n",
	})
	require.NoError(t, err)

	count := map[string]int{}
	values := map[string]string{}
	for _, e := range result.Winner.Draft.Workloads[0].Env {
		count[e.Key]++
		if e.Value != nil {
			values[e.Key] = *e.Value
		}
	}
	require.Equal(t, 1, count["RAILS_ENV"])
	require.Equal(t, 1, count["SECRET_KEY_BASE"])
	require.Equal(t, "production", values["RACK_ENV"], "the one it did not declare is added")
	for _, s := range result.Winner.Draft.Slots {
		require.NotEqual(t, "SECRET_KEY_BASE", s.Key, "a declared variable is not asked for again")
	}
}

// A Dockerfile the checkout lists but cannot open still bids — it is at the
// root, which is what decides the reading — and asks about nothing it could
// not read. There are no build arguments to ask for in a file nobody saw.
func TestAnUnreadableDockerfileAsksAboutNoBuildArguments(t *testing.T) {
	src := faultySource{
		memSource: memSource{"Dockerfile": "FROM alpine\nARG TOKEN\nRUN test -n \"$TOKEN\" || exit 1\n"},
		openFails: map[string]bool{"Dockerfile": true},
	}
	result, err := detect.NewAuction(detect.DockerfileDetector{}).Run(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, spec.BuildDockerfile, result.Winner.Strategy)
	for _, q := range result.Questions {
		require.NotContains(t, q.Key, detect.BuildArgKeyPrefix)
	}
}

// TestR200_AnImageVolumeOnlyReachesThePrimaryWorkload asserts R-200. The
// image the trial ran is the primary's; two of its declared paths that share a
// base name get two volumes rather than one shared between them.
func TestR200_AnImageVolumeOnlyReachesThePrimaryWorkload(t *testing.T) {
	draft := detect.Draft{Workloads: []spec.Workload{
		{Name: "worker"},
		{Name: "web", Primary: true},
	}}
	out, _ := detect.ApplyTrial(draft, nil, detect.Trial{Ran: true, ImageVolumes: []string{"/data", "/srv/data"}})

	require.Empty(t, out.Workloads[0].Mounts, "the worker is not the image that was run")
	require.Len(t, out.Volumes, 2)
	require.Equal(t, "data", out.Volumes[0].ID)
	require.Equal(t, "data-2", out.Volumes[1].ID)
	require.ElementsMatch(t, []spec.Mount{
		{VolumeID: "data", Path: "/data"},
		{VolumeID: "data-2", Path: "/srv/data"},
	}, out.Workloads[1].Mounts)
}

// TestR021_AnImageWithAWebPortBesideADatabasePortIsNotRefused asserts R-021 in
// the other direction: an image that also listens on HTTP has something for a
// browser, whatever else it serves.
func TestR021_AnImageWithAWebPortBesideADatabasePortIsNotRefused(t *testing.T) {
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: fixedTrial{ports: []int{6379, 8080}}}
	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "example/app:1"}, nil)
	require.NoError(t, err)
	require.Nil(t, p.Blocked)
	require.NotEqual(t, detect.StatusBlocked, p.Status)
	primary, ok := p.DraftSpec.PrimaryWorkload()
	require.True(t, ok)
	require.Equal(t, 8080, primary.Ports[0].Number, "the web port is the one routed to")
}

type publishedProbe struct{ images []detect.PublishedImage }

func (p publishedProbe) Published(context.Context, spec.Source) ([]detect.PublishedImage, error) {
	return p.images, nil
}

// TestR094_APublishedImageShortCircuitsTheAuction asserts R-094. An image the
// maintainer already publishes is proposed as it is, and the repository is not
// read to work out how to build it.
func TestR094_APublishedImageShortCircuitsTheAuction(t *testing.T) {
	job := &detect.Job{
		Auction:  detect.NewAuction(detect.DockerfileDetector{}),
		Registry: publishedProbe{images: []detect.PublishedImage{{Ref: "ghcr.io/acme/notes:latest", Registry: "ghcr.io"}}},
		Runtime:  fixedTrial{ports: []int{3000}},
	}
	p, err := job.Run(context.Background(), "app_1",
		spec.Source{Type: spec.SourceGit, URL: "https://github.com/acme/notes"},
		memSource{"Dockerfile": "FROM alpine\nEXPOSE 9999\n"})
	require.NoError(t, err)

	require.Equal(t, "registry", p.Winner.Detector)
	require.Equal(t, spec.BuildPrebuilt, p.Winner.Strategy)
	primary, ok := p.DraftSpec.PrimaryWorkload()
	require.True(t, ok)
	require.Equal(t, "ghcr.io/acme/notes:latest", primary.Image)
	require.Equal(t, 3000, primary.Ports[0].Number, "the port is the one watched, not the Dockerfile's")
	require.Empty(t, detect.Asked(p.Questions))
}

// An answer adopting a runner-up keeps only the answers to that runner-up's own
// questions. A port given for the winner's reading is not the adopted one's
// port, and a start command it never asked for is not applied to it either.
func TestAdoptingARunnerUpKeepsOnlyTheAnswersItAsked(t *testing.T) {
	runnerUp := spec.AppSpec{
		Workloads: []spec.Workload{{Name: "api", Primary: true, Exposed: true,
			Ports: []spec.Port{{Number: 8000, Protocol: "http"}}}},
		Build: spec.Build{Strategy: spec.BuildCompose},
	}
	p := proposalFor(primary("web"))
	p.RunnersUp = []detect.Candidate{{
		Strategy:  spec.BuildCompose,
		Draft:     detect.Draft{Workloads: runnerUp.Workloads},
		Spec:      &runnerUp,
		Questions: []detect.Question{{Key: detect.KeyPrimaryService}},
	}}

	got := p.WithAnswers(map[string]string{
		detect.KeyBuildStrategy:  string(spec.BuildCompose),
		detect.KeyPrimaryPort:    "80",
		detect.KeyStartCommand:   "npm start",
		detect.KeyPrimaryService: "api",
	})
	require.Equal(t, "api", got.Workloads[0].Name)
	require.True(t, got.Workloads[0].Primary)
	require.Equal(t, 8000, got.Workloads[0].Ports[0].Number, "the winner's port answer is not stamped on it")
	require.Empty(t, got.Workloads[0].Command)
}

// A build argument answered again replaces the value already in the spec
// rather than sitting beside it, and the others are left as they were.
func TestAnAnsweredBuildArgumentReplacesTheOneThere(t *testing.T) {
	p := proposalFor(primary("web"))
	p.DraftSpec.Build.Args = []spec.KV{{Key: "TOKEN", Value: "old"}, {Key: "REGION", Value: "eu"}}

	got := p.WithAnswers(map[string]string{detect.BuildArgKeyPrefix + "TOKEN": " new "})
	require.Equal(t, []spec.KV{{Key: "REGION", Value: "eu"}, {Key: "TOKEN", Value: "new"}}, got.Build.Args)
	require.Equal(t, "old", p.DraftSpec.Build.Args[0].Value, "the proposal itself is not changed")
}
