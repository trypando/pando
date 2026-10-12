package deploy

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

// barrierBuilder builds each service once every other has started, so builds
// run one after another never finish; a service named in fail fails at once.
type barrierBuilder struct {
	api.BuilderAdapter
	want int
	fail string

	mu      sync.Mutex
	started int
	all     chan struct{}
}

func (b *barrierBuilder) Category() api.Category { return api.CategoryBuilder }
func (b *barrierBuilder) Capabilities(context.Context) (api.BuilderCapabilities, error) {
	return api.BuilderCapabilities{}, nil
}

func (b *barrierBuilder) Build(ctx context.Context, req api.BuildRequest) (api.BuildResult, error) {
	service := req.CacheNamespace[strings.Index(req.CacheNamespace, "/")+1:]
	_, _ = io.WriteString(req.LogSink, "step 1 of "+service+"\npartial")
	if service == b.fail {
		return api.BuildResult{}, errs.New(errs.BuildFailed, "The build of "+service+" failed.")
	}
	b.mu.Lock()
	b.started++
	if b.started == b.want {
		close(b.all)
	}
	b.mu.Unlock()
	select {
	case <-b.all:
	case <-ctx.Done():
		return api.BuildResult{}, ctx.Err()
	case <-time.After(10 * time.Second):
		return api.BuildResult{}, errors.New("the builds ran one at a time")
	}
	return api.BuildResult{ImageRef: "pando/" + service + ":" + req.Tag}, nil
}

// namingRuntime imports nothing, so a build keeps the builder's reference.
type namingRuntime struct{ api.RuntimeAdapter }

func (namingRuntime) Category() api.Category { return api.CategoryRuntime }
func (namingRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return importing, nil
}
func (namingRuntime) HasImage(context.Context, string) (bool, error) { return true, nil }

func (namingRuntime) ImportImage(_ context.Context, in io.Reader) (string, error) {
	_, err := io.Copy(io.Discard, in)
	return "", err
}

func composeApp() *spec.AppSpec {
	return &spec.AppSpec{
		AppID:   "app_01HQ8",
		Build:   spec.Build{AdapterRef: "bld", Strategy: spec.BuildCompose},
		Runtime: spec.RuntimeRef{AdapterRef: "rt"},
		Workloads: []spec.Workload{
			{Name: "web", Primary: true, Build: &spec.WorkloadBuild{Dockerfile: "web/Dockerfile"}},
			{Name: "worker", Build: &spec.WorkloadBuild{Dockerfile: "worker/Dockerfile"}},
			{Name: "db", Image: "postgres:16"},
		},
	}
}

func composeRunner(t *testing.T, b *barrierBuilder, limit int) *Runner {
	t.Helper()
	reg := api.NewRegistry()
	require.NoError(t, reg.Register("bld", b))
	require.NoError(t, reg.Register("rt", namingRuntime{}))
	r := &Runner{registry: reg}
	return r.WithBuildConcurrency(func(context.Context) (int, error) { return limit, nil })
}

// TestR096_AComposeAppsServicesBuildAtOnce asserts that a compose app's
// services build concurrently, within the install's limit (R-311: detection
// builds each of them before the plan is shown, so a build that waits on
// another it does not need is time somebody spends watching). Each service's
// log lines carry its name.
func TestR096_AComposeAppsServicesBuildAtOnce(t *testing.T) {
	t.Parallel()
	b := &barrierBuilder{want: 2, all: make(chan struct{})}
	r := composeRunner(t, b, 2)

	var log strings.Builder
	image, images, err := r.BuildImages(context.Background(), composeApp(), &source.Checkout{}, &log, "detect-1")
	require.NoError(t, err)
	require.Empty(t, image)
	require.Equal(t, map[string]string{"web": "pando/web:detect-1", "worker": "pando/worker:detect-1"}, images)
	require.Contains(t, log.String(), "web | step 1 of web\n")
	require.Contains(t, log.String(), "worker | partial\n", "a last line with no newline is kept")
}

// TestR096_OneServiceFailingStopsTheOthers asserts that the first build to
// fail is the error, and the builds beside it are canceled rather than left
// running for a deploy that has already failed.
func TestR096_OneServiceFailingStopsTheOthers(t *testing.T) {
	t.Parallel()
	b := &barrierBuilder{want: 2, fail: "worker", all: make(chan struct{})}
	r := composeRunner(t, b, 2)

	_, _, err := r.buildAll(context.Background(), composeApp(), &source.Checkout{}, io.Discard, "dep_1")
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	require.Contains(t, err.Error(), "worker")
}

func TestABuildLimitComesFromTheInstall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	require.Equal(t, 1, (&Runner{}).buildLimit(ctx), "one at a time when nothing says otherwise")
	require.Equal(t, 3, (&Runner{}).WithBuildConcurrency(func(context.Context) (int, error) { return 3, nil }).buildLimit(ctx))
	require.Equal(t, DefaultConcurrency(), (&Runner{}).WithBuildConcurrency(func(context.Context) (int, error) {
		return 0, errors.New("policy unreadable")
	}).buildLimit(ctx))
}

type policyDoc struct {
	doc policy.Document
	err error
}

func (p policyDoc) Document(context.Context) (policy.Document, error) { return p.doc, p.err }

func TestABuildLimitIsTheInstallsDeployLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := (&Runner{}).WithBuildConcurrency(DeployConcurrency(policyDoc{doc: policy.Document{MaxConcurrentDeploys: 4}}))
	require.Equal(t, 4, r.buildLimit(ctx))
	r.WithBuildConcurrency(DeployConcurrency(policyDoc{err: errors.New("unreadable")}))
	require.Equal(t, DefaultConcurrency(), r.buildLimit(ctx))
}

func TestADigestIsReadFromAnyImageName(t *testing.T) {
	t.Parallel()
	require.Equal(t, "sha256:abc", digestOf("registry.example/app@sha256:abc"))
	require.Equal(t, "sha256:abc", digestOf("sha256:abc"))
}

type storedBuild struct {
	built *detect.Built
	err   error
}

func (s storedBuild) Built(context.Context, string) (*detect.Built, error) { return s.built, s.err }

// TestR311_ADeployRunsDetectionsBuildOnlyWhenItIsWhatItWouldBuild asserts the
// rule behind building once (R-311): the image detection built is run only for
// the same build inputs from the same source, and a compose app is reused
// whole or not at all.
func TestR311_ADeployRunsDetectionsBuildOnlyWhenItIsWhatItWouldBuild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	app := buildApp()
	checkout := &source.Checkout{Commit: "abc"}
	match := &detect.Built{Inputs: spec.BuildInputs(app), Source: "abc", Image: "sha256:1"}

	r := &Runner{}
	_, ok := r.detectionBuild(ctx, app, checkout)
	require.False(t, ok, "nothing to ask")

	r.WithDetectionBuilds(storedBuild{built: match})
	got, ok := r.detectionBuild(ctx, app, checkout)
	require.True(t, ok)
	require.Equal(t, "sha256:1", got.Image)

	_, ok = r.detectionBuild(ctx, app, &source.Checkout{Commit: "def"})
	require.False(t, ok, "another commit")

	r.WithDetectionBuilds(storedBuild{err: errors.New("unreadable")})
	_, ok = r.detectionBuild(ctx, app, checkout)
	require.False(t, ok, "an unreadable detection builds")

	r.WithDetectionBuilds(storedBuild{built: &detect.Built{Inputs: match.Inputs, Source: "abc"}})
	_, ok = r.detectionBuild(ctx, app, checkout)
	require.False(t, ok, "a build with no image")

	compose := composeApp()
	half := &detect.Built{Inputs: spec.BuildInputs(compose), Source: "abc", Images: map[string]string{"web": "sha256:w"}}
	r.WithDetectionBuilds(storedBuild{built: half})
	_, ok = r.detectionBuild(ctx, compose, checkout)
	require.False(t, ok, "a compose app is not half reused")

	half.Images["worker"] = "sha256:k"
	_, ok = r.detectionBuild(ctx, compose, checkout)
	require.True(t, ok)
}
