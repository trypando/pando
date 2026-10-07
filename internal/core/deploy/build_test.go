package deploy

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// recordingBuilder is a builder that keeps the request it was given and
// streams a few bytes of "image" into the sink, the way a real one does.
type recordingBuilder struct {
	api.BuilderAdapter
	asked  api.BuildRequest
	result api.BuildResult
	err    error
	pushes bool
}

func (b *recordingBuilder) Category() api.Category { return api.CategoryBuilder }

func (b *recordingBuilder) Capabilities(context.Context) (api.BuilderCapabilities, error) {
	return api.BuilderCapabilities{SupportsPush: b.pushes}, nil
}

func (b *recordingBuilder) Build(_ context.Context, req api.BuildRequest) (api.BuildResult, error) {
	b.asked = req
	if b.err != nil {
		return api.BuildResult{}, b.err
	}
	if req.Push != nil {
		return b.result, nil
	}
	_, _ = io.WriteString(req.ImageSink, "image-bytes")
	return b.result, nil
}

// importing is a runtime that takes a built image directly, as single-host
// Docker does.
var importing = api.RuntimeCapabilities{ImageDelivery: []api.ImageDelivery{api.ImageDeliveryImport}}

// importingRuntime can import an image and records what it read.
type importingRuntime struct {
	api.RuntimeAdapter
	caps     api.RuntimeCapabilities
	capsErr  error
	id       string
	imported string
}

func (r *importingRuntime) Category() api.Category { return api.CategoryRuntime }

func (r *importingRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return r.caps, r.capsErr
}

func (r *importingRuntime) ImportImage(_ context.Context, in io.Reader) (string, error) {
	b, err := io.ReadAll(in)
	r.imported = string(b)
	return r.id, err
}

func buildRunner(t *testing.T, b *recordingBuilder, rt *importingRuntime) *Runner {
	t.Helper()
	reg := api.NewRegistry()
	require.NoError(t, reg.Register("bld", b))
	require.NoError(t, reg.Register("rt", rt))
	return &Runner{registry: reg}
}

func buildApp() *spec.AppSpec {
	return &spec.AppSpec{
		AppID: "app_01HQ8",
		Build: spec.Build{
			AdapterRef: "bld",
			Strategy:   spec.BuildBuildpack,
			Dockerfile: "Dockerfile.app",
			Context:    "app",
			Target:     "prod",
			StaticDir:  "dist",
			Args:       []spec.KV{{Key: "NODE_ENV", Value: "production"}, {Key: "SHARED", Value: "app"}},
		},
		Runtime: spec.RuntimeRef{AdapterRef: "rt"},
		Workloads: []spec.Workload{{
			Name: "web", Primary: true,
			Command: []string{"sh", "-c", "gunicorn app:app --bind 0.0.0.0:$PORT"},
		}},
	}
}

// The app-wide build is the spec's own strategy, file, context and target, and
// carries the start command a person answered at detection. Without it the
// builder plans again and the answer never reaches the image.
func TestTheAppWideBuildIsAskedForWhatTheSpecSays(t *testing.T) {
	b := &recordingBuilder{result: api.BuildResult{ImageRef: "pando/app:latest"}}
	rt := &importingRuntime{caps: importing, id: "sha256:imported"}
	r := buildRunner(t, b, rt)

	var log strings.Builder
	image, err := r.build(context.Background(), buildApp(), &source.Checkout{}, &log, nil, "app_01HQ8", "dep_1")
	require.NoError(t, err)
	require.Equal(t, "sha256:imported", image, "what the runtime imported is what runs")
	require.Equal(t, "image-bytes", rt.imported, "the built image went through to the runtime")
	require.Contains(t, log.String(), "=> Building\n")

	got := b.asked
	require.Equal(t, spec.BuildBuildpack, got.Strategy)
	require.Equal(t, "Dockerfile.app", got.Dockerfile)
	require.Equal(t, "app", got.Context)
	require.Equal(t, "prod", got.Target)
	require.Equal(t, "dist", got.StaticDir)
	require.Equal(t, "gunicorn app:app --bind 0.0.0.0:$PORT", got.StartCommand)
	require.Equal(t, map[string]string{"NODE_ENV": "production", "SHARED": "app"}, got.Args)
	require.Equal(t, "app_01HQ8", got.CacheNamespace)
}

// TestR096_AComposeServiceBuildsFromItsOwnDockerfile asserts R-096.
//
// One service of a compose app is an ordinary Dockerfile build with its own
// context, file and target, whatever the app-wide strategy says. Its own build
// arguments win over the app's, and it gets no start command: its Dockerfile
// says its own.
func TestR096_AComposeServiceBuildsFromItsOwnDockerfile(t *testing.T) {
	b := &recordingBuilder{result: api.BuildResult{ImageRef: "pando/app-api:latest"}}
	rt := &importingRuntime{caps: importing}
	r := buildRunner(t, b, rt)

	wb := &spec.WorkloadBuild{
		Context: "api", Dockerfile: "api/Dockerfile", Target: "runtime",
		Args: []spec.KV{{Key: "SHARED", Value: "service"}},
	}
	var log strings.Builder
	image, err := r.build(context.Background(), buildApp(), &source.Checkout{}, &log, wb, "app_01HQ8/api", "dep_1")
	require.NoError(t, err)
	require.Equal(t, "pando/app-api:latest", image,
		"a runtime that names nothing on import leaves the builder's reference")
	require.NotContains(t, log.String(), "=> Building\n", "the caller names the service instead")

	got := b.asked
	require.Equal(t, spec.BuildDockerfile, got.Strategy)
	require.Equal(t, "api/Dockerfile", got.Dockerfile)
	require.Equal(t, "api", got.Context)
	require.Equal(t, "runtime", got.Target)
	require.Empty(t, got.StartCommand)
	require.Equal(t, map[string]string{"NODE_ENV": "production", "SHARED": "service"}, got.Args)
	require.Equal(t, "app_01HQ8/api", got.CacheNamespace)
}

// TestR146_ABuildFailureIsReturnedAsIs asserts R-146.
//
// The deploy writes the builder's error into the log and leaves the running
// app alone; that only works if the error comes back unchanged.
func TestR146_ABuildFailureIsReturnedAsIs(t *testing.T) {
	cause := errs.New(errs.BuildFailed, "The build failed.")
	b := &recordingBuilder{err: cause}
	rt := &importingRuntime{caps: importing}
	r := buildRunner(t, b, rt)

	_, err := r.build(context.Background(), buildApp(), &source.Checkout{}, io.Discard, nil, "app_01HQ8", "dep_1")
	require.ErrorIs(t, err, cause)
}

// A runtime that cannot take a built image is refused before anything is
// built, with a way forward, rather than building an image nothing can run.
func TestABuildIsRefusedForARuntimeThatCannotImport(t *testing.T) {
	b := &recordingBuilder{}
	r := buildRunner(t, b, &importingRuntime{})

	_, err := r.build(context.Background(), buildApp(), &source.Checkout{}, io.Discard, nil, "app_01HQ8", "dep_1")
	e := errs.As(err)
	require.NotNil(t, e)
	require.Equal(t, errs.PlanCapabilityUnsupported, e.Code)
	require.NotEmpty(t, e.Remedy)
	require.Empty(t, b.asked.Strategy, "nothing was built")

	broken := buildRunner(t, &recordingBuilder{}, &importingRuntime{capsErr: errors.New("daemon down")})
	_, err = broken.build(context.Background(), buildApp(), &source.Checkout{}, io.Discard, nil, "app_01HQ8", "dep_1")
	require.EqualError(t, err, "daemon down")
}

// The start command a builder is told is a shell line a person would type,
// and only a buildpack build, which plans itself, is told one.
func TestTheStartCommandIsTheLineAPersonWouldType(t *testing.T) {
	app := func(cmd ...string) *spec.AppSpec {
		return &spec.AppSpec{
			Build:     spec.Build{Strategy: spec.BuildBuildpack},
			Workloads: []spec.Workload{{Name: "web", Primary: true, Command: cmd}},
		}
	}

	require.Equal(t, "npm start", startCommand(app("sh", "-c", "npm start"), nil),
		"a detection answer is unwrapped rather than quoted twice")
	require.Equal(t, "./server", startCommand(app("./server"), nil))
	require.Equal(t, "node server.js --port 3000", startCommand(app("node", "server.js", "--port", "3000"), nil))
	require.Empty(t, startCommand(app(), nil), "no command, nothing to say")

	require.Empty(t, startCommand(app("npm start"), &spec.WorkloadBuild{}),
		"a compose service's Dockerfile says its own")

	dockerfile := app("npm start")
	dockerfile.Build.Strategy = spec.BuildDockerfile
	require.Empty(t, startCommand(dockerfile, nil), "a Dockerfile says its own")

	noPrimary := &spec.AppSpec{
		Build:     spec.Build{Strategy: spec.BuildBuildpack},
		Workloads: []spec.Workload{{Name: "worker", Command: []string{"work"}}},
	}
	require.Empty(t, startCommand(noPrimary, nil))
}

// TestR097_APortNobodyFilledInIsReplacedByTheRoutedOne asserts R-097.
//
// A PORT name read out of .env.example with no value is not a choice, so the
// routed port fills it. Other variables are not mistaken for it.
func TestR097_APortNobodyFilledInIsReplacedByTheRoutedOne(t *testing.T) {
	empty := ""
	w := spec.Workload{
		Name: "web", Primary: true, Ports: []spec.Port{{Number: 8000}},
		Env: []spec.EnvEntry{
			{Key: "LOG_LEVEL", Value: &empty, Source: spec.EnvFromDetection},
			{Key: "PORT", Value: &empty, Source: spec.EnvFromDetection},
		},
	}
	port, ok := defaultPort(w)
	require.True(t, ok)
	require.Equal(t, "8000", port)

	w.Env[1].Source = spec.EnvFromUser
	_, ok = defaultPort(w)
	require.False(t, ok, "an empty PORT a person typed is kept")

	_, ok = defaultPort(spec.Workload{Name: "web", Primary: true})
	require.False(t, ok, "no port, nothing to tell it")
}
