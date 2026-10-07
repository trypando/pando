package buildkit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/tonistiigi/fsutil"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Kind is the adapter's kind string.
const Kind = "buildkit"

// Adapter builds images with rootless BuildKit in its own container (R-111).
//
// The build environment has no container runtime socket, and it cannot get one:
// BuildKit runs as a separate service that Pando talks to over its address, and
// nothing in that service's definition mounts a socket. R-112 is satisfied by
// the topology rather than by this code being careful, which is the only way it
// stays satisfied.
type Adapter struct {
	cli     *bkclient.Client
	config  Config
	address string

	// pruneMu keeps cache trims from piling up when builds finish together.
	pruneMu sync.Mutex

	// localCacheMu does the same for evicting apps' caches under Pando's
	// data directory (trimLocalCaches).
	localCacheMu sync.Mutex
}

// Config is the adapter's configuration.
type Config struct {
	// Address is where buildkitd listens. The bundled Compose file supplies
	// tcp://buildkit:1234.
	Address string `json:"address,omitempty"`

	// CacheMaxBytes caps the build service's own cache. Zero uses
	// defaultCacheMaxBytes; a negative value leaves it unbounded.
	CacheMaxBytes int64 `json:"cache_max_bytes,omitempty"`

	// LocalCacheMaxBytes caps every app's build cache under Pando's data
	// directory together, evicting whole apps' caches least recently used
	// first. Zero uses defaultLocalCacheMaxBytes; a negative value leaves them
	// unbounded.
	LocalCacheMaxBytes int64 `json:"local_cache_max_bytes,omitempty"`
}

// defaultCacheMaxBytes is how much the build service may keep between builds.
//
// [P]. Nothing ever pruned it: a 169-app run grew Docker's disk by about 75 GB
// and filled the host (issue #55). 10 GiB keeps the layers a busy install
// rebuilds from and trims the rest, oldest first.
const defaultCacheMaxBytes int64 = 10 << 30

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryBuilder }

func (a *Adapter) Configure(ctx context.Context, raw json.RawMessage) error {
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a.config); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The BuildKit configuration could not be read.", err)
		}
	}

	a.address = a.config.Address
	if a.address == "" {
		a.address = os.Getenv("PANDO_BUILDKIT_ADDRESS")
	}
	if a.address == "" {
		a.address = "tcp://buildkit:1234"
	}

	// Dialing is deferred: BuildKit may still be starting when Pando does, and
	// an adapter that refuses to configure would be dropped from the registry
	// for the life of the process. HealthCheck reports the truth instead, and
	// the planner turns that into a readable refusal.
	cli, err := bkclient.New(ctx, a.address)
	if err != nil {
		//nolint:nilerr // Deliberate: a dial failure at configure time is not a
		// configuration error. Returning it would drop the adapter from the
		// registry for the life of the process, and BuildKit may simply not be
		// up yet. HealthCheck reports the truth on every plan instead.
		return nil
	}
	a.cli = cli
	return nil
}

func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.cli == nil {
		cli, err := bkclient.New(ctx, a.address)
		if err != nil {
			return errs.Wrap(errs.AdapterUnavailable, "The build service is not responding.", err)
		}
		a.cli = cli
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if _, err := a.cli.ListWorkers(ctx); err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "The build service is not responding.", err)
	}
	return nil
}

// Capabilities reports what this builder can do (R-254).
func (a *Adapter) Capabilities(context.Context) (api.BuilderCapabilities, error) {
	return api.BuilderCapabilities{
		// Rootless BuildKit in a container: a shared kernel, like the runtime.
		// Reported honestly so a policy floor above this excludes it (R-114).
		IsolationClass: spec.IsolationContainer,

		// Compose is here because a compose app's pieces are Dockerfile builds:
		// core runs one per service that builds, each with that service's own
		// context and file. This builder never sees a compose file and never
		// needs to — nothing here runs `docker compose build`, which would want
		// a runtime socket and is forbidden outright (R-112).
		Strategies: []api.BuildStrategy{
			spec.BuildDockerfile, spec.BuildStatic, spec.BuildBuildpack, spec.BuildCompose,
		},

		SupportsCache: true,

		// Build-time egress restriction needs network policy BuildKit does not
		// expose directly. Claiming it would turn R-118 into a promise nothing
		// keeps, so the planner refuses a spec that needs it instead.
		SupportsEgressRestriction: false,
	}, nil
}

// Bid inspects the source. Detection proper is phase 6; this is the minimum
// that makes the auction's shape real.
func (a *Adapter) Bid(_ context.Context, src api.SourceView) (api.Bid, error) {
	if _, err := src.Stat("Dockerfile"); err == nil {
		return api.Bid{
			Confidence: 0.92,
			Strategy:   spec.BuildDockerfile,
			Evidence:   []string{"Dockerfile at repository root"},
		}, nil
	}
	return api.Bid{Confidence: 0, Strategy: spec.BuildDockerfile}, nil
}

// Build produces an image and streams it back to the caller.
//
// R-111: Pando hands BuildKit source and receives an image. The image is
// exported as a tarball rather than pushed to a registry, because a registry
// would need daemon-level configuration on the host and charge the setup cost
// R-002 says is paid once.
func (a *Adapter) Build(ctx context.Context, req api.BuildRequest) (api.BuildResult, error) {
	if a.cli == nil {
		return api.BuildResult{}, errs.New(errs.AdapterUnavailable, "The build service is not responding.")
	}
	dir, ok := req.Source.(interface{ Root() string })
	if !ok {
		return api.BuildResult{}, errs.New(errs.BuildFailed,
			"This builder needs the source on disk and was given something else.")
	}
	contextDir := dir.Root()
	if req.Context != "" {
		contextDir = filepath.Join(contextDir, filepath.Clean("/"+req.Context))
	}

	dockerfile := req.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	dockerfileDir := filepath.Dir(filepath.Join(contextDir, dockerfile))

	// A strategy with no Dockerfile in the repository gets one written for it.
	//
	// The context stays the checkout; only the generated Dockerfile lives in a
	// temporary directory, which BuildKit accepts because it takes the two as
	// separate filesystems. So nothing is copied and the app's source is never
	// modified.
	if req.Strategy != spec.BuildDockerfile {
		gen, genErr := synthesize(req, contextDir)
		if genErr != nil {
			return api.BuildResult{}, genErr
		}
		defer gen.Cleanup()
		dockerfileDir, dockerfile = gen.Dir, gen.Name
	}

	// R-119: a build that never finishes is a build that holds a slot forever.
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if req.ImageSink == nil {
		return api.BuildResult{}, errs.New(errs.BuildFailed,
			"This build has nowhere to put the image it produces.")
	}

	cacheDir := cachePath(req.CacheNamespace)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return api.BuildResult{}, errs.Wrap(errs.BuildFailed,
			"Could not prepare the build cache.", err)
	}
	// Marked as in use before the build, so eviction leaves it alone while
	// the build runs (cacheInUseFor), and ordered as most recently used after.
	touchCache(bundleOf(req.CacheNamespace), time.Now())

	contextFS, err := fsutil.NewFS(contextDir)
	if err != nil {
		return api.BuildResult{}, errs.Wrap(errs.BuildFailed, "Could not read the app's source.", err)
	}
	dockerfileFS, err := fsutil.NewFS(dockerfileDir)
	if err != nil {
		return api.BuildResult{}, errs.Wrap(errs.BuildFailed, "Could not read the app's Dockerfile.", err)
	}

	imageRef := imageName(req.CacheNamespace)

	frontendAttrs := map[string]string{
		"filename": filepath.Base(dockerfile),

		// Which app the image belongs to, on the image itself. The tag moves on
		// every rebuild and the previous image is left untagged, so the label is
		// the only thing that still says whose it is when the app is deleted —
		// and deleted apps left every image they had ever built on the host
		// (issue #55).
		"label:" + api.ImageLabelBundle: bundleOf(req.CacheNamespace),
	}
	// The plan's own arguments first, then the spec's: an app that sets one
	// itself means it.
	//
	// A plan made just now, at build time, is read from where it was written:
	// its arguments are in its build.sh like any other plan's, and reading
	// them only from a stored plan left a Poetry build running
	// `pip install poetry==` with the version empty (issue #55).
	plan := req.GeneratedFiles
	if len(plan) == 0 && req.Strategy == spec.BuildBuildpack {
		plan, _ = collectPlan(contextDir)
	}
	for k, v := range planArgs(plan) {
		frontendAttrs["build-arg:"+k] = v
	}
	for k, v := range req.Args {
		frontendAttrs["build-arg:"+k] = v
	}
	// The stage the spec names. It was stored and never passed, so a compose
	// service with `target: development` was built from the Dockerfile's last
	// stage — a production image without the dev server its command runs
	// (`nodemon: not found`, issue #55).
	if req.Target != "" {
		frontendAttrs["target"] = req.Target
	}

	solveOpt := bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: frontendAttrs,
		LocalMounts: map[string]fsutil.FS{
			"context":    contextFS,
			"dockerfile": dockerfileFS,
		},
		Exports: []bkclient.ExportEntry{{
			Type: bkclient.ExporterDocker,
			Attrs: map[string]string{
				"name": imageRef,
			},
			Output: func(map[string]string) (io.WriteCloser, error) {
				return nopWriteCloser{req.ImageSink}, nil
			},
		}},

		// R-117: the cache is namespaced per app, so one app's build cannot
		// read layers produced by another's.
		CacheExports: []bkclient.CacheOptionsEntry{{
			Type:  "local",
			Attrs: map[string]string{"dest": cacheDir},
		}},
		CacheImports: []bkclient.CacheOptionsEntry{{
			Type:  "local",
			Attrs: map[string]string{"src": cacheDir},
		}},
	}

	statusCh := make(chan *bkclient.SolveStatus, 16)
	logsDone := make(chan struct{})

	// Build logs stream to the caller live, which is what the console's SSE
	// endpoint forwards.
	go func() {
		streamStatus(statusCh, req.LogSink)
		close(logsDone)
	}()

	_, err = a.cli.Solve(ctx, nil, solveOpt, statusCh)
	<-logsDone

	// Trim the build service's cache now that this build is done with it. In
	// the background: the image is already on its way to the runtime, and a
	// deploy does not wait on housekeeping.
	go a.trimCache(context.WithoutCancel(ctx))

	if err != nil {
		if ctx.Err() != nil {
			return api.BuildResult{}, errs.Newf(errs.BuildTimeout,
				"The build took longer than %s and was stopped.", timeout).
				WithRemedy("Check the build logs for a step that is hanging, or raise the build timeout for this app.")
		}
		return api.BuildResult{}, errs.Wrap(errs.BuildFailed,
			"The build failed.", err).
			WithRemedy("Check the build logs above for the failing step.")
	}

	// The export just rewrote the cache's index; what it no longer reaches is
	// an earlier build's, and would otherwise stay for the life of the app.
	_, _ = pruneCacheDir(cacheDir)

	// And the caches together kept under their total, in the background for
	// the same reason as trimCache.
	go a.trimLocalCaches()

	return api.BuildResult{ImageRef: imageRef}, nil
}

// nopWriteCloser lets a plain writer satisfy BuildKit's exporter, which closes
// what it is given. Closing the caller's sink here would end the pipe before
// the caller had finished reading from the other side.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

var _ api.BuilderAdapter = (*Adapter)(nil)

// streamStatus forwards BuildKit's progress to a log sink in readable form.
func streamStatus(statusCh chan *bkclient.SolveStatus, sink io.Writer) {
	for status := range statusCh {
		if sink == nil {
			continue
		}
		for _, v := range status.Vertexes {
			if v.Started != nil && v.Completed == nil {
				fmt.Fprintf(sink, "=> %s\n", v.Name)
			}
			if v.Error != "" {
				fmt.Fprintf(sink, "!! %s: %s\n", v.Name, v.Error)
			}
		}
		for _, l := range status.Logs {
			_, _ = sink.Write(l.Data)
		}
	}
}

// trimCache prunes the build service's cache down to its cap, oldest first.
// Failures are ignored: a cache that stays too big is the state before this
// existed, not a broken build.
func (a *Adapter) trimCache(ctx context.Context) {
	limit := a.config.CacheMaxBytes
	if limit == 0 {
		limit = defaultCacheMaxBytes
	}
	if limit < 0 || a.cli == nil {
		return
	}
	a.pruneMu.Lock()
	defer a.pruneMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_ = a.cli.Prune(ctx, nil, bkclient.WithKeepOpt(0, 0, limit, 0))
}

// bundleOf is the app a cache namespace belongs to: the namespace is the app's
// ID, or the app's ID and a compose service's name.
func bundleOf(namespace string) string {
	bundle, _, _ := strings.Cut(namespace, "/")
	return bundle
}

func imageName(namespace string) string {
	clean := strings.ToLower(strings.ReplaceAll(namespace, "_", "-"))
	return fmt.Sprintf("pando/%s:latest", clean)
}

// cachePath is resolved on THIS side of the connection, not inside buildkitd.
//
// A local cache export is written by the client through filesync, so the path
// has to be one Pando itself can write to — pointing it at a directory in the
// BuildKit container fails with a permission error that reads as a build
// failure. R-117's per-app namespace is preserved either way.
func cachePath(namespace string) string {
	return filepath.Join(cacheRoot(), namespace)
}

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryBuilder,
		Kind:        Kind,
		Name:        "BuildKit",
		Description: "Builds images from source in an isolated BuildKit daemon.",
		IDPrefix:    "bld_",
		Fields: []api.Field{
			{Key: "address", Label: "BuildKit address", Type: "string", Help: "Where the BuildKit daemon listens. PANDO_BUILDKIT_ADDRESS is used when this is empty.", Default: "tcp://buildkit:1234", Advanced: true},
		},
	}
}
