package buildkit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
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
	// cli is the BuildKit an install topology runs (Config.Address), shared
	// by every app. Nil when Pando runs its own, one per app (daemons).
	cli     *bkclient.Client
	config  Config
	address string

	// managed starts and removes the BuildKits Pando runs itself, one per
	// app, and daemons holds them. Nil when the install topology names one.
	managed *managed
	daemons *daemons

	// pruneMu keeps cache trims from piling up when builds finish together.
	pruneMu sync.Mutex

	// localCacheMu does the same for evicting apps' caches under Pando's
	// data directory (trimLocalCaches).
	localCacheMu sync.Mutex
}

// Config is the adapter's configuration.
type Config struct {
	// Address is the BuildKit an install topology runs beside Pando, set by
	// it through PANDO_BUILDKIT_ADDRESS: deploy/kubernetes runs one as its
	// own Deployment. Internal, and not among Info's settings: Pando does not
	// support a BuildKit on another server, so nobody sets this by hand.
	// Unset, Pando runs its own on the Docker daemon it runs apps on, one per
	// app (managed.go, daemons.go). One named here is shared by every app.
	Address string `json:"address,omitempty"`

	// TLS for the BuildKit at Address: files in Pando's container holding
	// the CA its certificate is checked against, and the client certificate
	// and key Pando presents. Default PANDO_BUILDKIT_TLS_CA,
	// PANDO_BUILDKIT_TLS_CERT and PANDO_BUILDKIT_TLS_KEY. TLSServerName,
	// PANDO_BUILDKIT_TLS_SERVER_NAME, is the name its certificate is checked
	// for when it is not the address's host. Unset, the connection is plain.
	TLSCAFile     string `json:"tls_ca_file,omitempty"`
	TLSCertFile   string `json:"tls_cert_file,omitempty"`
	TLSKeyFile    string `json:"tls_key_file,omitempty"`
	TLSServerName string `json:"tls_server_name,omitempty"`

	// RegistryHost and RegistryCAFile let the BuildKit Pando runs trust a
	// registry signed by the installation's own CA, such as the one
	// docker-compose.registry.yml runs: host:port, and a file in Pando's
	// container holding the CA. Default PANDO_BUILDKIT_REGISTRY_HOST and
	// PANDO_BUILDKIT_REGISTRY_CA. Internal, like Networks: the install
	// topology sets them. Not used with Address, whose BuildKit the topology
	// configures itself.
	RegistryHost   string `json:"registry_host,omitempty"`
	RegistryCAFile string `json:"registry_ca_file,omitempty"`

	// Networks are Docker networks Pando's own BuildKit joins besides its
	// own, so it reaches a registry on one of them. Default
	// PANDO_BUILDKIT_NETWORKS, separated by commas.
	Networks []string `json:"networks,omitempty"`

	// CacheMaxBytes and LocalCacheMaxBytes are the only settings Info offers
	// a person, both advanced.

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

// configureManaged sets up the BuildKits Pando runs itself, one per app, when
// the install topology names none. None starts here: each starts when its
// app's first build needs it. What an earlier run left behind is removed.
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
	a.daemons = newDaemons(a.managed)
	a.managed.sweep(ctx, true)
	return nil
}

// LimitDaemons bounds how many of Pando's own BuildKits run and wait warm
// together: limit is host policy's max_concurrent_deploys, read as the deploy
// queue reads it, zero or less for its default. Without it, the bound is that
// default.
func (a *Adapter) LimitDaemons(limit func(context.Context) int) {
	if a.daemons != nil {
		a.daemons.limit = limit
	}
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
		return a.configureManaged(ctx)
	}

	// Dialing is deferred: BuildKit may still be starting when Pando does, and
	// an adapter that refuses to configure would be dropped from the registry
	// for the life of the process. HealthCheck reports the truth instead, and
	// the planner turns that into a readable refusal.
	opts, err := a.externalTLS()
	if err != nil {
		return err
	}
	cli, err := bkclient.New(ctx, a.address, opts...)
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

// externalTLS is the TLS the install topology's BuildKit is reached with, from
// Config or the environment: none, a CA to check it against, a client
// certificate to present, or both.
func (a *Adapter) externalTLS() ([]bkclient.ClientOpt, error) {
	ca := setting(a.config.TLSCAFile, "PANDO_BUILDKIT_TLS_CA")
	cert := setting(a.config.TLSCertFile, "PANDO_BUILDKIT_TLS_CERT")
	key := setting(a.config.TLSKeyFile, "PANDO_BUILDKIT_TLS_KEY")
	serverName := setting(a.config.TLSServerName, "PANDO_BUILDKIT_TLS_SERVER_NAME")
	if (cert == "") != (key == "") {
		return nil, errs.New(errs.ValidInvalid,
			"The build service was given half a client certificate: PANDO_BUILDKIT_TLS_CERT and PANDO_BUILDKIT_TLS_KEY go together.").
			WithRemedy("Set both, as files in Pando's container, or neither.")
	}
	var opts []bkclient.ClientOpt
	if ca != "" {
		if serverName == "" {
			if u, err := url.Parse(a.address); err == nil {
				serverName = u.Hostname()
			}
		}
		opts = append(opts, bkclient.WithServerConfig(serverName, ca))
	}
	if cert != "" {
		opts = append(opts, bkclient.WithCredentials(cert, key))
	}
	return opts, nil
}

// setting is a configured value, or the environment's when there is none.
func setting(configured, env string) string {
	if configured != "" {
		return configured
	}
	return os.Getenv(env)
}

func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.managed != nil {
		// Plan time: Docker, the build authority and the image only. No
		// daemon is started or waited on here.
		return a.managed.healthy(ctx)
	}
	if a.cli == nil {
		opts, err := a.externalTLS()
		if err != nil {
			return err
		}
		cli, err := bkclient.New(ctx, a.address, opts...)
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

// buildKitStartWait is how long a build waits for the BuildKit Pando started
// for it to answer.
const buildKitStartWait = time.Minute

// daemonFor is the BuildKit a build of namespace's app runs on, and what to
// call when the build is done with it: the app's own when Pando runs them,
// the install topology's otherwise.
func (a *Adapter) daemonFor(ctx context.Context, namespace string) (*bkclient.Client, func(), error) {
	if a.daemons == nil {
		if a.cli == nil {
			return nil, nil, errs.New(errs.AdapterUnavailable, "The build service is not responding.")
		}
		return a.cli, keepShared, nil
	}
	d, err := a.daemons.acquire(ctx, bundleOf(namespace))
	if err != nil {
		return nil, nil, err
	}
	return d.inst.cli, func() { a.daemons.release(ctx, d) }, nil
}

// keepShared is what a build does with the install topology's BuildKit when
// it is done: nothing, since it outlives every build.
func keepShared() {
	// The topology's to keep running.
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
	if a.cli == nil && a.daemons == nil {
		return api.BuildResult{}, errs.New(errs.AdapterUnavailable, "The build service is not responding.")
	}
	contextDir, err := sourceDir(req)
	if err != nil {
		return api.BuildResult{}, err
	}
	dockerfileDir, dockerfile, cleanup, err := prepareDockerfile(req, contextDir)
	if err != nil {
		return api.BuildResult{}, err
	}
	defer cleanup()

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

	cli, done, err := a.daemonFor(ctx, req.CacheNamespace)
	if err != nil {
		return api.BuildResult{}, err
	}
	// Trim the build service's cache now that this build is done with it, and
	// only then let the daemon go. In the background: the image is already on
	// its way to the runtime, and a deploy does not wait on housekeeping.
	defer func() {
		go func() {
			a.trimCache(context.WithoutCancel(ctx), cli)
			done()
		}()
	}()

	// A build that may be steered keeps the end of its log, which is what
	// the steering reads.
	steerable := req.Steer && req.Strategy == spec.BuildBuildpack && isRailpackPlan(dockerfile)
	var log *logTail
	if steerable {
		log = newLogTail(req.LogSink)
		req.LogSink = log
	}
	job := solveJob{cli: cli, req: req, contextDir: contextDir, cacheDir: cacheDir, timeout: timeout}
	result, err := a.solve(ctx, job, dockerfileDir, dockerfile)
	if err == nil || !steerable || ctx.Err() != nil {
		return result, err
	}
	return a.steer(ctx, job, err, log)
}

// sourceDir is the directory on disk the build's context is read from: the
// checkout, or the subdirectory of it the request names.
func sourceDir(req api.BuildRequest) (string, error) {
	dir, ok := req.Source.(interface{ Root() string })
	if !ok {
		return "", errs.New(errs.BuildFailed,
			"This builder needs the source on disk and was given something else.")
	}
	contextDir := dir.Root()
	if req.Context != "" {
		contextDir = filepath.Join(contextDir, filepath.Clean("/"+req.Context))
	}
	return contextDir, nil
}

// prepareDockerfile returns the directory and name of the Dockerfile to build,
// and a cleanup to run once the build is done.
//
// A strategy with no Dockerfile in the repository gets one written for it.
//
// The context stays the checkout; only the generated Dockerfile lives in a
// temporary directory, which BuildKit accepts because it takes the two as
// separate filesystems. So nothing is copied and the app's source is never
// modified.
func prepareDockerfile(req api.BuildRequest, contextDir string) (dir, name string, cleanup func(), err error) {
	if req.Strategy != spec.BuildDockerfile {
		gen, genErr := synthesize(req, contextDir)
		if genErr != nil {
			return "", "", nil, genErr
		}
		return gen.Dir, gen.Name, gen.Cleanup, nil
	}
	name = req.Dockerfile
	if name == "" {
		name = "Dockerfile"
	}
	dir = filepath.Dir(filepath.Join(contextDir, name))
	if len(req.GeneratedFiles) > 0 {
		// The repository's Dockerfile with its own build run first
		// (prebuild.go), replayed from the spec as written (R-020).
		if err := writeInto(contextDir, req.GeneratedFiles); err != nil {
			return "", "", nil, err
		}
		if err := writePlanIgnore(contextDir, name); err != nil {
			return "", "", nil, err
		}
	}
	return dir, name, keepDockerfile, nil
}

// keepDockerfile is the cleanup for a Dockerfile the repository already has:
// it is the app's own file, so there is nothing of Pando's to remove.
func keepDockerfile() {
	// Nothing to undo: the file is the repository's.
}

// solve runs one build of the plan or Dockerfile named, from contextDir.
// solveJob is what every solve of one build shares: the daemon it runs on,
// the request, the source and cache directories, and the time it has.
type solveJob struct {
	cli                  *bkclient.Client
	req                  api.BuildRequest
	contextDir, cacheDir string
	timeout              time.Duration
}

func (a *Adapter) solve(ctx context.Context, job solveJob, dockerfileDir, dockerfile string) (api.BuildResult, error) {
	cli, req, contextDir, cacheDir, timeout := job.cli, job.req, job.contextDir, job.cacheDir, job.timeout
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
	// cache mount by key across every build it runs. A daemon Pando runs is
	// one app's (R-117); the install topology's is every app's, and there the
	// bundle keeps one app's package caches out of another's.
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
		resp, err = cli.Build(ctx, solveOpt, "pando", railpackBuild(map[string]string{
			api.ImageLabelBundle: bundleOf(req.CacheNamespace),
		}), statusCh)
	} else {
		resp, err = cli.Solve(ctx, nil, solveOpt, statusCh)
	}
	<-logsDone

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
func (a *Adapter) trimCache(ctx context.Context, cli *bkclient.Client) {
	limit := a.config.CacheMaxBytes
	if limit == 0 {
		limit = defaultCacheMaxBytes
	}
	if limit < 0 || cli == nil {
		return
	}
	a.pruneMu.Lock()
	defer a.pruneMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_ = cli.Prune(ctx, nil, bkclient.WithKeepOpt(0, 0, limit, 0))
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
		Description: "Builds images from source in a rootless BuildKit that Pando runs itself.",
		IDPrefix:    "bld_",
		// Pando starts this BuildKit itself, or the install topology does
		// (Config.Address); a person never adds one or points it elsewhere.
		Managed: true,
		// Only the cache caps. The address, registry and networks are the
		// install topology's, set through its environment (Config).
		Fields: []api.Field{
			{Key: "cache_max_bytes", Label: "Build service cache", Type: "int", Help: "Bytes BuildKit keeps between builds, oldest trimmed first; a negative number removes the limit.", Default: bytesDefault(defaultCacheMaxBytes), Advanced: true},
			{Key: "local_cache_max_bytes", Label: "App build caches", Type: "int", Help: "Bytes every app's build cache may use together, least recently used evicted first; a negative number removes the limit.", Default: bytesDefault(defaultLocalCacheMaxBytes), Advanced: true},
		},
	}
}

// bytesDefault states a byte default as the number the setting takes and the
// GiB it comes to, so the form shows what leaving it empty gets.
func bytesDefault(n int64) string {
	return fmt.Sprintf("%d (%d GiB)", n, n>>30)
}
