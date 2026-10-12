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
	"github.com/moby/moby/client"
	"github.com/tonistiigi/fsutil"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/registrylimit"
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

	// managed is the BuildKit Pando runs itself, when no address names one
	// somebody else runs. Nil otherwise.
	managed *managed

	// answers asks BuildKit whether it is up; nil asks it for its workers.
	// For tests, which have no BuildKit to ask.
	answers func(context.Context) error

	// pruneMu keeps cache trims from piling up when builds finish together.
	pruneMu sync.Mutex

	// localCacheMu does the same for evicting apps' caches under Pando's
	// data directory (trimLocalCaches).
	localCacheMu sync.Mutex
}

// Config is the adapter's configuration.
type Config struct {
	// Address is a BuildKit somebody else runs: Kubernetes' Deployment, a
	// multi-host control host's, one with its own registry configuration.
	// Unset here and in PANDO_BUILDKIT_ADDRESS, Pando runs its own on the
	// Docker daemon it runs apps on (managed.go, issue #130).
	Address string `json:"address,omitempty"`

	// RegistryHost and RegistryCAFile let the BuildKit Pando runs trust a
	// registry signed by the installation's own CA, such as the one
	// docker-compose.registry.yml runs: host:port, and a file in Pando's
	// container holding the CA. Default PANDO_BUILDKIT_REGISTRY_HOST and
	// PANDO_BUILDKIT_REGISTRY_CA. Not used with Address, whose BuildKit is
	// configured by whoever runs it.
	RegistryHost   string `json:"registry_host,omitempty"`
	RegistryCAFile string `json:"registry_ca_file,omitempty"`

	// Networks are Docker networks Pando's own BuildKit joins besides its
	// own, so it reaches a registry on one of them. Default
	// PANDO_BUILDKIT_NETWORKS, separated by commas.
	Networks []string `json:"networks,omitempty"`

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

// registryFiles reads the registry settings for Pando's own BuildKit.
func (a *Adapter) registryFiles() (map[string][]byte, error) {
	host := a.config.RegistryHost
	if host == "" {
		host = os.Getenv("PANDO_BUILDKIT_REGISTRY_HOST")
	}
	caFile := a.config.RegistryCAFile
	if caFile == "" {
		caFile = os.Getenv("PANDO_BUILDKIT_REGISTRY_CA")
	}
	if host == "" && caFile == "" {
		return nil, nil
	}
	if host == "" || caFile == "" {
		return nil, errs.New(errs.ValidInvalid,
			"The build service was given half a registry: PANDO_BUILDKIT_REGISTRY_HOST and PANDO_BUILDKIT_REGISTRY_CA go together.").
			WithRemedy("Set both — the registry as host:port, and the file in Pando's container holding the CA that signed its certificate — or neither.")
	}
	ca, err := os.ReadFile(caFile) //nolint:gosec // G703: the operator's own setting, read from Pando's own filesystem; nothing from a request reaches it.
	if err != nil {
		return nil, errs.Wrap(errs.ValidInvalid, "PANDO_BUILDKIT_REGISTRY_CA is "+caFile+", which Pando could not read.", err).
			WithRemedy("Mount the registry's CA certificate into Pando's container at that path.")
	}
	return registryFiles(host, ca), nil
}

// dockerClient is the daemon Pando runs its own BuildKit on: the one it runs
// apps on, from DOCKER_HOST or the default socket. A variable so a test can
// hand it a fake rather than start a real BuildKit.
var dockerClient = func() (dockerAPI, error) { return client.New(client.FromEnv) }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryBuilder }

// configureManaged sets up the BuildKit Pando runs itself, when no address
// names one somebody else runs. Started now if Docker is up, and by every
// health check after that if it is not, or if it stopped.
func (a *Adapter) configureManaged(ctx context.Context) error {
	dc, err := dockerClient()
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not reach Docker to run the build service on.", err)
	}
	a.managed = newManaged(dc)
	files, err := a.registryFiles()
	if err != nil {
		return err
	}
	a.managed.files = files
	a.managed.networks = a.config.Networks
	if len(a.managed.networks) == 0 {
		a.managed.networks = envNetworks()
	}
	a.address = a.managed.address(true)
	if addr, err := a.managed.ensure(ctx); err == nil {
		a.address = addr
	}
	return nil
}

// envNetworks is PANDO_BUILDKIT_NETWORKS, a comma-separated list.
func envNetworks() []string {
	var networks []string
	for _, n := range strings.Split(os.Getenv("PANDO_BUILDKIT_NETWORKS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			networks = append(networks, n)
		}
	}
	return networks
}

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
		if err := a.configureManaged(ctx); err != nil {
			return err
		}
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
	if a.managed != nil {
		addr, err := a.managed.ensure(ctx)
		if err != nil {
			return err
		}
		if addr != a.address {
			a.address, a.cli = addr, nil
		}
	}
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
		e := errs.Wrap(errs.AdapterUnavailable, "The build service is not responding.", err)
		if a.managed != nil {
			e = e.WithRemedy(a.managed.notAnswering())
		}
		return e
	}
	return nil
}

// buildKitStartWait is how long a build waits for the BuildKit Pando runs to
// answer, when the build is what found it stopped or missing.
const buildKitStartWait = time.Minute

// buildKitRetryEvery is how often startBuildKit asks again.
var buildKitRetryEvery = time.Second

// startBuildKit makes sure the BuildKit Pando runs is there and answering
// before a build, creating or starting it as ensure does.
//
// Configure did this once, and ignored a failure, and the planner checks no
// builder's health before a deploy. So a BuildKit stopped after Pando started —
// every container on rootless Podman, after a reboot, since Podman restarts
// nothing — stayed down, and each build dialed a name that was not there
// (issue #179). Not HealthCheck, which also replaces the client: builds run
// side by side, and this touches only what ensure guards.
func (a *Adapter) startBuildKit(ctx context.Context, wait time.Duration) error {
	if a.managed == nil {
		return nil
	}
	if _, err := a.managed.ensure(ctx); err != nil {
		return err
	}
	answers := a.answers
	if answers == nil {
		answers = func(ctx context.Context) error {
			_, err := a.cli.ListWorkers(ctx)
			return err
		}
	}
	deadline := time.Now().Add(wait)
	for {
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := answers(check)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return errs.Wrap(errs.AdapterUnavailable, "The build service is not responding.", err).
				WithRemedy(a.managed.notAnswering())
		}
		select {
		case <-ctx.Done():
		case <-time.After(buildKitRetryEvery):
		}
	}
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

		// BuildKit's image exporter pushes to a registry with the credential
		// the build carries, through the client session (push.go). Nothing is
		// written to buildkitd's configuration.
		SupportsPush: true,
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

// Build produces an image and streams it back to the caller, or pushes it to
// the install's registry when the request says so.
//
// R-111: Pando hands BuildKit source and receives an image. On a single host
// the image is exported as a tarball rather than pushed to a registry, because
// a registry would need daemon-level configuration on the host and charge the
// setup cost R-002 says is paid once (O-34). A runtime that pulls from a
// registry gets Push instead (issue #72, PR 5).
func (a *Adapter) Build(ctx context.Context, req api.BuildRequest) (api.BuildResult, error) {
	if a.cli == nil {
		return api.BuildResult{}, errs.New(errs.AdapterUnavailable, "The build service is not responding.")
	}
	if err := a.startBuildKit(ctx, buildKitStartWait); err != nil {
		return api.BuildResult{}, err
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

	if (req.ImageSink == nil) == (req.Push == nil) {
		return api.BuildResult{}, errs.New(errs.BuildFailed,
			"This build has nowhere to put the image it produces, or was given two places.")
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

	imageRef := imageName(req.CacheNamespace, req.Tag)

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
	// A Railpack plan's cache mounts are keyed by this, and BuildKit shares a
	// cache mount by key across every build it runs. The bundle keeps one
	// app's package caches out of another's, as R-117 asks of the layer cache.
	railpack := isRailpackPlan(dockerfile)
	if railpack {
		frontendAttrs["build-arg:cache-key"] = bundleOf(req.CacheNamespace)
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

	if req.Push != nil {
		solveOpt.Exports = []bkclient.ExportEntry{pushExport(req.Push)}
		solveOpt.Session = append(solveOpt.Session, pushAuth(req.Push))
	}

	var resp *bkclient.SolveResponse
	if railpack {
		solveOpt.Frontend = ""
		resp, err = a.cli.Build(ctx, solveOpt, "pando", railpackBuild(map[string]string{
			api.ImageLabelBundle: bundleOf(req.CacheNamespace),
		}), statusCh)
	} else {
		resp, err = a.cli.Solve(ctx, nil, solveOpt, statusCh)
	}
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
		return api.BuildResult{}, solveFailed(err)
	}

	// The export just rewrote the cache's index; what it no longer reaches is
	// an earlier build's, and would otherwise stay for the life of the app.
	_, _ = pruneCacheDir(cacheDir)

	// And the caches together kept under their total, in the background for
	// the same reason as trimCache.
	go a.trimLocalCaches()

	if req.Push != nil {
		return pushedResult(req.Push, resp)
	}
	return api.BuildResult{ImageRef: imageRef}, nil
}

// solveFailed is a build's failure as a person reads it. A base image the
// registry refused for its download limit is said as that (R-105), in the
// words the runtimes use for the same refusal; anything else is in the log.
func solveFailed(err error) error {
	if registrylimit.Mentioned(err.Error()) {
		r := registrylimit.FromText(err.Error(), "")
		r.Build = true
		return r.Error(time.Now(), err)
	}
	// The solver's own reason, in the message. A frontend that fails before
	// any step runs — Railpack's, reading a plan it cannot build — reports
	// nothing in the log, and its error was the only thing that said why: the
	// build ended with "The build failed." and no failing step (issue #90,
	// R-105).
	reason := solverReason(err)
	if reason == "" {
		return errs.Wrap(errs.BuildFailed, "The build failed.", err).
			WithRemedy("Check the build logs above for the failing step.")
	}
	return errs.Wrap(errs.BuildFailed, "The build failed: "+reason, err).
		WithRemedy("Check the build logs above for the failing step. If there is none, the reason above is the build service's own.")
}

// solverPrefixes are the layers BuildKit and gRPC wrap a build's error in, which
// say where the error travelled rather than what it was.
var solverPrefixes = []string{
	"failed to solve:",
	"failed to solve with frontend gateway.v0:",
	"failed to solve with frontend dockerfile.v0:",
	"rpc error:",
	"code = Unknown desc =",
	"code = Internal desc =",
	"code = Unavailable desc =",
	"code = Canceled desc =",
	"code = InvalidArgument desc =",
	"code = NotFound desc =",
	"code = FailedPrecondition desc =",
}

// maxSolverReason bounds the reason quoted in a message; the log carries the
// rest.
const maxSolverReason = 600

// solverReason is a build error's text without its transport wrapping: the
// first line, the gRPC and "failed to solve" prefixes removed, held to a
// length that reads as a sentence.
func solverReason(err error) string {
	text := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	for changed := true; changed; {
		changed = false
		for _, prefix := range solverPrefixes {
			if strings.HasPrefix(text, prefix) {
				text = strings.TrimSpace(strings.TrimPrefix(text, prefix))
				changed = true
			}
		}
	}
	if len(text) > maxSolverReason {
		text = strings.ToValidUTF8(text[:maxSolverReason], "") + "…"
	}
	return text
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

// imageName is the tag a streamed build is loaded under: pando/<app>:<tag>,
// where the tag is the deployment's ID. One per build rather than :latest, so
// a build refused after it was loaded never takes the place of the one that
// runs (R-146). The runtime prunes old ones.
func imageName(namespace, tag string) string {
	clean := strings.ToLower(strings.ReplaceAll(namespace, "_", "-"))
	tag = strings.ToLower(tag)
	if tag == "" {
		tag = "latest"
	}
	return fmt.Sprintf("pando/%s:%s", clean, tag)
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
