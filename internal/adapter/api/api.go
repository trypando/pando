package api

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/egress"
	"github.com/trypando/pando/internal/secret"
)

// IsolationClass is ordered, so policy floors can be compared (R-114, R-255).
//
// An integer rather than a string enum precisely because R-024 and R-114 need
// `got >= floor`. Gaps of 10 leave room to insert a class without a migration.
type IsolationClass = spec.IsolationClass

// RoutingMode and BuildStrategy are the spec's, not the adapter's. An adapter
// that needed its own vocabulary for these would be translating in the wrong
// direction (R-251).
type (
	RoutingMode   = spec.RoutingMode
	BuildStrategy = spec.BuildStrategy
	EgressMode    = spec.EgressMode
)

// --- capabilities ----------------------------------------------------------
//
// Capabilities are returned as data, never discovered by type assertion
// (R-254). A type assertion is invisible to the planner and cannot produce a
// readable plan-time error, which is the whole point of asking.

// RuntimeCapabilities describes what a runtime adapter can do.
type RuntimeCapabilities struct {
	IsolationClass IsolationClass

	SupportsPersistentVolumes bool
	SupportsExec              bool
	SupportsMultipleWorkloads bool

	// SupportsCarriedFiles is whether the runtime can place a configuration
	// file inside a workload before it starts (spec.File).
	//
	// Data, never a type assertion (R-254): a planner that cannot see the
	// answer cannot produce a readable plan-time error, and an app whose
	// Caddyfile silently did not arrive starts and serves the wrong thing.
	SupportsCarriedFiles bool

	// SupportsPrivateNetwork is required for R-026. An adapter without it is
	// unusable — it would place workloads somewhere other apps could reach.
	SupportsPrivateNetwork bool

	// SupportsResourceLimits is whether the runtime applies the CPU and
	// memory limits every app has (R-240) — applies, not accepts. Docker
	// accepts limits it then drops, on a host whose cgroups cannot hold them
	// (issue #130). False, and ResourceLimitsRemedy says why and what fixes it,
	// in the runtime's own words: the planner refuses with it as the remedy.
	SupportsResourceLimits bool
	ResourceLimitsRemedy   string
	SupportsStartThenSwap  bool // R-145

	// ReportsUsage means Usage works: the runtime can say what each workload
	// is using now (R-245). False, and the console says the runtime does not
	// report it rather than showing zeros.
	ReportsUsage bool

	// LogRetention is what the runtime can do about R-222's size cap.
	LogRetention LogRetentionCapability

	// ImageDelivery is how an image built by Pando can reach this runtime, in
	// the runtime's order of preference (issue #72, PR 5). Empty means it
	// cannot run a built image at all, only a published one.
	//
	// ImageDeliveryImport: the runtime takes the image as bytes (ImportImage).
	// R-111 says Pando "hands it source, receives an image", so on a single
	// host the built image travels back through Pando rather than via a
	// registry — which would otherwise need daemon-level configuration and
	// charge the setup cost R-002 says is paid once (O-34).
	//
	// ImageDeliveryRegistry: the runtime pulls the image by digest from the
	// install's registry, with WorkloadPlan.PullAuth. A runtime spanning
	// machines offers only this: pushing a tarball to every node is the wrong
	// shape.
	//
	// Data, never a type assertion (R-254): the planner turns a runtime, a
	// builder and the install's registry that cannot meet into a readable
	// plan-time refusal rather than a failure after the build has already run.
	ImageDelivery []ImageDelivery

	// EdgeConfig is how an edge on this runtime can receive its routes, for a
	// routing adapter that writes them (notes-kubernetes-runtime-issue-72.md).
	// Core hands it to the routing adapter in EdgeRequest; a routing adapter
	// that can use none of them refuses its edge rather than running one that
	// never learns a route. Data, never a type assertion (R-254).
	EdgeConfig []EdgeConfig

	// SupportsSelfUpgrade means this runtime runs Pando itself and can start
	// the helper that replaces it (R-355, R-359): the adapter implements
	// SelfUpgrader. Data, never a type assertion (R-254), so the Updates
	// screen can say why an in-place upgrade is not possible rather than
	// discovering it halfway.
	SupportsSelfUpgrade bool

	// MaxWorkloadsPerBundle is 0 for unlimited.
	MaxWorkloadsPerBundle int

	// SupportsTrialRun means Trial can start a throwaway workload and report
	// whether it stayed up (R-097). A runtime without it makes detection skip
	// the trial, which turns deferred questions back into real ones rather
	// than into a failure.
	SupportsTrialRun bool

	// SupportsPortObservation means Trial reports ObservedPorts. This is the
	// capability R-097 is really about — "port discovery happens by observing
	// what the process binds, not by asking" — so a runtime without it costs
	// the user a question per app.
	SupportsPortObservation bool

	// SupportsWriteObservation means Trial reports ObservedWrites, which is
	// what lets the persistence warning name the directory (R-202) instead of
	// being generic (R-201).
	SupportsWriteObservation bool

	// SupportsEdge means the runtime can run an edge — the install-scoped
	// workload a routing adapter puts in front of Pando (R-174, edge.go). A
	// routing adapter that needs one is refused a start on a runtime without
	// it, rather than configured and silently unreachable.
	SupportsEdge bool

	// SupportsEgressRestriction means the runtime enforces NetworkPlan.Egress
	// when it restricts anything (R-186). Without it, a plan whose rules
	// refuse something is refused, never deployed with them ignored.
	SupportsEgressRestriction bool

	// SupportsBundleEvents means WatchBundles reports what happens to bundles
	// as it happens: a workload exiting, being killed for memory, starting,
	// being removed, changing health (O-52). The reconciler then visits an app
	// the moment something happens to it and looks at a settled app only on a
	// slow sweep. Without it every app is looked at on every pass, as before.
	// Data, never a type assertion (R-254).
	SupportsBundleEvents bool

	// Platform is the operating system and CPU architecture the runtime runs
	// images for, as a registry names them: linux/amd64, linux/arm64. Empty
	// when the runtime cannot say, and then an image's platforms are not
	// checked at plan time (issue #41).
	Platform string
}

// ImageDelivery is one way a built image reaches a runtime.
type ImageDelivery string

const (
	// ImageDeliveryImport streams the image from the builder into the
	// runtime's ImportImage.
	ImageDeliveryImport ImageDelivery = "import"

	// ImageDeliveryRegistry has the builder push the image to the install's
	// registry and the runtime pull it by digest.
	ImageDeliveryRegistry ImageDelivery = "registry"
)

// Delivers reports whether the runtime takes a built image this way.
func (c RuntimeCapabilities) Delivers(d ImageDelivery) bool {
	for _, have := range c.ImageDelivery {
		if have == d {
			return true
		}
	}
	return false
}

// RegistryAuth is what one image pull authenticates with, resolved by core
// from the app's registry credential (issue #41). Short-lived — an ECR
// password lasts twelve hours — so it travels with each plan and is never kept
// by the adapter. Nil means an anonymous pull.
type RegistryAuth struct {
	Registry string
	Username string
	Password secret.Value
	// IdentityToken stands in for a password where a registry was signed in
	// to through a browser (a Docker login carried over, issue #41).
	IdentityToken secret.Value
}

// RoutingCapabilities describes what a routing adapter can do.
type RoutingCapabilities struct {
	Modes []RoutingMode

	// DefaultMode is what an app gets when nobody chooses (R-162). It is what
	// makes an install feel like proxy mode or per-hostname — neither topology
	// is a global setting (design 03 §4.1).
	DefaultMode RoutingMode

	// BaseDomain is what a new app's hostname is carved out of under this
	// adapter — <app>.<base domain> — when the adapter has one: a Cloudflare
	// zone, a Traefik's configured domain. Empty leaves it to the install's
	// server.base_domain. Data, like DefaultMode, so adding an app on
	// Cloudflare names it in the zone without a second setting to keep in step.
	BaseDomain string

	SupportsTLS         bool
	SupportsWildcardTLS bool

	// RequiresPublicReachability is false for loopback, and also false for an
	// outbound-tunnel adapter such as Cloudflare, where nothing on the host
	// needs to be reachable from outside.
	RequiresPublicReachability bool
}

// BuilderCapabilities describes what a builder adapter can do.
type BuilderCapabilities struct {
	// IsolationClass is independent of the runtime's (R-114): how isolated a
	// build must be is a different question from how isolated the app must be.
	IsolationClass IsolationClass

	Strategies                []BuildStrategy
	SupportsCache             bool
	SupportsEgressRestriction bool // R-118

	// SupportsPush means the builder can push what it built to a registry,
	// with a credential it is handed per build and keeps nowhere
	// (BuildRequest.Push). A runtime that only pulls from a registry needs a
	// builder with it (RuntimeCapabilities.ImageDelivery).
	SupportsPush bool
}

// Supports reports whether the routing adapter advertises a mode.
func (c RoutingCapabilities) Supports(mode RoutingMode) bool {
	for _, m := range c.Modes {
		if m == mode {
			return true
		}
	}
	return false
}

// Supports reports whether the builder advertises a strategy.
func (c BuilderCapabilities) Supports(strategy BuildStrategy) bool {
	for _, s := range c.Strategies {
		if s == strategy {
			return true
		}
	}
	return false
}

// --- runtime ---------------------------------------------------------------

// RuntimeAdapter owns bundles, workloads, volumes, exec, logs, and capacity.
type RuntimeAdapter interface {
	Adapter
	Capabilities(ctx context.Context) (RuntimeCapabilities, error)

	// Capacity is adapter-reported, never host-inspected (R-243). The local
	// Docker adapter reports its own machine; a clustered adapter reports its
	// cluster. Core does not read /proc and has no concept of a host.
	Capacity(ctx context.Context) (Capacity, error)

	// LargestFitFor is Capacity.LargestFit as it stands for one bundle: the
	// CPU and memory of the roomiest place this bundle may run, counting what
	// the bundle already holds there as free, so a redeploy that fits where
	// it is is never refused. A runtime that keeps a bundle where it was
	// placed answers for that place only. Nil: one place, or not reported,
	// and the planner checks only Capacity's totals (R-242). The planner
	// refuses a bundle larger than this at plan time.
	LargestFitFor(ctx context.Context, bundleID string) (*Fit, error)

	// InUse reports what every workload on the runtime is using right now,
	// summed. Apart from Capacity because sampling CPU takes the runtime about
	// a second, and the planner calls Capacity on every plan without needing
	// it. Only called when ReportsUsage is true.
	InUse(ctx context.Context) (InUse, error)

	// Apply converges the bundle toward the plan. Idempotent: calling it with an
	// already-satisfied plan is a no-op.
	Apply(ctx context.Context, p BundlePlan) (BundleHandle, error)

	// Observe reports what exists. It never remediates — the reconciler decides
	// what to do (design 05). An adapter that silently restarts things makes
	// drift undetectable and breaks R-148's report path.
	Observe(ctx context.Context, ref BundleRef) (ObservedBundle, error)

	// Usage reports what each workload is using right now — CPU, memory and
	// disk — and each volume's size (R-245). A reading, not a history: Pando
	// keeps no time series (R-016). Only called when ReportsUsage is true.
	Usage(ctx context.Context, ref BundleRef) (BundleUsage, error)

	Stop(ctx context.Context, ref BundleRef) error
	Destroy(ctx context.Context, ref BundleRef, opts DestroyOptions) error

	CreateVolume(ctx context.Context, req VolumeRequest) (VolumeHandle, error)
	DestroyVolume(ctx context.Context, h VolumeHandle) error
	SnapshotVolume(ctx context.Context, h VolumeHandle, dst io.Writer) error
	RestoreVolume(ctx context.Context, h VolumeHandle, src io.Reader) error

	// ImportImage takes an image as a stream and makes it runnable, returning
	// the reference to use in a WorkloadPlan. Only called when ImageDelivery
	// includes ImageDeliveryImport.
	//
	// The reference must be immutable: the image's content-addressed ID
	// (sha256:…) rather than a tag. A deployment records it and the reconciler
	// restores from it, and a tag the next build moves would let a build that
	// was refused (by the security scan or the port check) be what a removed
	// workload is recreated from (R-146).
	ImportImage(ctx context.Context, r io.Reader) (string, error)

	Logs(ctx context.Context, ref WorkloadRef, opts LogOptions) (io.ReadCloser, error)
	Exec(ctx context.Context, ref WorkloadRef, req ExecRequest) (ExecSession, error)

	// Upstream says where Pando's proxy sends a request for one port of one
	// workload (R-023).
	//
	// The runtime answers because it is the runtime that made the workload
	// reachable: it put the workload on a private network and joined Pando to
	// it, or opened whatever path stands in for that. How a workload is
	// addressed is provider vocabulary core must never learn (R-251). The proxy
	// assembled the address itself, as a Docker container name, and any other
	// runtime would have received every request at a host that does not exist.
	//
	// Called on every proxied request. An adapter that can compute the answer
	// computes it rather than asking its provider each time.
	Upstream(ctx context.Context, ref WorkloadRef, port int) (Upstream, error)

	// Trial starts a workload in throwaway isolation and reports what it did
	// (R-097): which ports it bound, what it wrote outside its declared
	// storage, and — if it crashed — the log, which is the whole output when a
	// repository needs something it never declared (R-107).
	//
	// This is on the runtime rather than the builder, though design 03 §3 put
	// ObservedPorts and ObservedWrites on BuildResult. A builder cannot do it:
	// starting a container needs a container runtime socket, and R-112 says the
	// build path never gets one, categorically. Sequence A already had it as
	// its own step after the auction; the field placement was the error.
	//
	// Every runtime implements it, because a runtime that can Apply a bundle
	// can start one and throw it away. What varies is how much it can observe,
	// and that is declared in RuntimeCapabilities rather than discovered by
	// type assertion — a missing capability and a missing adapter must not look
	// alike.
	Trial(ctx context.Context, req TrialRequest) (TrialResult, error)

	// ApplyEdge converges an edge toward its plan: a matching edge is left
	// running, a changed plan recreates it. Only called when SupportsEdge is
	// true. A separate entry point from Apply on purpose (edge.go).
	ApplyEdge(ctx context.Context, p EdgePlan) error
	ObserveEdge(ctx context.Context, name string) (EdgeState, error)
	RemoveEdge(ctx context.Context, name string) error

	// Edges names every edge that exists, so one no routing adapter asks for
	// any more can be removed.
	Edges(ctx context.Context) ([]string, error)

	// EdgeVolumes is the storage edges own — a certificate store — for the
	// full-host backup (R-212). Each handle works with SnapshotVolume and
	// RestoreVolume, and restoring into one that does not exist yet creates it.
	EdgeVolumes(ctx context.Context) ([]VolumeHandle, error)

	// WatchBundles streams what happens to the workloads of Pando's bundles
	// to sink, for as long as ctx lives (O-52). Only called when
	// SupportsBundleEvents is true; a runtime without it returns an error at
	// once.
	//
	// It reports facts and never acts on them, as Observe does not (R-148):
	// what to do about an exited workload is the reconciler's decision.
	//
	// The first thing sent is BundleEventWatching, once the stream is open;
	// every change from then on is sent. A runtime that loses part of its
	// stream and recovers it without returning — one host of several — sends
	// BundleEventMissed after the gap. It returns when ctx ends or the stream
	// is lost; either way, anything after it returned was not seen, and the
	// caller calls it again. sink is called from one goroutine at a time and
	// must not block for long.
	WatchBundles(ctx context.Context, sink func(BundleEvent)) error
}

// BundleEventKind is what happened to a bundle's workload.
type BundleEventKind string

const (
	// BundleEventWatching: the stream is open, and every change from now on
	// is reported. Anything before it may have been missed. No BundleID.
	BundleEventWatching BundleEventKind = "watching"

	// BundleEventMissed: changes to any bundle may have gone unreported since
	// the last event — part of the stream was lost and is back. No BundleID.
	// Also sent for a change the runtime could not attribute to a bundle.
	BundleEventMissed BundleEventKind = "missed"

	BundleEventExited    BundleEventKind = "exited"
	BundleEventOOMKilled BundleEventKind = "oom_killed"
	BundleEventStarted   BundleEventKind = "started"
	BundleEventRemoved   BundleEventKind = "removed"
	BundleEventHealth    BundleEventKind = "health_changed"
)

// BundleEvent is one thing that happened to one workload.
type BundleEvent struct {
	Kind     BundleEventKind
	BundleID string
	Workload string

	// At is when the runtime says it happened. Zero when it does not say.
	At time.Time
}

// TrialRequest asks a runtime to start something once and watch it.
type TrialRequest struct {
	// TrialID names the throwaway bundle, so a trial that is interrupted leaves
	// something identifiable to clean up rather than an anonymous container.
	TrialID string

	Image      string
	Command    []string
	Entrypoint []string
	WorkingDir string
	Env        map[string]secret.Value

	// PullAuth authenticates the pull of a private image. Nil is anonymous.
	PullAuth *RegistryAuth

	// DeclaredPaths are the container paths the draft spec declares storage
	// for. Writes underneath them are expected; a write anywhere else is what
	// R-202 asks to have named in the persistence warning.
	DeclaredPaths []string

	// Timeout bounds the observation. An app that is still running when it
	// expires has passed: it started and stayed up, which is what was being
	// checked.
	Timeout        time.Duration
	IsolationFloor IsolationClass

	LogSink io.Writer
}

// TrialResult is what the runtime saw.
type TrialResult struct {
	// Started means the workload reached a running state at all. A workload
	// that never started and one that started and exited are different
	// situations: the first is usually a bad image or command, the second is
	// usually a missing dependency (R-107).
	Started bool

	// ExitCode is nil while the workload was still running when observation
	// ended — which is the healthy outcome, not a missing value.
	ExitCode *int

	// ObservedPorts carry Source "observed" into the spec, which is the
	// distinction the review UI shows (R-097): watched, not guessed.
	ObservedPorts []int

	// LoopbackPorts are ports the app bound to 127.0.0.1 rather than to an
	// address traffic can arrive on.
	//
	// Separate from ObservedPorts because routing to one would not work, and
	// reporting none at all would be misleading. An app listening only on
	// loopback is a common and quiet failure — it is a dev-server default, the
	// container reports itself healthy, and every request times out with
	// nothing in the log to say why.
	LoopbackPorts []int

	// ObservedWrites are directories written outside DeclaredPaths (R-202).
	ObservedWrites []string

	// ImageVolumes are the paths the image itself declares as persistent
	// storage (a Dockerfile's VOLUME), sorted. The image's author saying where
	// data lives is the same declaration a compose volume is (R-200).
	ImageVolumes []string

	// Log is captured whether or not the trial crashed, because on a crash it
	// is the entire answer Pando has and R-107 says showing it and stopping is
	// the correct outcome rather than a gap to close with inference.
	Log string
}

// BundleRef identifies an app's bundle to an adapter.
type BundleRef struct {
	BundleID string
}

// WorkloadRef identifies one workload within a bundle.
type WorkloadRef struct {
	BundleID string
	Workload string
}

// Upstream is how Pando's proxy reaches a workload.
//
// A struct rather than a string so that a runtime whose workloads are not
// directly addressable — a remote host reached through a tunnel it opens — can
// answer here later without the interface changing again.
type Upstream struct {
	// URL is the scheme, host and port the proxy forwards to, such as
	// "http://pando-app_01HQ8-web:3000". No path: the proxy keeps the request's.
	URL string

	// Dial opens the connection the proxy sends a request over. Nil: the proxy
	// dials URL's host itself, as on one Docker host.
	//
	// Set by a runtime whose workloads are not on a network Pando's container
	// is joined to — the multi-host Docker adapter, whose Dial connects to the
	// forwarding agent on the app's host (O-45, design 06 §4). The proxy has
	// already decided the request by then; Dial is transport only. Supplied by
	// the runtime so the agent's protocol stays the adapter's vocabulary
	// (R-251).
	Dial func(ctx context.Context) (net.Conn, error)

	// PoolKey groups connections the proxy may reuse: one key, one
	// destination, so a connection opened by one Dial is only reused for a
	// request this Upstream would have dialed the same way. Empty is URL's
	// host. Only read when Dial is set.
	PoolKey string
}

// BundleHandle is the adapter's own identifier for a bundle.
type BundleHandle struct {
	BundleID string
	Handle   string
}

// VolumeHandle is the adapter's own identifier for a volume.
type VolumeHandle struct {
	VolumeID string
	Handle   string
}

// VolumeRequest asks for storage.
type VolumeRequest struct {
	VolumeID  string
	BundleID  string
	Name      string
	SizeBytes int64
}

// DestroyOptions controls teardown.
type DestroyOptions struct {
	// KeepVolumes is the default posture. Volumes outlive the apps that mount
	// them (R-204), and destroying them is a separate, explicit act.
	KeepVolumes bool
}

// LogOptions controls a log stream.
type LogOptions struct {
	Follow bool
	Since  time.Time
	Tail   int
}

// BundlePlan is a fully-resolved description of what should exist.
type BundlePlan struct {
	// BundleID is stable across deploys of one app.
	BundleID  string
	Workloads []WorkloadPlan
	Volumes   []VolumePlan
	Network   NetworkPlan
	Labels    map[string]string

	// FirstDeploy is true when the app has never had a successful deploy, so
	// nothing of it can be anywhere a runtime cannot see right now. A runtime
	// that places bundles may then place it while part of itself does not
	// answer; for any other bundle it must not, because a copy might already
	// run in the part that is silent (O-46). False is the safe value, and
	// what the reconciler sends.
	FirstDeploy bool
}

// WorkloadPlan is one workload, fully resolved.
type WorkloadPlan struct {
	Name       string
	Image      string
	Command    []string
	Entrypoint []string
	WorkingDir string

	// Env arrives fully resolved: slots filled, secrets injected. Adapters
	// never see a slot, never talk to the secrets adapter, and never learn that
	// a value was sensitive. That keeps R-027's boundary intact and the
	// interface small.
	Env map[string]secret.Value

	// PullAuth authenticates the pull of Image when it is private. Nil is an
	// anonymous pull. Never placed in the workload: it is how the runtime
	// fetches the image, not something the app is given.
	PullAuth *RegistryAuth

	Mounts    []MountPlan
	Files     []FilePlan
	Ports     []PortPlan
	DependsOn []string
	Health    *HealthPlan
	Resources ResourcePlan

	// LogBytes caps this workload's logs (R-222, R-223). Zero means the
	// runtime's own default, which is usually unbounded — so core always sets
	// it from the spec rather than leaving it to chance.
	LogBytes int64
	Exposed  bool
}

// MountPlan attaches a volume.
type MountPlan struct {
	VolumeID string
	Path     string
	ReadOnly bool
}

// FilePlan is a configuration file to place in the workload before it starts.
//
// Carried in the spec and replayed here rather than fetched from anywhere: the
// spec is the sole record of how an app runs (R-020). A runtime that cannot
// place a file says so through its capabilities, and the planner refuses before
// anything is created (R-254) — a workload started without its configuration is
// the failure that looks like success.
type FilePlan struct {
	Path    string
	Content string
	Mode    int
}

// PortPlan is a port to expose within the bundle network.
type PortPlan struct {
	Number   int
	Protocol string
}

// VolumePlan is a volume the bundle needs.
type VolumePlan struct {
	VolumeID string
	Name     string
}

// HealthPlan is how to check a workload.
type HealthPlan struct {
	Command         []string
	Path            string
	Port            int
	IntervalSeconds int
	TimeoutSeconds  int
	Retries         int
}

// ResourcePlan caps a workload.
type ResourcePlan struct {
	CPUMillis   int
	MemoryBytes int64
}

// NetworkPlan describes the bundle's network.
type NetworkPlan struct {
	// Private is always true (R-026). It is a field rather than an assumption
	// so an adapter that cannot provide a private network fails loudly at the
	// capability check instead of silently placing workloads on a shared one.
	Private bool

	// Egress is the rules the app's workloads run with, already merged from
	// the installation's and the app's (R-182). A runtime enforces every
	// layer, or says it cannot (SupportsEgressRestriction) and is refused at
	// plan time (R-186). When Egress.Restricted() is false nothing may be put
	// in the app's path: an app with no restriction runs exactly as it would
	// with no egress controls at all.
	Egress EgressRules
}

// EgressRules is what decides whether a workload's connection may leave.
type EgressRules = egress.Rules

// ObservedBundle is what actually exists.
type ObservedBundle struct {
	Exists    bool
	Workloads []ObservedWorkload
	Volumes   []ObservedVolume
}

// ObservedWorkload is a workload as found.
//
// Carries no environment, deliberately. Reading back resolved environment would
// require every runtime adapter to handle secret-bearing data, which is exactly
// what WorkloadPlan.Env's one-way flow avoids. Stale-environment drift is
// detected state-side by fingerprint instead (design 02 §2.4).
type ObservedWorkload struct {
	Name         string
	Present      bool
	Running      bool
	ImageDigest  string
	StartedAt    time.Time
	ExitCode     *int
	RestartCount int

	// Healthy is a pointer because "no health signal" and "unhealthy" are
	// different states and must not collapse (R-221). An app with no health
	// check is running, not perpetually degraded.
	Healthy *bool

	// Restarting means the workload is in a restart loop right now.
	//
	// Separate from Running because a runtime can report both at once — Docker
	// does, and a crash-looping container inspects as Running=true,
	// Restarting=true. Without this the reconciler reads a looping app as
	// running and healthy, clears its failure count, and the app never reaches
	// `failed`: every glimpse of it up undoes the progress toward giving up on
	// it. Design 05 §1.1 has always said degraded means "health failing **or**
	// restarting"; this is the signal that carries the second half.
	//
	// A runtime that cannot tell reports false, and the reconciler falls back
	// to noticing that a workload which has restarted before started again
	// moments ago.
	Restarting bool
}

// ObservedVolume is a volume as found.
type ObservedVolume struct {
	VolumeID string
	Present  bool
	Handle   string
}

// LogRetentionCapability is how a runtime can bound log growth (R-222–R-224).
//
// Capabilities as data (R-254), and this is a case where the honest answer is
// "partly". Docker can cap a container's log at creation and cannot change it
// afterwards without recreating the container — and the reconciler may not
// recreate a container because an unrelated app turned chatty, which is
// destruction on a schedule. An adapter that says so lets the planner enforce
// what it can and say plainly what it cannot, instead of Pando promising a
// guarantee nothing delivers.
//
// That was O-16. The decision recorded here is: cap per app at creation, and
// enforce R-224's aggregate as a **plan-time bound on the sum of caps** rather
// than as an observation of usage. Bounding what is committed is stronger than
// watching what accumulates — if every app's logs are capped and the caps sum
// under the budget, the total cannot exceed it — and it needs nothing from the
// runtime beyond the cap it already applies.
type LogRetentionCapability struct {
	// SupportsSizeCap means the runtime applies a per-workload byte cap when
	// the workload is created (R-222). False means logs are unbounded, and the
	// planner says so rather than pretending.
	SupportsSizeCap bool

	// CanChangeWithoutRecreate means a cap can be changed on a running
	// workload. False on Docker. When false, a changed cap takes effect on the
	// next deploy — which the console must say, because a setting that appears
	// to apply and does not is worse than one that says "next deploy".
	CanChangeWithoutRecreate bool

	// ReportsUsage means the runtime can say how much log space an app is
	// actually using. False on Docker, which is why R-224 is enforced against
	// committed caps rather than measured bytes.
	ReportsUsage bool

	// MinBytes is the smallest cap the runtime will honor, 0 for none. Docker
	// rounds; a runtime with a hard floor says so here so the planner can
	// refuse a spec asking for less rather than silently granting more.
	MinBytes int64
}

// BundleUsage is one reading of what an app's workloads are using (R-245).
type BundleUsage struct {
	Workloads []WorkloadUsage
	Volumes   []VolumeUsage
	Reported  time.Time
}

// WorkloadUsage is one workload's use, beside its limits. A limit of 0 means
// none: the workload may use what the host has.
type WorkloadUsage struct {
	Workload string
	Running  bool

	// CPUMillis is thousandths of a core, averaged over the adapter's sample.
	CPUMillis      int
	CPULimitMillis int

	MemoryBytes      int64
	MemoryLimitBytes int64

	// DiskBytes is what the workload has written outside its volumes, -1 when
	// the runtime cannot say.
	DiskBytes int64
}

// VolumeUsage is how much a volume holds, -1 when the runtime cannot say.
type VolumeUsage struct {
	VolumeID string
	Bytes    int64
}

// Capacity is what the adapter reports about itself (R-243): the readings
// every runtime gives in the same shape, so the console and the planner can
// read them without knowing which runtime answered, and Details for the rest.
//
// A total of 0 means the runtime does not know it. The planner then does not
// check that resource, and the console says it is not reported rather than
// drawing an empty meter.
type Capacity struct {
	TotalCPUMillis   int
	TotalMemoryBytes int64
	TotalDiskBytes   int64

	// RunningWorkloads is how many workloads are running now, Pando's or
	// not; -1 when the runtime cannot say.
	RunningWorkloads int

	// LargestFit is the CPU and memory of the roomiest single place a new
	// workload could go now, by committed limits. On a runtime spread over
	// several machines the total can have room that no one machine has. Nil
	// when the runtime is one place, where the totals already say it
	// (notes-multi-host-docker-issue-72.md).
	LargestFit *Fit

	// Details is anything else the runtime reports about itself, in its own
	// shape — version, storage driver. Shown as it is, never interpreted:
	// Pando does not own its schema.
	Details map[string]any

	Reported time.Time
}

// Fit is an amount of CPU and memory one place has free.
type Fit struct {
	CPUMillis   int
	MemoryBytes int64
}

// InUse is what a runtime's workloads are using now, summed (R-245).
type InUse struct {
	CPUMillis   int
	MemoryBytes int64
	Reported    time.Time
}

// ExecRequest opens a session in a workload.
type ExecRequest struct {
	Command []string
	TTY     bool
	Env     map[string]string
}

// ExecSession is an open exec session.
type ExecSession interface {
	io.ReadWriteCloser
	Resize(rows, cols uint16) error
	ExitCode() (int, bool)
}

// --- routing ---------------------------------------------------------------

// RoutingAdapter makes traffic arrive at Pando's proxy.
//
// It never routes to the workload (design 00 §1.3). Every method describes
// intent rather than mechanism, which is why the interface survived being
// sketched against an outbound-tunnel provider as well as a local reverse proxy
// — see notes-cloudflare-routing-sketch.md. Adding a Reload() or a ConfigPath
// here would undo that.
type RoutingAdapter interface {
	Adapter
	Capabilities(ctx context.Context) (RoutingCapabilities, error)

	Ensure(ctx context.Context, r RouteRequest) (RouteHandle, error)
	Remove(ctx context.Context, h RouteHandle) error
	Observe(ctx context.Context, h RouteHandle) (RouteState, error)

	// Edge describes the workload this adapter needs running in front of
	// Pando, if any (R-174, edge.go). false means none — loopback, or a
	// Traefik somebody else runs. An adapter may prepare files it owns here,
	// such as the route that sends every other hostname to the console.
	Edge(ctx context.Context, r EdgeRequest) (EdgePlan, bool, error)
}

// RouteRequest tells an adapter where to send traffic.
type RouteRequest struct {
	AppID      string
	Mode       RoutingMode
	Hostname   string
	PathPrefix string
	Port       int

	// ProxyUpstream is where the adapter must send traffic, and it is always
	// Pando's proxy (R-023). It is in the request rather than discovered by the
	// adapter so the contract is explicit: an adapter is told where to point.
	//
	// The address must be reachable from wherever the adapter's data plane runs
	// — which is not necessarily where Pando runs. A tunnel daemon in another
	// container dials it from there.
	ProxyUpstream string

	TLS TLSRequest
}

// TLSRequest is advisory.
//
// An adapter may satisfy it however it likes, or ignore it because its edge
// already terminates TLS. It is an intent, not a set of instructions — issuance
// is a per-adapter concern (O-5).
type TLSRequest struct {
	Enabled  bool
	Hostname string
}

// RouteHandle is the adapter's own identifier for a route.
type RouteHandle struct {
	AppID  string
	Handle string
}

// RouteState is a route as found.
type RouteState struct {
	Present bool
	Address string
}

// --- builder ---------------------------------------------------------------

// BuilderAdapter turns source into a runnable image.
type BuilderAdapter interface {
	Adapter
	Capabilities(ctx context.Context) (BuilderCapabilities, error)

	// Bid inspects the source and returns a confidence score plus a draft spec
	// fragment. Part of the detector auction (R-093).
	Bid(ctx context.Context, src SourceView) (Bid, error)

	Build(ctx context.Context, req BuildRequest) (BuildResult, error)

	// Forget removes what the builder keeps for an app between builds — its
	// build cache — once the app is deleted. The namespace is the app's ID,
	// the prefix of every CacheNamespace it built under. Idempotent: an app
	// that never built, or was already forgotten, is not an error.
	Forget(ctx context.Context, namespace string) error
}

// SourceView is a read-only view of an app's source (R-020).
//
// It has no write methods, structurally. Pando looks at the source and never
// asks it for permission.
type SourceView interface {
	Open(name string) (io.ReadCloser, error)
	Stat(name string) (FileInfo, error)
	Glob(pattern string) ([]string, error)
}

// FileInfo is what SourceView reports about a path.
type FileInfo struct {
	Name  string
	Size  int64
	IsDir bool
}

// Bid is a builder's claim on a source tree.
type Bid struct {
	Confidence float64
	Strategy   BuildStrategy

	// Evidence is human-readable and shown in the review UI, so the user can
	// see the auction rather than being handed a verdict (R-102).
	Evidence []string

	// Questions are what the bidder could not determine (R-102).
	Questions []Question
}

// QuestionKind shapes the answer control in the console.
type QuestionKind string

const (
	QuestionChoice QuestionKind = "choice"
	QuestionText   QuestionKind = "text"
	QuestionPort   QuestionKind = "port"
	QuestionPath   QuestionKind = "path"
)

// Question is something detection could not determine.
//
// Prompt carries a hard content requirement from R-105: it must be answerable
// by a model that cannot see the repo, because the expected workflow is pasting
// it into the assistant that wrote the app. "Which port?" fails review.
type Question struct {
	Key     string
	Prompt  string
	Why     string
	Kind    QuestionKind
	Options []string
}

// BuildRequest asks for an image.
type BuildRequest struct {
	Source     SourceView
	Strategy   BuildStrategy
	Dockerfile string
	Context    string
	Args       map[string]string

	// Target is the stage of a multi-stage Dockerfile to build. Empty builds
	// the last stage, which is what a Dockerfile with no target means.
	Target string

	// GeneratedFiles are build inputs the spec carries, keyed by path relative
	// to the context. Written into the checkout before the build.
	//
	// Present means "use these" — the builder does not plan again. That is what
	// makes a reviewed plan the one that runs, and an edited one take effect.
	GeneratedFiles map[string]string

	// StaticDir is the directory to serve, for the static strategy.
	//
	// What "serving" means is the builder's business, not core's. Core says
	// "this app is a directory of files"; the builder decides what image serves
	// them (R-250, R-251). Empty means the repository root.
	StaticDir string

	// StartCommand is how the app starts, when the spec says so: a shell
	// command line. A builder that plans a build from convention uses it in
	// place of guessing one, and a builder that has no use for it ignores it.
	//
	// It exists because detection asks for the start command when it cannot
	// work one out, and the answer used to stop at the workload. A buildpack
	// plan made at build time then failed with "No start command could be
	// found" on exactly the apps whose owners had just typed one (issue #55).
	StartCommand string

	IsolationFloor IsolationClass
	Timeout        time.Duration
	EgressMode     EgressMode
	EgressAllow    []string

	// CacheNamespace is per-app (R-117), so one app's build cache cannot be
	// read by another's build.
	CacheNamespace string

	// LogSink streams build output to the console live.
	LogSink io.Writer

	// ImageSink receives the built image as a stream.
	//
	// R-111: Pando hands the builder source and receives an image. Writing it
	// to a sink rather than returning it means the image never has to be held
	// in memory or staged on disk — the caller pipes it straight into the
	// runtime's ImportImage.
	//
	// Exactly one of ImageSink and Push is set.
	ImageSink io.Writer

	// Push sends the image to a registry instead, for a runtime that pulls
	// (RuntimeCapabilities.ImageDelivery). Only set for a builder with
	// SupportsPush. BuildResult.Digest is then the pushed manifest's digest
	// and BuildResult.ImageRef is Repository@Digest.
	Push *PushTarget

	// Tag names this build's image, unique to the build: core passes the
	// deployment ID. Never a tag the next build moves, so an image refused
	// after it was built is never mistaken for the one that runs (R-146).
	// Empty lets the builder choose.
	Tag string
}

// PushTarget is where a build pushes its image (issue #72, PR 5).
type PushTarget struct {
	// Repository is the registry host and path, with no tag:
	// registry.internal:5000/pando/apps/app_01hq8.
	Repository string

	// Tag is for people reading the registry. Nothing Pando runs refers to
	// it: what runs is Repository@digest.
	Tag string

	// Auth is the push credential, for this build only. The builder keeps it
	// nowhere — not in its configuration and not on disk (R-194).
	Auth *RegistryAuth

	// Insecure permits plain HTTP to the registry. Only set when the operator
	// said so in the image registry adapter's settings (O-35).
	Insecure bool
}

// ImageLabelBundle is the image label a builder sets to the app an image was
// built for, and a runtime reads to remove an app's images when it is
// destroyed. Its value is the BundleID. An image label rather than a tag,
// because a rebuild moves the tag and leaves the previous image untagged.
//
// Its own key, not the one a runtime marks a bundle's containers with: a
// container inherits its image's labels, and a throwaway trial container
// started from the image must not look like one of the app's workloads.
const ImageLabelBundle = "io.pando.built-for"

// BuildResult is what a build produced.
//
// It carries no trial-run output, though design 03 §3 put ObservedPorts and
// ObservedWrites here. Producing them means starting a container, starting a
// container means a container runtime socket, and R-112 forbids the build path
// ever having one — categorically, with an integration test asserting the build
// container's mount list. The trial run is RuntimeAdapter.Trial instead, called
// as its own step, which is what Sequence A described all along.
type BuildResult struct {
	ImageRef string
	Digest   string
}

// --- secrets ---------------------------------------------------------------

// SecretsAdapter stores secret values.
type SecretsAdapter interface {
	Adapter
	Put(ctx context.Context, ref SecretRef, v secret.Value) (StoredRef, error)
	Get(ctx context.Context, ref StoredRef) (secret.Value, error)
	Delete(ctx context.Context, ref StoredRef) error
}

// SecretRef names a secret within an app.
type SecretRef struct {
	AppID string
	Key   string
}

// StoredRef is the adapter's own pointer to a stored secret.
type StoredRef struct {
	AppID      string
	Key        string
	Handle     string
	Ciphertext []byte
}

// --- services --------------------------------------------------------------

// ServicesAdapter fills provisioned slots (R-131).
//
// Provision must be pure and cheap. It is called on every deploy — the
// workloads it returns *are* the service, so a deploy that skipped it would
// produce a bundle with no database in it — and again on every reconcile, to
// work out what should be running. An implementation that talks to a provider
// here would be talking to it every fifteen seconds for every app.
//
// The interface is shaped for that: Provision returns plans, and the runtime
// adapter is what creates anything.
type ServicesAdapter interface {
	Adapter
	Capabilities() ServicesCapabilities
	Supports() []spec.SlotType
	Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)
	Destroy(ctx context.Context, h ServiceHandle) error
	Snapshot(ctx context.Context, h ServiceHandle, dst io.Writer) error
	Restore(ctx context.Context, h ServiceHandle, src io.Reader) error
}

// ServicesCapabilities is what a services adapter can do, as data (R-254).
type ServicesCapabilities struct {
	// DataInAppVolumes says the service's data lives in volumes returned from
	// Provision, which Pando owns and already backs up.
	//
	// This is the difference between a service that runs inside the app's
	// bundle and one that lives somewhere Pando can only reach over the wire.
	// It decides how a DR bundle captures the service (R-212): a true here
	// means the volume snapshots already contain it and calling Snapshot would
	// copy the same bytes twice; a false means Snapshot is the only way to get
	// the data, and a bundle taken without calling it silently omits every
	// provisioned database in the install.
	//
	// Data, not a type assertion, so the backup path can state in its manifest
	// which services it captured and how.
	DataInAppVolumes bool
}

// ProvisionRequest asks for a service instance.
type ProvisionRequest struct {
	AppID    string
	BundleID string
	SlotKey  string
	Type     spec.SlotType

	// ServiceID is Pando's identifier for this instance, minted by core before
	// the adapter is called.
	//
	// Supplied rather than returned so provisioning is idempotent: a deploy
	// that fails after the adapter answered and before core stored the row can
	// call again with the same ID and get the same names back, instead of
	// leaving an orphaned database nobody references.
	ServiceID string

	// ExistingSecret is the connection string a previous Provision returned for
	// this same service, empty the first time.
	//
	// Every deploy calls Provision, because the workloads it returns are what
	// keeps the service running. So every deploy after the first must produce
	// the *same* credentials: a database sets its password once, when its data
	// directory is created, and ignores the variable forever after. An adapter
	// that generated a fresh password on each call would hand the app a
	// password the database has never heard of, and the symptom — an app that
	// deployed successfully and cannot authenticate — points nowhere near the
	// cause.
	ExistingSecret secret.Value
}

// ServiceHandle is the adapter's own identifier for a provisioned service.
type ServiceHandle struct {
	ServiceID string
	Handle    string
}

// ProvisionResult is a provisioned service.
type ProvisionResult struct {
	Handle ServiceHandle

	// ConnectionSecret is the URL or DSN, stored as a secret (R-131).
	ConnectionSecret secret.Value

	// Workloads join the app's private bundle. A provisioned service is not
	// exposed, not addressable from outside, and not shareable with another app
	// (R-134) — sharing is two apps binding to one external target.
	Workloads []WorkloadPlan

	// Volumes the workloads mount. [P], added because a database workload with
	// nowhere to put its files is a database that loses everything on the next
	// deploy, and because R-135 says a provisioned service's data follows the
	// app's volume rules — which it can only do if it is in a Pando volume.
	//
	// The volumes are the app's, recorded and backed up like any other. That
	// is what makes R-135 true rather than aspirational.
	Volumes []VolumePlan
}

// --- notification ----------------------------------------------------------

// NotificationKind is why a notification fired.
type NotificationKind string

const (
	NotifyAppFailed       NotificationKind = "app_failed"
	NotifyDeployFailed    NotificationKind = "deploy_failed"
	NotifyPolicyViolation NotificationKind = "policy_violation"
	NotifyBackupFailed    NotificationKind = "backup_failed"

	// NotifyDeployApproval: a deploy is waiting for somebody's approval, or
	// a request somebody made was answered (R-159).
	NotifyDeployApproval NotificationKind = "deploy_approval"
	// NotifyUpdateAvailable: a newer Pando is released (R-362), once per
	// version. NotifyUpgradeFailed: an in-place upgrade did not finish and
	// the previous version was put back, or could not be (R-359).
	NotifyUpdateAvailable NotificationKind = "update_available"
	NotifyUpgradeFailed   NotificationKind = "upgrade_failed"

	// NotifyAppShared: somebody was given use of an app (R-266). Off by
	// default on every channel — the launcher tile is the notification — and
	// a person may turn it on for a channel that reaches them elsewhere.
	NotifyAppShared NotificationKind = "app_shared"
	// NotifySubscriptionDisabled: Pando turned one of your event
	// subscriptions off because its endpoint kept failing (R-370).
	NotifySubscriptionDisabled NotificationKind = "subscription_disabled"
	// NotifyAuditSinkDisabled: Pando turned off an audit sink because it kept
	// failing (R-383). Sent to everybody holding install.audit.export.
	NotifyAuditSinkDisabled NotificationKind = "audit_sink_disabled"
	// NotifyAppIdle: an app nobody uses is about to be stopped or deleted,
	// or was (R-395). Sent to its owner.
	NotifyAppIdle NotificationKind = "app_idle"
)

// NotifyAdapter delivers notifications.
type NotifyAdapter interface {
	Adapter
	Capabilities() NotifyCapabilities
	Notify(ctx context.Context, n Notification) error
}

// NotifyAudience is who a notify adapter's messages reach.
type NotifyAudience string

const (
	// AudiencePeople: the adapter reaches the recipients named on each
	// notification — the console, an email. Pando's own notifications go to
	// these, as each person's preferences allow (R-373).
	AudiencePeople NotifyAudience = "people"
	// AudienceChannel: the adapter posts to one place its configuration
	// names — a Slack channel, a Teams channel, an ntfy topic — and ignores
	// recipients. Only subscriptions send to these, because a message meant
	// for one person must not land in a room (R-373).
	AudienceChannel NotifyAudience = "channel"
)

// NotifyCapabilities is what a notify adapter can do, as data (R-254).
type NotifyCapabilities struct {
	Audience NotifyAudience
}

// Recipient is who to tell.
type Recipient struct {
	UserID string
	Email  string
}

// Notification is one message.
type Notification struct {
	// Kind is one of the constants above, or, for a message a subscription
	// sent, the name of the event it is about (R-374).
	Kind       NotificationKind
	AppID      string
	Recipients []Recipient
	Subject    string
	Body       string

	// EventID is set when the notification tells of an event
	// (evt_…), so a receiver can deduplicate a delivery made twice.
	EventID string
	// Fields are the event's data, in order, for an adapter that lays them
	// out — a Slack block, a Teams fact set. Never a secret (R-194).
	Fields []NotificationField
	// Link is where in the console to look, when Pando knows its own address.
	Link string
}

// NotificationField is one labeled value.
type NotificationField struct {
	Label string
	Value string
}

// PlanDeclaration is what in a repository dictated its build plan.
//
// R-094 ranks detection evidence, and the top of that ladder is the app's
// author saying something rather than Pando inferring it. A builder that
// planned from a Makefile target, a CI workflow, or a pair of declarations
// naming one directory knows which it was. Detection wants to say so, and
// cannot work it out for itself without a second implementation of the same
// reading — which is the duplication R-027's boundary already prevents by
// keeping the two packages apart.
//
// Nil means convention-matching chose the plan, which is the bottom rung of the
// ladder and the usual case.
//
// A definition rather than a mechanism, which is what belongs in this package:
// the builder fills it in, core reads it, and neither learns the other's
// vocabulary (R-251).
type PlanDeclaration struct {
	// Source is the file that said it, relative to the repository root.
	Source string

	// Why explains the reading in one line. It is shown to a person as
	// evidence, and is held to the same standard as everything else they read.
	Why string

	// Confidence is where this reading sits on R-094's ladder: the bid a
	// detector should make when the plan came from it.
	Confidence float64
}
