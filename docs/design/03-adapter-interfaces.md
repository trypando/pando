# 03 — Adapter Interfaces

Package `internal/adapter/api`. Definitions only, no implementations.

R-250: the app declares requirements; adapters translate. R-251: core never learns a provider's vocabulary. These interfaces are where that promise is either kept or broken — if a Docker-shaped concept appears in a signature below, the design has failed.

R-253: everything is compiled in-tree. These are Go interfaces, freely refactorable. There is no wire protocol and none is planned.

---

## 1. Common types

```go
package api

type Category string

const (
    CategoryIdentity Category = "identity"
    CategoryRouting  Category = "routing"
    CategoryBuilder  Category = "builder"
    CategoryRuntime  Category = "runtime"
    CategorySecrets  Category = "secrets"
    CategoryServices Category = "services"
    CategoryNotify   Category = "notify"
)

type IsolationClass int   // ordered, comparable — policy floors depend on this

const (
    IsolationContainer     IsolationClass = 10  // shared kernel
    IsolationSandboxed     IsolationClass = 20  // gVisor, Kata
    IsolationVM            IsolationClass = 30  // Firecracker, Incus
    IsolationDedicatedHost IsolationClass = 40
)

// Adapter is implemented by every adapter in every category.
type Adapter interface {
    Kind() string
    Category() Category
    Configure(ctx context.Context, raw json.RawMessage) error
    HealthCheck(ctx context.Context) error
}
```

**[D]** `IsolationClass` is an ordered integer, not a string enum, because R-114 and R-255 require a policy floor comparison. Gaps of 10 leave room to insert classes without a migration.

**[D]** Every adapter implements `HealthCheck`. The planner refuses to plan against an unhealthy adapter and returns `ADAPTER_UNAVAILABLE` rather than failing mid-deploy (R-254).

### 1.1 Capabilities as data

**[D]** R-254: capabilities are a returned struct, never a Go type assertion. Type assertions are invisible to the planner and cannot produce a readable plan-time error.

```go
type RuntimeCapabilities struct {
    IsolationClass          IsolationClass
    SupportsPersistentVolumes bool
    SupportsExec            bool
    SupportsMultipleWorkloads bool
    SupportsPrivateNetwork  bool   // required for R-026; an adapter without it is unusable
    SupportsResourceLimits  bool
    SupportsStartThenSwap   bool   // R-145
    ReportsUsage            bool   // R-245
    MaxWorkloadsPerBundle   int    // 0 = unlimited
    SupportsEgressRestriction bool // enforces NetworkPlan.Egress (R-186)
    EdgeConfig              []EdgeConfig    // how an edge here receives routes: "shared_mount" | "kubernetes_api"
    ImageDelivery           []ImageDelivery // how a built image reaches it: import | registry
}

type RoutingCapabilities struct {
    Modes             []RoutingMode  // subdomain | path | port
    DefaultMode       RoutingMode    // R-162
    SupportsWildcardTLS bool
    SupportsTLS       bool
    RequiresPublicReachability bool  // false for loopback
}

type BuilderCapabilities struct {
    IsolationClass  IsolationClass  // R-114, independent of runtime
    Strategies      []BuildStrategy // dockerfile | compose | buildpack | static | prebuilt
    SupportsCache   bool
    SupportsEgressRestriction bool  // R-118
    SupportsPush    bool            // can push to a registry with a per-build credential
}
```

**[D] Image delivery (issue #72, PR 5).** `ImageDelivery` replaces the single `SupportsImageImport`
bool. It lists, in the runtime's order of preference, how an image Pando built can reach it:
`import` (the runtime takes the image as a stream, `ImportImage`) or `registry` (the builder pushes to
the install's registry and the runtime pulls by digest, with `WorkloadPlan.PullAuth`). Single-host
Docker reports `[import, registry]`; a runtime spanning machines reports `[registry]`; empty means it
runs only published images. The planner decides from data (`planner.ChooseDelivery`, plan step 5a):
import when the runtime takes it and the install does not send every build through its registry
(`PANDO_REGISTRY_ALWAYS`); otherwise the registry when the runtime pulls, one is configured
(`PANDO_REGISTRY_URL`) and the builder has `SupportsPush`; otherwise `PLAN_CAPABILITY_UNSUPPORTED`
naming what is missing. The deploy asks again before building. `BuildRequest` carries exactly one of
`ImageSink` and `Push`, and a `Tag` (the deployment ID) so no build is ever named by a tag the next
build moves (R-146). `ImportImage` returns the loaded image's content-addressed ID; a push returns
`repository@digest`. Either is what the deployment records and the reconciler restores.
`notes-image-registry-issue-72.md` has the rest.

---

**[P] `ImageDelivery` and `EdgeConfig` (issue #72).** `ImageDelivery` is how a built image can reach the
runtime: `import` streams it into `ImportImage` (single-host Docker), `registry` has the builder push it
and every node pull it by digest (Kubernetes; `notes-image-registry-issue-72.md`, PR 5, wires the push).
`EdgeConfig` is how an edge on the runtime can receive routes; core passes the default runtime's list to
the routing adapter in `EdgeRequest`, and a routing adapter whose delivery the runtime does not offer
refuses its edge with a readable error rather than running one that never learns a route (§4.4).

## 2. Runtime

The largest interface. Owns bundles, workloads, volumes, exec, logs, and capacity.

```go
type RuntimeAdapter interface {
    Adapter
    Capabilities(ctx context.Context) (RuntimeCapabilities, error)

    // Capacity is adapter-reported, never host-inspected (R-243).
    Capacity(ctx context.Context) (Capacity, error)

    // What every workload on the runtime is using now, summed (R-245). Apart
    // from Capacity because it samples, and the planner calls Capacity on
    // every plan. Only when ReportsUsage.
    InUse(ctx context.Context) (InUse, error)

    // Apply converges the named bundle toward the plan. Idempotent:
    // calling it with an already-satisfied plan must be a no-op.
    Apply(ctx context.Context, p BundlePlan) (BundleHandle, error)

    // Observe reports what actually exists. Drives reconciliation (R-148).
    Observe(ctx context.Context, ref BundleRef) (ObservedBundle, error)

    // What each workload is using now — CPU, memory, disk — and each volume's
    // size (R-245). A reading, never a history (R-016). Only when ReportsUsage.
    Usage(ctx context.Context, ref BundleRef) (BundleUsage, error)

    Stop(ctx context.Context, ref BundleRef) error
    Destroy(ctx context.Context, ref BundleRef, opts DestroyOptions) error

    CreateVolume(ctx context.Context, req VolumeRequest) (VolumeHandle, error)
    DestroyVolume(ctx context.Context, h VolumeHandle) error
    SnapshotVolume(ctx context.Context, h VolumeHandle, dst io.Writer) error
    RestoreVolume(ctx context.Context, h VolumeHandle, src io.Reader) error

    Logs(ctx context.Context, ref WorkloadRef, opts LogOptions) (io.ReadCloser, error)
    Exec(ctx context.Context, ref WorkloadRef, req ExecRequest) (ExecSession, error)

    // Where Pando's proxy sends a request for one port of one workload (R-023).
    // Called on every proxied request.
    Upstream(ctx context.Context, ref WorkloadRef, port int) (Upstream, error)
}

type Upstream struct {
    URL string // scheme, host and port; the proxy keeps the request's path

    // Dial opens the connection the proxy sends the request over. Nil: the
    // proxy dials URL's host. PoolKey groups reusable connections: one key,
    // one destination. Empty is URL's host.
    Dial    func(ctx context.Context) (net.Conn, error)
    PoolKey string
}
```

**[D]** The runtime says where a workload is reachable; the proxy never assembles the address. How a
workload is addressed is provider vocabulary (R-251), and it is the runtime that made the workload
reachable — by joining Pando to the bundle network, or by whatever stands in for that. Core still
chooses *which* port (the primary workload's HTTP port), because that is a reading of the spec. The
proxy built a Docker container name itself until this was moved, which would have sent every request
for an app on any other runtime to a host that does not exist.

**[D]** `Upstream.Dial` is for a runtime whose workloads are not on a network Pando's container is
joined to. The multi-host Docker adapter's `Dial` opens a mutually authenticated connection to the
forwarding agent on the app's host, which carries it to the container (O-45, design 06 §4,
`notes-multi-host-docker-issue-72.md`). The proxy decides the request — steps 1–10 of design 06 §4,
including the header and cookie strips — before it dials, so `Dial` is transport and nothing else.
The proxy pools a dialed upstream's connections under `PoolKey` on a transport of their own, with no
environment HTTP proxy. A runtime whose workloads Pando's container can reach leaves both empty.

### 2.1 The plan

**[D]** `BundlePlan.FirstDeploy` is true when the app has never had a successful deploy, which core
reads from its deployments (`RunningSpecID` empty). Nothing of such an app can be anywhere a runtime
cannot see, so a runtime that places bundles may place it while part of itself does not answer. For
any other bundle it must not: a copy may already run in the silent part, and placing it again would
be a second copy or a move (O-46). False is the safe value and what the reconciler sends.

```go
type BundlePlan struct {
    BundleID   string          // stable across deploys of one app
    Workloads  []WorkloadPlan
    Volumes    []VolumePlan
    Network    NetworkPlan
    Labels     map[string]string
}

type WorkloadPlan struct {
    Name       string
    Image      string
    Command    []string
    Entrypoint []string
    WorkingDir string
    Env        map[string]string  // fully resolved. Slots filled, secrets injected.
    PullAuth   *RegistryAuth      // the pull of Image, when private. Never in the workload.
    Mounts     []MountPlan
    Ports      []PortPlan
    DependsOn  []string
    Health     *HealthPlan
    Resources  ResourcePlan
    Exposed    bool
}

type NetworkPlan struct {
    Private bool        // always true (R-026)
    Egress  EgressRules // = egress.Rules, merged from the install's and the app's (R-182)
}

// internal/egress
type Rules struct {
    Layers       []Layer // every layer must allow a destination
    BlockPrivate bool
}
type Layer struct {
    Mode Mode     // allowlist | denylist
    List []string // entries (R-185)
    From string   // install | app — named in a refusal
}
```

**[D]** `Env` arrives fully resolved. Adapters never see a slot, never talk to the secrets adapter, and never learn that a value was sensitive. That keeps R-027's identity/authz/secrets boundary intact and keeps the interface small.

**[D] `PullAuth` arrives resolved the same way (issue #41).** Core reads the app's registry credential,
mints an ECR password if that is what it is, and hands the runtime a username and a `secret.Value` for
one registry — set only on the workloads that run the app's own image, so a sidecar's public image is
pulled anonymously and a credential for one registry is never sent to another. `TrialRequest` carries it
too. The runtime uses it for the pull and keeps nothing: Docker receives it as the request's
`X-Registry-Auth` and stores it nowhere. It is short-lived by construction, which is why it travels with
each plan rather than being configured on the adapter.

**[D] `RuntimeCapabilities.Platform`** is the `os/arch` the runtime runs images for (`linux/arm64`),
data rather than a probe (R-254). The planner compares it with the platforms an image app's image is
published for and refuses a mismatch before anything is pulled. Empty means the runtime cannot say,
and the check is left to the pull.

**[D] Registry authentication is not an adapter category.** It fails §8.1's test on both halves: the
planner asks it nothing — a credential either opens the registry or it does not — and "a username and a
password for a host" is no vocabulary worth hiding. ECR's token minting is the one provider-specific
part, and it is a dozen lines in `core/oci` behind the credential's `kind`. A category would add a
`Capabilities()` nobody consults. If GCR, Artifact Registry or ACR are wanted, each is another `kind`
there.

**[D]** `NetworkPlan.Private` is always true. It is a field rather than an assumption so an adapter that cannot provide a private network fails loudly at capability check (`SupportsPrivateNetwork`) rather than silently placing workloads on a shared network.

**[D]** `NetworkPlan.Egress` arrives resolved: core merged the install's rules with the app's
(`policy.Document.EgressFor`) and decided every loosening before the plan existed. The adapter sees
layers, never policy, verbs or where a rule came from beyond the `From` label it repeats in a refusal
(R-027). `internal/egress` is a leaf package, so the adapter, the plan and the gateway read one
definition of "does this entry match" (`Rules.Compile`, `Compiled.Allows`). A runtime has exactly two
options (R-186):

- **Enforce every layer** and `BlockPrivate`, advertising `SupportsEgressRestriction`. Partial
  enforcement — the allowlist but not the private block, names but not addresses — is not an option.
- **Say it cannot.** The planner then refuses any plan whose `Egress.Restricted()` is true with
  `PLAN_CAPABILITY_UNSUPPORTED` (capability `egress_restriction`). Never deployed with the rules ignored.

**[D]** When `Egress.Restricted()` is false — no layers, or only an empty denylist, and no private
block — **nothing goes in the app's path.** The app runs exactly as it did before egress controls
existed: no gateway, no proxy variables, the ordinary network (R-186). Every existing app on a default
install is in this case, and must not notice the feature.

**[D] The Docker runtime's mechanism (v1).** A restricted app's bundle network is created `internal`,
so it has no route out. A per-app **egress gateway** container sits on that network and on one that
does reach out; it is an HTTP forward and `CONNECT` proxy that decides each connection with
`egress.Compiled` — the name before resolving (`AllowsName`), then the resolved address (`Allows`), so a
public name that resolves to a private address is refused under `BlockPrivate` (R-185). Every workload
is given `HTTP_PROXY` and `HTTPS_PROXY` naming it. The gateway runs Pando's own image by default, so an
install pulls nothing new, and a refused connection is logged with `Decision.Reason`, which is written
for the app's owner.

- **Networks.** A restricted bundle runs on `pando-<bundle>-internal` (`Internal: true`), not on the
  ordinary `pando-<bundle>`. A separate network is used rather than recreating the ordinary one,
  because removing a network means detaching Pando's own container from it first, which `Destroy`
  avoids (on Docker Desktop it drops Pando's published ports). Switching posture recreates each
  workload on the other network; the old one is removed when empty, or by `ReclaimNetworks` at the
  next start. The gateway alone also sits on `pando-<bundle>-outbound`, an ordinary bridge. That
  network is **per app**, not shared: on a shared one, an app whose rules allow private addresses could
  ask its gateway for another app's gateway and through it reach that app's network (R-180).
- **The gateway container** is `pando-egress-<bundle>`: Pando's binary with the hidden
  `pando egress-gateway` command. Its rules come in `PANDO_EGRESS_RULES` and its label
  `io.pando.egress.digest` hashes image, binary and rules, so changed rules recreate the gateway and
  nothing else. It runs as nonroot with a read-only root filesystem, no capabilities and small limits,
  and restarts itself (`unless-stopped`, like the edge), because `Observe` does not report it as one of
  the app's workloads and the reconciler would never notice it down. `Destroy` removes it and all three
  networks.
- **Workloads** get `HTTP_PROXY`, `HTTPS_PROXY` and their lower-case forms set to
  `http://pando-egress:3128`, and `NO_PROXY` naming localhost and every workload in the bundle, which
  talk to each other directly.
- **The gateway** refuses a name before looking it up when no entry could allow it, so a refused name
  is not leaked through DNS. It resolves once and dials only an address it checked, so rebinding cannot
  get around the private block. It never connects to its own loopback.
- **Image.** The adapter's `egress_gateway_image` setting, or else the image of Pando's own container.
  With neither (Pando run on the host), `SupportsEgressRestriction` is false and restricted plans are
  refused (R-186).

**[D] The limitation is stated, not hidden (R-187).** Only traffic through the gateway leaves. Raw TCP,
UDP, and clients that ignore the proxy variables do not — **even under a denylist**, where an owner
would expect everything else to keep working. The plan carries a note saying so whenever a restriction
is in effect. A transparent mechanism would lift this; v1 does not have one, and
adding one later is a change the plan note would have to follow. Two smaller gaps: on Docker versions
whose embedded DNS still answers outside names on an internal network, a workload can leak data
through lookups (clients do not need them, since they hand names to the proxy); and after a posture
switch the old network stays until Pando's next start.

**[D]** The rules a runtime enforces are the ones recorded on the deployment (`deployments.egress_rules`,
design 02 §2.3), which the reconciler passes back on every converge. Build egress is `BuildRequest`'s own
setting and untouched by any of this (R-118, R-189).

### 2.2 Observation

```go
type ObservedBundle struct {
    Exists    bool
    Workloads []ObservedWorkload
    Volumes   []ObservedVolume
}

type ObservedWorkload struct {
    Name        string
    Present     bool
    Running     bool
    Healthy     *bool      // nil = no health signal available
    ImageDigest string
    StartedAt   time.Time
    ExitCode    *int
    RestartCount int
}
```

**[D]** `Observe` reports facts and never remediates. The reconciler (§05) decides what to do. An adapter that silently restarts things makes drift undetectable and breaks R-148's report path.

**[D]** `Healthy` is a pointer because "no health signal" and "unhealthy" are different states and must not collapse (R-221).

### 2.3 Exec

```go
type ExecRequest struct {
    Command []string
    TTY     bool
    Env     map[string]string
}

type ExecSession interface {
    io.ReadWriteCloser
    Resize(rows, cols uint16) error
    ExitCode() (int, bool)
}
```

**[D]** Core opens an exec session only after `app.exec` is checked and policy is consulted (R-084, R-085), and writes the audit event before the session opens (R-228). The adapter does not authorize.

**[D] Resolved (O-7): the command is recorded, the stream is not.** `ExecRequest.Command` goes into
the audit event at session open, along with the workload and the principal. The PTY stream is not
captured.

The argument for full capture is that `app.exec` is the highest-privilege action in the system and
R-086 already concedes the verb list is not a boundary against someone holding it. The arguments
against are decisive: a terminal stream contains secrets — the operator will `env`, will `psql`, will
paste a token — so capturing it builds a durable, searchable store of every secret in the install and
puts it in the audit log, which is the one table deliberately readable by anyone holding audit access.
It also cannot be redacted, because `secret.Value` protects values Pando handles and a stream is
bytes Pando never parses.

Recording the command preserves what the audit log is for — *who did what, when, on which app* — and
answers the question an investigation actually asks first. It does not pretend to answer what was
typed after the shell opened, and the documentation must say so rather than implying exec is fully
audited (R-086's standard).

### 2.4 Capacity

```go
type Capacity struct {
    TotalCPUMillis   int   // 0: not known, and the planner does not check it
    TotalMemoryBytes int64
    TotalDiskBytes   int64
    RunningWorkloads int   // -1: not known
    LargestFit       *Fit  // the roomiest single place's free CPU and memory; nil: not reported
    Details          map[string]any // the runtime's own shape, shown, never interpreted
    Reported         time.Time
}

type Fit struct {
    CPUMillis   int
    MemoryBytes int64
}

type InUse struct {
    CPUMillis   int
    MemoryBytes int64
    Reported    time.Time
}
```

**[D]** R-243. The local Docker adapter reports its own machine; a clustered adapter reports its cluster. Core does not read `/proc` and has no concept of a host.

**[P]** `LargestFit` is what one place has free by committed limits, because on several machines the
total can have room no one machine has: 6 GB free over three hosts does not place a 4 GB app. The
single-host Docker adapter reports its totals less what its app containers are limited to; the
multi-host adapter the roomiest host open to new apps; Kubernetes the eligible node with the most
memory left (then CPU) once every pod's requests on it are counted, Pando's included — one node, so
the figure is a place that exists (notes-kubernetes-runtime-issue-72.md). Kubernetes's totals still
leave out only pods outside Pando's namespaces, because the planner subtracts Pando's own
allocations from them itself.

**[D]** The planner refuses at plan time an app no single place has room for, through
`RuntimeAdapter.LargestFitFor(ctx, bundleID) (*Fit, error)`: `LargestFit` as it stands for that
bundle. A runtime that keeps a bundle where it was placed answers for that place only, counting what
the bundle already holds there as free, so a redeploy that fits in place is never refused; for a
bundle placed nowhere yet it answers the roomiest place. The question takes the app's ID rather than
the planner subtracting the app's recorded allocation, because only the runtime knows where the app
is and what its workloads actually reserve there, and the answer stays a number: core learns no
host (R-251). The planner compares the sum of the bundle's workload limits with it and refuses with
`CAPACITY_WOULD_OVERSUBSCRIBE` and a message that says the room cannot be combined across places;
policy allowing CPU or memory oversubscription lifts the check for that resource, as it does R-242's.
Nil means one place, or not reported: the single-host Docker adapter answers nil, since R-242's check
of its totals already says everything this would. Kubernetes answers the roomiest node with the
bundle's own pods' requests counted as free there; a cluster reschedules freely, so it is not held to
one node.

**[P]** The check compares the sum of an app's workload limits with one place, as multi-host Docker
places a bundle whole. On Kubernetes each workload is its own pod and could land on a different node,
so the sum is stricter than the scheduler needs; it errs toward a plan-time refusal over a pod that
waits for room. Where it still passes and the scheduler finds no node, the Kubernetes adapter turns
the scheduler's refusal into the deploy's error. Image delivery is §1's `ImageDelivery` (PR 5):
multi-host Docker reports `[registry]` and refuses `ImportImage`.

**[P]** The common fields are the readings every runtime reports in the same shape, so the console can say how much room is left without knowing which runtime answered (issue #88). Anything else goes in `Details`, which the console shows as it came. Live use is its own call, `InUse`, rather than `Used*` fields on `Capacity`. Sampling CPU takes about a second, and the planner reads `Capacity` on every plan without needing it. The `Used*` fields, which no adapter ever filled, are gone. `GET /capacity` reports each runtime's totals beside what Pando has committed on it, which is the planner's own R-242 arithmetic, and live use where the runtime reports usage.

---

## 3. Builder

```go
type BuilderAdapter interface {
    Adapter
    Capabilities(ctx context.Context) (BuilderCapabilities, error)

    // Bid inspects the source and returns a confidence score plus a draft
    // spec fragment. Part of the detector auction (R-093).
    Bid(ctx context.Context, src SourceView) (Bid, error)

    Build(ctx context.Context, req BuildRequest) (BuildResult, error)

    // Forget removes what the builder keeps for an app between builds (its
    // build cache) once the app is deleted. Called by the GC's teardown;
    // idempotent. R-224: a deleted app's cache had stayed on disk forever.
    Forget(ctx context.Context, namespace string) error
}

type Bid struct {
    Confidence float64        // 0..1
    Strategy   BuildStrategy
    Draft      spec.Partial   // workloads, ports, volumes, slots it can infer
    Evidence   []string       // human-readable, shown in the review UI
    Questions  []Question     // things it cannot determine (R-102)
}

type Question struct {
    Key       string
    Prompt    string   // must satisfy R-105: self-contained and pasteable
    Why       string
    Kind      QuestionKind  // choice | text | port | path
    Options   []string
}
```

**[D]** `Question.Prompt` carries a hard content requirement from R-105: it must be answerable by a model that cannot see the repo. Enforce it in review — a prompt reading "Which port?" fails; "This app appears to be a Node.js service. Pando could not determine which port it serves HTTP on. Valid answer: a port number such as 3000." passes.

**[D]** `SourceView` is a read-only filesystem view (R-020). It exposes `Open`, `Stat`, `Glob`. It has no write methods, structurally.

```go
type BuildRequest struct {
    Source         SourceView
    Strategy       BuildStrategy
    Dockerfile     string
    Context        string
    Args           map[string]string
    IsolationFloor IsolationClass
    Timeout        time.Duration
    EgressMode     EgressMode
    EgressAllow    []string
    CacheNamespace string        // per-app (R-117)
    LogSink        io.Writer     // streamed to the console live
}

type BuildResult struct {
    ImageRef    string
    Digest      string
    ObservedPorts []int    // from the trial run (R-097)
    ObservedWrites []string // directories written outside declared volumes (R-202)
}
```

**[D]** `ObservedPorts` and `ObservedWrites` are the trial run's output. They are the mechanism behind R-097's "watch what it binds, don't ask" and R-202's improved persistence warning.

**[D]** A builder implementation must never receive or request a container runtime socket (R-112). This cannot be enforced by the type system; it is a review checklist item and an integration test that asserts the build environment has no socket mounted.

**[P]** R-116: where a runtime adapter can provision an isolated environment per app — Incus, a
Firecracker VM — building *inside that environment* is preferred, because build isolation then comes
free from the boundary that already exists rather than from a second mechanism layered beside it. This
is not v1 work: v1 ships a Docker runtime whose isolation class is `container`, so BuildKit supplies
the build boundary independently. The note is here so that whoever writes the first VM-class runtime
adapter considers it before building a parallel isolation path. `BuilderCapabilities.IsolationClass`
stays independent of the runtime's either way (R-114) — a builder that borrows the runtime's boundary
reports the class it actually gets.

---

## 4. Routing

```go
type RoutingAdapter interface {
    Adapter
    Capabilities(ctx context.Context) (RoutingCapabilities, error)

    // Ensure makes traffic for this app arrive at Pando's proxy.
    // It never routes to the workload (§00 1.3).
    Ensure(ctx context.Context, r RouteRequest) (RouteHandle, error)
    Remove(ctx context.Context, h RouteHandle) error
    Observe(ctx context.Context, h RouteHandle) (RouteState, error)

    // Edge describes the install-scoped workload this adapter needs running
    // in front of Pando, if any (R-174, §4.4). false means none: loopback, or
    // a Traefik somebody else runs.
    Edge(ctx context.Context, r EdgeRequest) (EdgePlan, bool, error)
}

type RouteRequest struct {
    AppID      string
    Mode       RoutingMode
    Hostname   string
    PathPrefix string
    Port       int

    // Where the adapter must send traffic. Always Pando's proxy.
    ProxyUpstream string
    TLS           TLSRequest
}
```

**[D]** `ProxyUpstream` is in the request rather than discovered by the adapter, so the contract is explicit: an adapter is told where to point, and that destination is always the proxy. R-023 has no exceptions and this field is where that is made obvious to an adapter author.

**[D]** The address must be reachable **from wherever the adapter's data plane runs**, which is not
necessarily where Pando runs. With Traefik they are the same host; with an outbound-tunnel adapter the
tunnel daemon dials it from its own container. Surfaced by the Cloudflare sketch
(`notes-cloudflare-routing-sketch.md`) — a documentation change, not an interface one.

**[D]** `TLSRequest` is **advisory**. An adapter may satisfy it however it likes, or ignore it because
its edge already terminates TLS. It is an intent, not a set of instructions. This is O-5 resolving the
way the design assumed, and the tunnel sketch is the second adapter to want it.

**[D] The interface survived being sketched against a provider that works nothing like Traefik** — no
local config file, no listening port, no certificate on the host, configuration applied by remote API.
The reason it survived is that `Ensure`/`Remove`/`Observe` describe *intent* rather than mechanism.
Remember that when someone proposes adding a `Reload()` or a `ConfigPath` here: the abstraction holds
because it has neither.

**[D]** Path-mode adapters must strip the prefix and set `X-Forwarded-Prefix` (R-167). They must not rewrite response bodies (R-028).

### 4.1 Two topologies

R-164 and R-165 describe two ways an install can be reached. Both are supported; they are not adapter
choices so much as postures the routing configuration expresses.

**Proxy mode** — one hostname, one certificate, one thing to open on the firewall. Every app is
reached by logging into Pando first and following a path or a menu. **[D]** This is the recommended
enterprise topology, and the reason is procurement rather than engineering: one public hostname is far
easier to get approved than N of them. An install that cannot get a wildcard DNS record or a second
firewall rule can still run every app.

**Per-hostname** — each app has its own hostname; users bookmark URLs and carry a session. Pando is
invisible except at login. Better for apps that feel like products in their own right, and required
for anything where the URL is shared outside the organization.

**[D]** Neither is a global setting. The topology is the aggregate of each app's `Routing.Mode`, and an
install can mix them — an internal tool on a path and a customer-facing app on its own hostname, on
one Pando. What makes an install feel like one topology or the other is the routing adapter's
`DefaultMode` (R-162), which is why that field exists and why deviating from it is gated by
`app.routing.override`.

**[D]** The choice is not surfaced during setup (R-005, R-104). A user adding an app gets the
adapter's default; `ModeSource` records whether the mode was inherited or deliberately chosen, so the
console can later show which apps deviate and host policy can restrict overrides (R-274, O-10).

**[D] Changing a configured app's address** is `PUT /apps/{id}/routing` (`internal/core/address`),
and the console's Change beside Address on the app's Overview. It writes a revision the next deploy
ships, like any edit (R-152). Moving to another adapter takes that adapter's default mode and a
hostname in its domain; a path defaults to the app's slug and may be any other (below); a port is
allocated (§4.2). Two gates, both on the server so every client meets them: a mode the
adapter does not default to needs `app.routing.override` (R-163) — checked on `POST /specs` as well,
so a whole spec is not a way around it, and `ModeSource` is set from the adapter rather than taken
from the author — and any change needs `confirm`, because the old address stops working and bookmarks
to it break (R-165, design 01 §6). Re-running Configuration keeps the app's routing (`spec.Carry`):
the repository has nothing to say about it.

**[D] A path-mode app's path is whatever it is set to**, not only its slug: up to four segments of
lowercase letters, digits and hyphens (`spec.CheckPathPrefix`), such as `/team/notes`. The proxy finds
the app by the longest whole-segment prefix of the request's path (`state.Apps.ByPath`), strips it,
and names it in `X-Forwarded-Prefix` (R-167); `/<slug>` still answers as it always has. Three rules keep
one path to one app, decided when a revision is pinned (`state.Apps.Pin`, migration 35) and also asked
before a change is saved so the person choosing hears it then:

- **One live app per address.** `apps.address_hostname` and `apps.address_path` carry the pinned
  address, with unique indexes over live apps. This also closes a gap that predates paths: two apps
  could pin the same hostname, and the proxy answered for whichever the database returned first.
  The proxy resolves a request by these columns, and by `apps.address_port` for a port-mode app
  (migration 46), never by reading pinned specs' JSON: one indexed lookup per request (issue #72).
- **No path inside or around another's** (`/team` and `/team/notes`), and none whose first segment is
  another app's slug. One app would receive the other's requests, and an app claiming a path under
  another's would be a page in that app's name that it does not control — on Pando's own origin.
- **Never one of Pando's own paths** (`spec.ReservedPaths`: `/api`, `/admin`, `/login`, `/.pando` and
  the rest). A test walks the router and fails if a top-level route is not reserved.

### 4.2 Host ports in port mode

**[D] Resolved (O-15): the lowest free port in a configured range, held as a durable allocation.**
Default range `9000-9999`, one row per `(adapter_ref, port)`, exhaustion returning
`CAPACITY_NO_FREE_PORT` and naming the setting to widen.

A port is an **allocation**, not a derivation. The first version computed "the lowest number no pinned
spec is using", which is a guess about an allocation rather than one, and it raced in the ordinary
case rather than an exotic one: adding five apps at once runs five background detections, two compute
the same answer before either writes anything down, and both are handed 9001 with nothing noticing.
The unique constraint is what makes a collision impossible instead of unlikely.

Lowest-free rather than random, so an app tends to keep its port across a rebuild and a bookmark keeps
working. Reused rather than ever-increasing, so a deleted app's port comes back.

**A port is only an address if something listens on it.** The allocation was written into every
port-mode spec and shown to users for several phases before anything accepted a connection on it, so
a loopback install advertised an address that refused the connection and the apps were in fact
reachable only under Pando's path prefix. That is the worse half of the bug rather than the cosmetic
one: under a prefix an app that writes `/assets/app.js` into its own HTML comes up blank, and R-167
forbids rewriting the page to hide it — port mode is the answer to exactly that case. The proxy now
opens one listener per allocated port (`internal/proxy/ports.go`), reconciled against the allocations
rather than against running apps, so a stopped app answers "this app isn't running right now" at its
own address instead of refusing the connection. Requests on those listeners take the same path as
every other request: the port is one more way to resolve an app, not a second enforcement point
(R-023).

**[P] In a containerized install the published range and the allocation range are the same range.**
`docker-compose.yml` sets both from one pair of variables, defaulting to `9000-9019` rather than the
shipped `9000-9999`: Docker publishes a range by opening every port in it, and a thousand is minutes
of startup and a process per port. They have to move together — an app allocated a port outside the
published range gets an address nothing can reach, which is the bug above with extra steps.

**The revisit is closed by Traefik shipping.** O-15 was to be reconsidered at phase 10, on the
question of whether lowest-free and `9000-9999` were right. Phase 10 shipped a routing adapter that
does subdomain and path, so port mode is now the laptop default's path and not the only path —
`loopback` supports neither of the R-166 topologies (§4.1 above, and the table in §10), which is why
it needs a host port at all. An install that outgrows the range has a better answer available than a
wider range, and R-005 rules out the obvious alternative for the laptop case: someone who may not know
what a port is cannot pick a free one.

### 4.3 Certificates on the edge Pando runs

**[D] Resolved (O-5): both ACME challenge types, chosen per install.** R-169.

O-5 sat open for as long as it did because deferring to each adapter really was the answer while the
edge was somebody else's process. R-174 changed that: Pando starts the edge and therefore writes its
static configuration, so Pando is the thing choosing a challenge type, and "the adapter decides"
stopped being a place to put the question.

| | HTTP-01 | DNS-01 |
|---|---|---|
| Needs | a reachable `:80`, an email address | a DNS provider credential |
| Gives | one certificate per app hostname | `*.base-domain`, before an app exists |
| Suits | an install that does not control its own DNS | R-166's preferred topology |

Offering one would be wrong in opposite directions. HTTP-01 alone makes R-166's preference —
subdomain where a wildcard is available — permanently unavailable on the adapter Pando ships. DNS-01
alone makes TLS conditional on a credential many installs cannot produce, in a product whose defining
property is that setup is paid once.

**Neither is a silent default.** An install that configures neither gets `:80`, and is told that is
what it has. A certificate that quietly failed to issue surfaces as a browser warning to a visitor
rather than as a message to an operator, which is the wrong person finding out.

The per-adapter shape is unchanged and still carries it: `RoutingCapabilities.SupportsTLS` and
`SupportsWildcardTLS` are how an adapter says what it can do, and they now follow the configured
challenge type rather than the mere presence of a resolver name.

### 4.4 The edge Pando runs

**[D] Built (R-174).** A routing adapter that needs a process in front of Pando — Traefik terminating
`:80` and `:443`, `cloudflared` holding a tunnel open — describes it as an `EdgePlan`, and core asks
the default runtime adapter to run it. The routing adapter says *what* runs; the runtime adapter says
*how*; core joins them. Neither adapter reaches into the other, which is the same reason the Traefik
adapter uses the file provider rather than labels on a workload.

```go
type EdgePlan struct {
    Name   string                  // unique per install; one edge per routing adapter config
    Image  string
    Args   []string
    Env    map[string]secret.Value // DNS-01 credentials, a tunnel token
    Ports  []EdgePort              // host ports; the only ones Pando ever publishes
    Mounts []EdgeMount             // a path shared with Pando, or a volume the edge owns
    ProxyAlias string              // the name the edge uses to reach Pando's proxy
}

// On RuntimeAdapter, gated by RuntimeCapabilities.SupportsEdge:
ApplyEdge(ctx context.Context, p EdgePlan) error
ObserveEdge(ctx context.Context, name string) (EdgeState, error)
RemoveEdge(ctx context.Context, name string) error
Edges(ctx context.Context) ([]string, error)
```

**[D] A separate entry point, not a flag on `BundlePlan`.** An edge publishes host ports, which R-026
forbids for a workload, and it is not in any app's private network. A flag on the app path would be a
loophole every app plan could reach; a second method cannot be reached by building a plan wrongly.

**[D] What an edge may do is narrow.** It joins one network, `pando-edge`, that holds the edges and
Pando's own container, under the alias its plan names — so it can reach Pando's proxy and nothing
else. It never joins an app's network: an edge that could reach a workload is R-023 with extra steps.
It restarts itself (`unless-stopped`) where an app does not (R-149): an edge has no backoff state and
no failed state, and every app behind it is unreachable while it is down.

**[D] Shared configuration is resolved by the runtime adapter** (R-251). The Traefik adapter writes its
per-app files into a directory in Pando's own container. `EdgeMount.SharedWithPando` names that path,
and the Docker adapter finds whatever volume or bind is mounted there in its own container and mounts
the same storage into the edge, read-only. Core never learns it is a Docker volume.

**[D] Applied at startup and on the reconciler's tick,** idempotently: a matching edge is left alone,
a changed plan recreates it, and an edge no configured adapter asks for is removed. Changing an
adapter's settings takes effect on restart, as every adapter setting does (`pending_restart`).

**[D] Traefik runs in one of two modes, Pando-run by default.** `managed: true` is the default for a
new Traefik adapter: Pando starts `traefik`, generates its static configuration from the adapter's
settings, and publishes `:80` and `:443` (both overridable). `managed: false` is the mode that existed
before R-174 was built — somebody else runs Traefik, and Pando only writes route files into the
directory it watches. It stays because an install that already runs Traefik for other services cannot
give Pando those ports. A migration marks every Traefik adapter configured before this change
`managed: false`, so upgrading never starts a second Traefik on a host that has one.

**[D] The console answers any hostname that is not an app's.** The proxy already falls through to the
console for an unknown host (`proxy.StateResolver.IsAppHostname`). A Pando-run Traefik adds a
lowest-priority catch-all router to Pando, so pointing a domain at the edge loads the console. The
**console hostname** setting exists only because a certificate is issued per hostname: it is the one
hostname the edge asks a certificate for on the console's behalf. Apps default to
`<app>.<base domain>`.

**[D] The Traefik image is pinned by Pando's release and overridable** in the adapter's settings.
Upgrading Pando upgrades the edge; an operator who needs a different Traefik sets `image`.

**[D] DNS-01 providers: five named, and "other".** Traefik supports every DNS provider its ACME
library does and needs only the provider's code and its credentials as environment variables. The
adapter offers Cloudflare, Route 53, DigitalOcean, Porkbun and Namecheap by name — which lets it name a
missing variable in a message that meets R-105 — and accepts any other provider code with whatever
`KEY=value` pairs the operator gives. Credentials are stored like every adapter credential (R-190) and
reach the edge as `secret.Value` environment variables.

**[P] Routes through the cluster's API on Kubernetes.** With `delivery: kubernetes_api` the Traefik
adapter writes one `IngressRoute` per app into the edge's namespace instead of a file, every one naming
the Service Pando's proxy is reached by, and its edge plan reads routes through Traefik's Kubernetes CRD
provider, limited to that namespace with cross-namespace references off (`ReadsRoutesFrom` on the
plan). On that delivery Traefik orders no certificates: its plan's `Issue` asks Pando's leader for
them (`internal/core/edgecert`), core puts the issued ones in `EdgePlan.Certificates`, and the runtime
writes them where every replica reads them (`notes-kubernetes-runtime-issue-72.md`).

**[D] Certificate storage survives the edge.** The ACME store is a volume the edge owns
(`EdgeMount.Volume`), not the container's filesystem, so recreating the edge does not re-issue every
certificate and meet Let's Encrypt's rate limits. It is part of the full-host DR bundle for the same
reason.

### 4.5 Cloudflare Tunnel

**[D] Built** as the sketch in `notes-cloudflare-routing-sketch.md` described, with no interface
change beyond §4.4, which Traefik needed first.

- **Pando creates and owns the tunnel by default.** Given an API token (Account → Cloudflare Tunnel:
  Edit, Zone → DNS: Edit, Zone → Zone: Read), the adapter creates a remotely managed tunnel named
  `pando-<zone>` and runs `cloudflared` as its edge with the tunnel's token, fetched through the API
  and passed in its environment. The name comes from the zone rather than the adapter's ID because
  the zone is known at `Configure`, and an app routed before the first `Edge` call must find the same
  tunnel. **Attaching to an existing tunnel** is the option: given its ID, Pando adds its rules to
  that tunnel's ingress and leaves the rules it did not write alone.
- **Rules are Pando's by hostname and path.** Each app's rule is upserted whole on every `Ensure`,
  and the last rule — the catch-all Cloudflare requires — sends everything else to Pando's proxy, so
  any hostname routed to the tunnel that is not an app's shows the console.
- **Subdomain by default, path available** (R-161, R-162) — path on one hostname is R-164's proxy mode.
- **Apps are one level below the zone; the console can be anywhere in it.** Zone `bemeek.io` with
  console hostname `pando.bemeek.io` serves the console there and apps at `<app>.bemeek.io`. The zone
  is the adapter's `RoutingCapabilities.BaseDomain`, which a new app's hostname is taken from ahead of
  the install's `server.base_domain` — the same way `DefaultMode` is, and for the same reason: an app
  named outside the zone is one the tunnel can only refuse.
- **There is no setting for a deeper base domain.** Cloudflare's included certificate covers `*.zone`
  and nothing below it, so `<app>.apps.bemeek.io` would have no HTTPS. An option whose only effect is
  that is a mistake the form would invite; it can come back with a setting that says the zone has
  Advanced Certificate Manager, when someone needs it.
- **Pando owns what it made, and says so.** A change made in Cloudflare's dashboard to one of Pando's
  rules is reported by `Observe` and reset by the next `Ensure`. Each
  DNS record Pando creates carries the comment *Managed by Pando — changes are reset*, and Pando never
  touches a record without it.
- **Cloudflare Access is not part of this adapter.** It would be a layer in front of Pando's proxy and
  never the only check (R-023). Whether Pando should configure it is O-23.

---

## 5. Identity

```go
type IdentityAdapter interface {
    Adapter

    // Begin returns where to send the user, or nil for adapters that
    // authenticate inline (local username/password).
    Begin(ctx context.Context, req BeginRequest) (*Redirect, error)

    // Authenticate resolves an inbound callback or credential to a subject.
    Authenticate(ctx context.Context, c Credential) (Subject, error)

    // ServiceMetadata is what the provider is given to trust Pando: SAML SP
    // metadata. Nil for adapters that have none.
    ServiceMetadata(ctx context.Context, e Endpoints) (*Metadata, error)

    SessionPolicy() SessionPolicy   // R-047: each adapter declares its own
    SupportsPush() bool             // the provider can push through SCIM (R-048)
}

type Endpoints struct {
    CallbackURL string   // OIDC redirect_uri; SAML ACS URL
    EntityID    string   // SAML SP entity ID, also its metadata URL
}

type BeginRequest struct {
    Endpoints
    State string         // core's: random, bound to the browser
}

type Redirect struct {
    URL  string
    Flow []byte          // the adapter's own data for this sign-in, returned in Credential.Flow
}

type Credential struct {
    Username string; Password secret.Value   // inline adapters
    Callback url.Values                      // redirect adapters: what the provider sent back
    Flow     []byte                          // nil for a sign-in the provider started
    Endpoints
}

type Subject struct {
    ExternalID    string   // stable within this adapter
    Email         string
    EmailVerified bool     // the provider vouches for it; linking by email needs it (O-1)
    DisplayName   string
    Username      string
    Groups        []string // external group identifiers
    Attributes    map[string][]string  // everything sent, for a test sign-in to show; read by nothing
    OneTimeID     string   // e.g. a SAML assertion ID, used once
    OneTimeUntil  time.Time
}

type SessionPolicy struct {
    MaxLifetime      time.Duration
    RevocationMode   RevocationMode // push | refresh | expiry_only
    RefreshInterval  time.Duration
}
```

**[D]** Identity adapters authenticate only (R-044). `Subject` carries no roles, no verbs, no permissions. Group *names* cross the boundary; what a group can *do* is Pando's (R-078).

**[D]** `SessionPolicy` is how R-047 is honored mechanically: each adapter declares its own lifetime and revocation mode, and the console can display the effective revocation window per adapter rather than implying a global guarantee.

**[D] Adapters stay stateless across a redirect (issue #51).** A redirect sign-in spans two requests,
and what the adapter must check the second against — a PKCE verifier and nonce, a SAML request ID —
goes back to core as `Redirect.Flow` and returns in `Credential.Flow`. Core stores it with the flow,
binds the flow to the browser that started it, and consumes it once (design 06 §3.2). Core also owns
the `State` and the `Endpoints`: an adapter never works out its own address, because behind a
TLS-terminating proxy it would get it wrong and a redirect URI must match to the character. This
changed `Begin`'s signature from `(ctx, redirect string)` to `(ctx, BeginRequest)` and replaced
`Credential.Code`/`State` with the raw callback parameters, since SAML sends neither; nothing used the
old fields.

**[D] Identity adapters are built on use, not at startup.** Every other category is registered once
from `adapter_configs` (R-253, §9). An identity provider is a row in `identity_adapters`, and core
builds its adapter when first used and again whenever the row changes, so connecting or fixing a
provider is not a restart in front of everyone who signs in. The kinds are still compiled in; only
their configuration is read live.

**[D]** `SupportsPush` says the provider *can* push. Whether it does is core's to know: SCIM is on when
the provider has a token, and only then does core report the provider's revocation as `push`. OIDC and
SAML declare `expiry_only` — Pando keeps no provider tokens to refresh — so without SCIM the session
lifetime is the revocation window (R-050), and the console says so.

**[D] What the two external adapters check.** OIDC: authorization code flow with PKCE (S256), nonce,
ID token signature/issuer/audience/expiry via discovery and JWKS, RFC 9207 `iss` when sent, userinfo
merged only for the same `sub`. SAML: SP-initiated HTTP-Redirect, response over HTTP-POST; a
signature on the response or the assertion is required; issuer, audience, recipient, destination and
`InResponseTo` are checked with three minutes of clock skew; IdP-initiated responses are refused
unless the provider allows them; assertion IDs are single-use through `OneTimeID`. A transient NameID
is refused, because it cannot identify anyone twice.

## 6. Secrets

```go
type SecretsAdapter interface {
    Adapter
    Put(ctx context.Context, ref SecretRef, v secret.Value) (StoredRef, error)
    Get(ctx context.Context, ref StoredRef) (secret.Value, error)
    Delete(ctx context.Context, ref StoredRef) error
}
```

**[D]** `secret.Value` is the redacting wrapper from §00 3.3. Its `String()`, `MarshalJSON()`, and zap marshaler all return `[redacted]`. A secret cannot be accidentally logged because the type will not render.

**[D]** `Get` is called by core only during plan resolution, to populate `WorkloadPlan.Env`. It is never called on behalf of a user request except when `app.secrets.read` is held, and that path writes an audit event first.

---

## 7. Services

Fills provisioned slots (R-131).

```go
type ServicesAdapter interface {
    Adapter
    Capabilities() ServicesCapabilities
    Supports() []SlotType
    Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)
    Destroy(ctx context.Context, h ServiceHandle) error
    Snapshot(ctx context.Context, h ServiceHandle, dst io.Writer) error
    Restore(ctx context.Context, h ServiceHandle, src io.Reader) error
}

type ServicesCapabilities struct {
    DataInAppVolumes bool  // the data is in volumes Pando already backs up
}

type ProvisionRequest struct {
    AppID, BundleID, SlotKey string
    Type      SlotType
    ServiceID string        // minted by core, so Provision is idempotent
    ExistingSecret secret.Value // the DSN a previous Provision returned, if any
}

type ProvisionResult struct {
    Handle      ServiceHandle
    ConnectionSecret secret.Value  // the URL/DSN, stored as a secret (R-131 literal note)
    Workloads   []WorkloadPlan     // injected into the bundle, never exposed (R-026)
    Volumes     []VolumePlan       // the workloads' storage, owned by the app
}
```

**[D]** A provisioned service returns workloads that join the app's private bundle. It is not exposed, not addressable from outside, and not shareable with another app (R-134).

**[P]** `ProvisionResult.Volumes` — added in phase 9. A database workload with nowhere to put its files
loses everything on the next deploy, and R-135 says a provisioned service's data follows the app's
volume rules, which it can only do if it is in a Pando volume. The volumes are the app's: recorded
after Apply, carried in the DR bundle, offered at delete, reclaimed once backed up. None of that
machinery knows services exist.

**[D] `Provision` must be pure and cheap.** It is called on every deploy, and again on every
reconcile — the reconciler's "what should be running" has to contain the provisioned service, or a
killed database is never restored (R-148) and the running one is reported every fifteen seconds as a
workload the spec does not declare. An implementation that talked to a provider here would be talking
to it four times a minute per app. The interface is shaped so it does not have to: `Provision` returns
plans, and the runtime adapter creates things. The reconciler drops `Env` from what comes back, so
building the comparison is never a reason to decrypt a secret (R-193).

**[P]** `ProvisionRequest.ServiceID` and `ExistingSecret` — added in phase 9 because **Provision runs on
every deploy**, not only the first: the workloads it returns *are* the service, so a redeploy that
skipped it would produce a bundle with no database in it and the runtime would converge to that.
Running every time means every run after the first must produce the same names and the same
credentials. A database sets its password when its data directory is created and ignores the variable
forever after, so an adapter generating a fresh one each call would hand the app a password the
database has never heard of — a deploy that succeeds and an app that cannot authenticate, with the
symptom nowhere near the cause.

**[P]** `Capabilities().DataInAppVolumes` — how the DR path knows whether to call `Snapshot` (R-212).
True for the in-bundle provisioner: its data is under `volumes/` in the bundle already, and calling
`Snapshot` would put the same bytes in twice. False for an adapter that provisions somewhere Pando can
only reach over the wire, where `Snapshot` is the only way the data arrives. The bundle manifest counts
both `services` and `services_snapshotted`, so "5 services, 0 snapshotted" is legible rather than
alarming. Data and not a type assertion, per R-254.

### 7.1 The in-bundle provisioner

**[P]** `internal/adapter/services/docker` fills `postgres`, `mysql` and `redis` and talks to no daemon.
It *plans* a service; the runtime adapter runs it. Two properties follow, and both are the point: the
service is reachable only on the app's private network because that is the only network its workloads
join and nothing publishes a port (R-134), and its data is an ordinary app volume (R-135).

**[P]** Images default to `postgres:17-alpine`, `mysql:8.4`, `redis:7-alpine`, overridable per install
through the adapter's config. Alpine variants because a hobbyist's host is the target and a 400MB
Postgres image on a two-core VPS is a cost with nothing to show for it.

**[P] The builder synthesizes a Dockerfile for strategies that have none.** `static` and `buildpack`
both end as a Dockerfile build, written outside the repository where possible and handed to BuildKit
as a separate filesystem from the context — so nothing is copied and the app's source is untouched.
The Dockerfile is Docker's vocabulary, so this belongs in the adapter and not in core (R-251): core
says "a directory of files to serve" or "a repository with no instructions", and a different builder
may answer either differently.

**[P] `buildpack` is nixpacks, invoked only to plan.** `nixpacks build --out` writes a Dockerfile and
builds nothing, so R-112 is not in play and the install needs no extra service. The generated
Dockerfile stays in the checkout because nixpacks' own output does `COPY . /app/.` alongside
`COPY .nixpacks/…` — the build context must be the source with the generated directory inside it.
Generation runs with no network and a minimal environment: it reads the repository and decides, and a
planner that can reach the internet while reading untrusted source is a wider boundary than this
needs. Paketo is the opt-in alternative and needs a registry in the install topology (R-095).

**[P] What nixpacks is *told* comes from the repository, when the repository says anything.** R-094's
ladder ranks evidence, and the rungs above convention-matching are the app's author having stated
something: a CI workflow's build job, a `Makefile`/`Taskfile.yml`/`justfile` target, a `Procfile`'s
web process, or a client config and a `go:embed` directive naming one directory. Each becomes
nixpacks' own `--build-cmd`, `--start-cmd` and `--pkgs`; nothing here synthesizes a Dockerfile or
picks a base image, which is what keeps R-095's "wrap rather than reimplement" true. Every reader
takes a literal and declines anything computed — a config that has to be evaluated to find its
answer is one this does not evaluate.

**[P] The planner runs twice when a declaration is an ordering rather than a command.** `//go:embed
dist` beside a client that builds into that directory says the client comes first, and says nothing
about what it comes *before*. So: plan, read the chosen build command back out of the generated
Dockerfile, plan again with the client build ahead of it. Two subprocesses is the cost of not knowing
what nixpacks would have chosen without asking it. Only this one case needs the second pass; a
declaration that names the build replaces nixpacks' choice in a single call.

**[P] `BuildPlanner.Plan` returns a `*api.PlanDeclaration`** — which source dictated the plan, why,
and the confidence a detector should bid. The builder knows this and detection cannot derive it
without a second implementation of the same reading, which R-027's import rule exists to prevent from
drifting apart. It is a definition in `internal/adapter/api`, filled by the builder and read by core,
so neither learns the other's vocabulary (R-251). Nil means convention-matching chose the plan. See
[the design note](notes-declared-builds-without-a-runner.md).

**[P] Every image Pando supplies itself is pinned by tag, not digest** — these three and the runtime
adapter's `busybox:stable` volume helper. This is a real weakness and worth naming rather than
leaving in a code comment: the bytes behind a tag can change, and the BusyBox one is pulled at
*restore* time, which means it can change between the backup and the disaster. A digest belongs in all
four places once there is a way to update them — a pinned digest with no update path is an image that
never gets a security fix, which is the failure this trades against.

**[P]** `s3` and `smtp` are slot types Pando recognizes (R-130) and deliberately does not provision:
standing up MinIO or an SMTP server is running infrastructure, which R-010 says Pando is not. The
planner refuses a `provisioned` resolution for them by name, at plan time.

**[P]** `Destroy` is a no-op and `Snapshot`/`Restore` return an error rather than an empty archive. The
workloads go with the bundle and the volume follows R-204 and R-135 — it outlives the app on purpose
and is reclaimed once it is backed up. An empty archive would look like a service with no data, which
is a thing an operator discovers at restore time.

---

## 8. Notification

```go
type NotifyAdapter interface {
    Adapter
    Capabilities() NotifyCapabilities // Audience: people | channel
    Notify(ctx context.Context, n Notification) error
}

type Notification struct {
    Kind       NotificationKind // Pando's own (app_failed, deploy_approval, app_shared, …) or an event name
    AppID      string
    Recipients []Recipient      // empty for a channel
    Subject    string
    Body       string
    EventID    string           // evt_…, when it tells of an event
    Fields     []NotificationField
    Link       string
}
```

**[D]** The console is the default (R-231). Email, Slack, Microsoft Teams, Discord and ntfy ship
built in (R-232, R-374, issue #50). An adapter's **audience** is capabilities data (R-254): one that reaches
**people** gets Pando's own notifications, filtered by each person's preferences; one that posts to a
**channel** hears only from event subscriptions, because a message meant for one person must not land in a
room (R-373). The kinds, the router and the subscriptions that send events through these adapters are in
[11-events-and-subscriptions.md](11-events-and-subscriptions.md).

---

## 8.1 What is and is not an adapter category

**[D] Backup is the eighth category (R-252).** This reverses the decision recorded here through phase
8, and the reversal is written down rather than quietly applied.

**What this section used to say**, and why it was reasonable: writing a DR bundle to local disk, S3 or
a mounted share is a choice of byte sink. The adapter model exists for capability negotiation and
vocabulary translation; a destination that accepts bytes and returns them has neither, so it should be
a small `Destination` interface over `io.Writer`/`io.Reader` and nothing more. Making it a category
would give it a `Capabilities()` no caller consults and a `HealthCheck()` whose failure means nothing
until a backup runs.

**Why that was wrong.** It described a *destination* accurately and then assumed the destinations
people want are destinations. They are not. An object store expires objects on its own schedule,
versions them, and may hold a compliance lock that prevents deletion; a filesystem path does none of
those. So R-211's retention has two possible owners — Pando, or the destination — and which one is in
charge is a real question with a real wrong answer: if Pando prunes what the store has already locked,
every prune fails; if the store expires what Pando still counts as retained, a restore finds nothing.
That is precisely the "advertise capabilities as data" case R-254 exists for. A `Destination`
interface would have grown a capabilities struct within one more provider, under a worse name, and it
would have been consulted through a type assertion — which R-254 forbids because a type assertion is
invisible to the caller that needs to plan around it.

`HealthCheck()` earns its place for the same reason the argument dismissed it: an unreachable backup
destination is worth knowing about *before* the disaster, not at the moment a backup runs. That is
R-216's argument for verifying a bundle before it is needed, one level up.

**The test to apply before adding a ninth** is unchanged, and it is a good test: **does the planner
need to ask it a question, and does it have a vocabulary worth hiding?** Backup passes the first half
— retention ownership and whether the destination can list and expire are questions with plan-time
consequences. It passes the second thinly: "bucket" and "prefix" are a vocabulary, if a small one.
A category that passes neither is a library.

**[D] The tenth is AI (R-258), and it passes both halves wide.** Whether a screener can read a
repository, how much of one, and which of R-106's three functions it performs are all questions with
consequences before any work starts — capabilities data, per R-254. And models, context windows,
tokens, tool calls and system prompts are as much a provider's vocabulary as anything in this
document; R-251 says core never learns it. Core says "screen this proposal against this source". The
interface, the closed set of amendments it may return, and why it returns amendments rather than a
spec are in [10-ai-assistance.md](10-ai-assistance.md).

**[D] AI has no category default.** Every other category resolves "the adapter" through
`Registry.Default`; AI resolves one per function. Each AI function is assigned to at most one AI
adapter, one adapter may hold any number, and an assignment may name a model when the adapter
advertises `ChoosesModel` (R-259). `Registry.AIFor(function)` is the lookup, and a function with no
assignment is off. An install has **one AI adapter per provider** (a unique index on
`adapter_configs (kind) WHERE category = 'ai'`), because two adapters of one kind would differ only by
credential or model, and the assignment carries the model. Design 10 §9.

---

## 9. Registration

```go
type Registry struct { /* ... */ }

func (r *Registry) Register(a Adapter)
func (r *Registry) Get(ref string) (Adapter, error)
func (r *Registry) ByCategory(c Category) []Adapter
func (r *Registry) Default(c Category) (Adapter, error)
```

**[D]** Registration happens in `main` at startup, from compiled-in packages (R-253). Configured instances come from `adapter_configs` (§02 2.5), and from the `adapters:` section of the config file, which overrides a stored adapter with the same ID, a stored AI adapter of the same kind, and a stored default in the category (R-271, design 10 §7.1).

**[D]** CI enforces an import rule: nothing under `internal/adapter/` may import `internal/core/authz`, `internal/core/audit`, or `internal/core/state`. This is R-027 as a lint rule rather than a convention.

---

## 10. v1 implementations

| Category | Kind | Note |
|---|---|---|
| identity | `local` | username/password, argon2id |
| identity | `oidc` | any OpenID Connect provider by issuer; presets for Okta, Entra ID, Google Workspace, Keycloak, Authentik (§5) |
| identity | `saml` | SAML 2.0 SP; IdP metadata by URL (re-read daily) or pasted; same presets (§5) |
| routing | `loopback` | port mode, no TLS, laptop default |
| routing | `traefik` | subdomain and path, TLS; Pando runs it by default (§4.4) |
| routing | `cloudflare` | Cloudflare Tunnel; subdomain and path, TLS at Cloudflare's edge (§4.5) |
| builder | `buildkit` | rootless, containerized, no socket (R-111) |
| runtime | `docker` | enforces app egress through an internal network and a per-app gateway proxy (§2.1, R-187); container isolation class; `sandboxed` when `oci_runtime` names gVisor (`runsc`) or a Kata runtime (R-115), which the daemon must have registered or the adapter reports itself unavailable. A sandboxed trial run still reports whether the app started, but not its ports or writes — both are read from outside the container, and a sandbox hides them. Also drives rootless Podman through its Docker-compatible socket (see below). |
| runtime | `kubernetes` | the cluster Pando runs in, one namespace per app under a default-deny NetworkPolicy admitting only Pando's server pods; bare pods with `restartPolicy: Never` (R-151); headless Services the proxy reaches by name; PVCs set to `Retain` (R-204); unusable on a cluster whose network plugin does not enforce NetworkPolicy, which a canary checks (O-43). Runs the edge as a spread Deployment (R-174). `notes-kubernetes-runtime-issue-72.md` |
| secrets | `local` | encrypted at rest, key on disk (R-190) |
| backup | `local` | a filesystem path; retention owned by Pando |
| services | `docker` | postgres, mysql, redis in-bundle |
| notify | `console`; `smtp`, `slack`, `teams`, `discord`, `ntfy` | R-231, R-232, R-374 |
| ai | `anthropic` | performs the AI functions assigned to it, each on its own model if the assignment names one (design 10 §9). Not seeded — needs a credential. One per install, like any AI provider. |
| ai | `openai` | the same functions with OpenAI's models, through the Responses API (design 10 §6.2). Not seeded — needs a credential. |
| ai | `local` | the same functions with a model on the install's own hardware, through any OpenAI-compatible server such as Ollama (design 10 §6.3). Not seeded — needs a server and a model. |

**[P] Podman is the Docker adapter pointed at a different socket, not an adapter of its own.** Its
Docker-compatible API does what this adapter asks, and the adapter's integration suite passes against
rootless Podman 4.9 with the same tests it passes against Docker. A second adapter would have been a
copy of this one. What differs is how the two engines *answer*, and each difference had become a
wrong result before it was handled; they live in `internal/adapter/runtime/docker/engine.go`:

- Podman spells images fully qualified (`docker.io/library/alpine:3.20`) and local tags under
  `localhost/`. Compared as written, every Apply recreated its containers and no pulled image was ever
  released (R-224). References are compared in their familiar form.
- It returns an empty health status for a container with no health check, which read as unhealthy
  (R-221).
- Its one-shot stats carry no previous sample, and its system CPU counter is not the all-CPU total
  Docker's arithmetic divides by. CPU is sampled twice and measured against wall time (R-245).
- It refuses a taken address block in words of its own, and its image-load answer lacks the line
  ending the old parser trimmed.
- It accepts a `HostConfig.Runtime`, lists that runtime, and does not apply it. `oci_runtime` is
  therefore refused on Podman when the adapter is checked, and every container that runs app code is
  checked for the runtime it actually got before it starts (R-255).

Installing Pando itself on Podman — the Compose file mounts Docker's socket, and `docker compose
--build` cannot build through rootless Podman — is the install-topology question in design 00 §1.1,
still open; this note covers only the runtime adapter.
