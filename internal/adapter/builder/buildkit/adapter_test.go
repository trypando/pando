package buildkit

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// view is a source on disk, which is what Bid and Plan read.
type view struct{ root string }

func (v view) Root() string { return v.root }

func (v view) Open(name string) (io.ReadCloser, error) { return os.Open(filepath.Join(v.root, name)) }

func (v view) Stat(name string) (api.FileInfo, error) {
	info, err := os.Stat(filepath.Join(v.root, name))
	if err != nil {
		return api.FileInfo{}, err
	}
	return api.FileInfo{Name: info.Name(), Size: info.Size(), IsDir: info.IsDir()}, nil
}

func (v view) Glob(pattern string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(v.root, pattern))
	for i, m := range matches {
		matches[i], _ = filepath.Rel(v.root, m)
	}
	return matches, err
}

func TestIdentity(t *testing.T) {
	a := New()
	require.Equal(t, Kind, a.Kind())
	require.Equal(t, api.CategoryBuilder, a.Category())
}

// Dialing is deferred: BuildKit may still be starting when Pando does, and an
// adapter that refuses to configure would be dropped from the registry for the
// life of the process.
func TestConfigureSucceedsEvenWhenBuildKitIsNotUpYet(t *testing.T) {
	a := New()
	require.NoError(t, a.Configure(context.Background(),
		json.RawMessage(`{"address":"tcp://127.0.0.1:1"}`)))
	require.Equal(t, "tcp://127.0.0.1:1", a.address)

	// HealthCheck reports the truth instead, and the planner turns that into a
	// readable refusal.
	err := a.HealthCheck(context.Background())
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
}

func TestTheAddressComesFromConfigThenTheEnvironmentThenADefault(t *testing.T) {
	a := New()
	require.NoError(t, a.Configure(context.Background(), nil))
	require.Equal(t, "tcp://buildkit:1234", a.address)

	t.Setenv("PANDO_BUILDKIT_ADDRESS", "tcp://elsewhere:1234")
	b := New()
	require.NoError(t, b.Configure(context.Background(), nil))
	require.Equal(t, "tcp://elsewhere:1234", b.address)

	c := New()
	require.NoError(t, c.Configure(context.Background(), json.RawMessage(`{"address":"tcp://explicit:1234"}`)))
	require.Equal(t, "tcp://explicit:1234", c.address, "configuration wins over the environment")
}

func TestConfigureRejectsAMalformedDocument(t *testing.T) {
	err := New().Configure(context.Background(), json.RawMessage(`{`))
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}

// R-254: capabilities are data, so the planner can refuse at plan time.
func TestR254_CapabilitiesAreReportedHonestly(t *testing.T) {
	caps, err := New().Capabilities(context.Background())
	require.NoError(t, err)

	// Rootless BuildKit in a container: a shared kernel, like the runtime.
	// Reported honestly so a policy floor above this excludes it (R-114).
	require.Equal(t, spec.IsolationContainer, caps.IsolationClass)

	require.True(t, caps.Supports(spec.BuildDockerfile))
	require.True(t, caps.Supports(spec.BuildStatic))
	require.True(t, caps.Supports(spec.BuildBuildpack))
	require.True(t, caps.Supports(spec.BuildCompose),
		"a compose app's pieces are Dockerfile builds, one per service that builds")
	require.False(t, caps.Supports(spec.BuildPrebuilt), "a prebuilt image is not built")

	require.True(t, caps.SupportsCache)

	// Claiming egress restriction would turn R-118 into a promise nothing
	// keeps, so the planner refuses a spec that needs it instead.
	require.False(t, caps.SupportsEgressRestriction)
}

func TestBidIsConfidentOnlyWhenThereIsADockerfile(t *testing.T) {
	root := t.TempDir()

	bid, err := New().Bid(context.Background(), view{root: root})
	require.NoError(t, err)
	require.Zero(t, bid.Confidence)
	require.Equal(t, spec.BuildDockerfile, bid.Strategy)

	require.NoError(t, os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch"), 0o644))
	bid, err = New().Bid(context.Background(), view{root: root})
	require.NoError(t, err)
	require.InDelta(t, 0.92, bid.Confidence, 0.001)
	require.Equal(t, spec.BuildDockerfile, bid.Strategy)
	require.NotEmpty(t, bid.Evidence, "R-102: the user sees the reasoning, not a verdict")
}

// R-112: nothing here runs `docker compose build`, which would want a runtime
// socket and is forbidden outright.
func TestBuildRefusesWhenTheBuildServiceIsNotConnected(t *testing.T) {
	_, err := New().Build(context.Background(), api.BuildRequest{Strategy: spec.BuildDockerfile})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
}

// The build wraps the caller's sink so BuildKit cannot close it: closing it
// here would end the pipe before the caller had finished reading the other side.
func TestTheLogSinkIsNotClosedOnTheCallersBehalf(t *testing.T) {
	var sink strings.Builder
	w := nopWriteCloser{Writer: &sink}

	_, err := w.Write([]byte("step 1\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	_, err = w.Write([]byte("step 2\n"))
	require.NoError(t, err, "the underlying writer is still open")
	require.Equal(t, "step 1\nstep 2\n", sink.String())
}

// R-117 keeps a per-app cache namespace, and the path is resolved on Pando's
// side of the connection: a local cache export is written by the client through
// filesync, so it has to be a directory Pando itself can write to.
func TestTheCachePathIsPerAppAndOnPandosSide(t *testing.T) {
	require.Equal(t, filepath.Join("/var/lib/pando/buildcache", "app_01HQ8"), cachePath("app_01HQ8"))
	require.NotEqual(t, cachePath("app_01HQ8"), cachePath("app_01HQ9"), "R-117: one namespace per app")

	t.Setenv("PANDO_BUILD_CACHE_DIR", "/srv/cache")
	require.Equal(t, filepath.Join("/srv/cache", "app_01HQ8"), cachePath("app_01HQ8"))
}

func TestImageNamesAreValidReferences(t *testing.T) {
	// Lowercased and underscores replaced: neither is legal in an OCI
	// repository name, and an app ID carries both.
	require.Equal(t, "pando/app-01hq8:latest", imageName("APP_01HQ8", ""))
	require.Equal(t, "pando/my-app:latest", imageName("my_app", ""))
	require.True(t, strings.HasPrefix(imageName("x", ""), "pando/"))
}

// TestR146_EachBuildIsTaggedByItsDeployment asserts R-146's half in the
// builder: a build is loaded under its deployment's tag, never one the next
// build moves, so a build refused after it was loaded cannot take the place of
// the image a deployment recorded.
func TestR146_EachBuildIsTaggedByItsDeployment(t *testing.T) {
	require.Equal(t, "pando/app-01hq8:dep_01hq9", imageName("APP_01HQ8", "dep_01HQ9"))
	require.NotEqual(t, imageName("app_1", "dep_a"), imageName("app_1", "dep_b"))
}

// One runaway generator must not put a megabyte of text into an error envelope.
func TestSubprocessOutputIsBoundedKeepingTheEnd(t *testing.T) {
	require.Equal(t, "short output", trim([]byte("  short output\n")))

	long := strings.Repeat("a", 3000) + "the actual error"
	got := trim([]byte(long))
	require.Len(t, got, 2000)
	require.True(t, strings.HasSuffix(got, "the actual error"),
		"the end is what says why it failed")
}

// The same generator the build path uses, so what somebody reviews is what runs
// (R-102).
func TestPlanNeedsTheSourceOnDisk(t *testing.T) {
	_, _, _, err := New().Plan(context.Background(), viewWithoutRoot{})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "on disk")
}

// unreachable is an adapter whose client points at an address nothing listens
// on. Configuring it does not dial, so everything Build does before it talks to
// the build service runs, and the build service's refusal comes back as the
// build's own failure.
func unreachable(t *testing.T) *Adapter {
	t.Helper()
	a := New()
	require.NoError(t, a.Configure(context.Background(), json.RawMessage(`{"address":"tcp://127.0.0.1:1"}`)))
	require.NotNil(t, a.cli)
	t.Cleanup(func() { _ = a.cli.Close() })
	return a
}

// A build with nowhere to put its image is refused before anything is sent to
// the build service.
func TestABuildWithNoImageSinkIsRefused(t *testing.T) {
	root := writeFiles(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	_, err := unreachable(t).Build(context.Background(), api.BuildRequest{
		Strategy: spec.BuildDockerfile, Source: view{root: root},
	})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "nowhere to put the image")
}

// A source that is not on disk cannot be handed to BuildKit, and a generated
// plan that cannot be made stops the build before it starts.
func TestABuildNeedsASourceOnDiskAndAPlan(t *testing.T) {
	a := unreachable(t)
	_, err := a.Build(context.Background(), api.BuildRequest{Strategy: spec.BuildDockerfile, Source: viewWithoutRoot{}})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "on disk")

	_, err = a.Build(context.Background(), api.BuildRequest{
		Strategy: spec.BuildStatic, StaticDir: "missing", Source: view{root: t.TempDir()},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing")
}

// A build the build service cannot run fails as a build failure with a remedy,
// after the cache directory for the app has been prepared on Pando's side
// (R-117). The request's plan arguments, build arguments, target stage and
// the app's image label are all assembled on the way; only the build service
// can say what it did with them, so what is asserted here is that the build
// got as far as asking it.
func TestABuildTheBuildServiceCannotRunFailsWithARemedy(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("PANDO_BUILD_CACHE_DIR", cache)

	// A buildpack build of a Go program: the plan is made at build time and
	// read back from the checkout for its arguments.
	root := writeFiles(t, map[string]string{"go.mod": "module x\ngo 1.24\n", "main.go": "package main\n"})
	var sink strings.Builder
	_, err := unreachable(t).Build(context.Background(), api.BuildRequest{
		Strategy:       spec.BuildBuildpack,
		Source:         view{root: root},
		CacheNamespace: "app_01HQ8/web",
		Args:           map[string]string{"GOFLAGS": "-mod=mod"},
		Target:         "build",
		ImageSink:      &sink,
		Timeout:        10 * time.Second,
	})
	require.Error(t, err)
	code := errs.CodeOf(err)
	require.Contains(t, []errs.Code{errs.BuildFailed, errs.BuildTimeout}, code)
	require.NotEmpty(t, errs.As(err).Remedy, "R-105 promises a way forward")

	info, statErr := os.Stat(filepath.Join(cache, "app_01HQ8", "web"))
	require.NoError(t, statErr)
	require.True(t, info.IsDir(), "the per-app cache directory was prepared")

	_, statErr = os.Stat(filepath.Join(root, ".nixpacks", "Dockerfile"))
	require.NoError(t, statErr, "the plan was written into the checkout it builds")
}

// A cache directory that cannot be made stops the build with a message that
// says which step failed.
func TestABuildCacheThatCannotBePreparedStopsTheBuild(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o644))
	t.Setenv("PANDO_BUILD_CACHE_DIR", blocked)

	root := writeFiles(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	_, err := unreachable(t).Build(context.Background(), api.BuildRequest{
		Strategy: spec.BuildDockerfile, Source: view{root: root}, CacheNamespace: "app_01HQ8",
		ImageSink: io.Discard,
	})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	require.Equal(t, "Could not prepare the build cache.", errs.As(err).Message)
}

// The image label names the app, not the compose service: a deleted app's
// images are found by the app's ID.
func TestTheImageLabelNamesTheApp(t *testing.T) {
	require.Equal(t, "app_01HQ8", bundleOf("app_01HQ8"))
	require.Equal(t, "app_01HQ8", bundleOf("app_01HQ8/web"))
}

// Trimming the build service's cache is skipped when it is unbounded or there
// is no client, and otherwise asks the build service to prune, ignoring a
// failure: a cache that stays too big is not a broken build.
func TestTrimmingTheBuildCacheIgnoresFailure(t *testing.T) {
	New().trimCache(context.Background()) // no client: nothing to do

	unbounded := unreachable(t)
	unbounded.config.CacheMaxBytes = -1
	unbounded.trimCache(context.Background())

	a := unreachable(t)

	// A canceled context makes the prune fail at once rather than wait on an
	// address nothing listens on.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		a.trimCache(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a failed prune held the build")
	}
}

// viewWithoutRoot is a source view that cannot hand over a path, which is the
// case Plan has to refuse rather than assume.
type viewWithoutRoot struct{}

func (viewWithoutRoot) Open(string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
func (viewWithoutRoot) Stat(string) (api.FileInfo, error)  { return api.FileInfo{}, os.ErrNotExist }
func (viewWithoutRoot) Glob(string) ([]string, error)      { return nil, nil }
