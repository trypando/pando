package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/registrylimit"
)

// Kind is the adapter's kind string.
const Kind = "docker"

// Labels Pando puts on everything it creates, so Observe can find a bundle
// again and a human can tell what a container belongs to.
const (
	labelApp      = "io.pando.app"
	labelBundle   = "io.pando.bundle"
	labelWorkload = "io.pando.workload"
	labelManaged  = "io.pando.managed"

	// labelFiles digests the configuration files carried into this container
	// (spec.File). They are copied in after create and leave no trace in the
	// container's own configuration, so this is what makes a changed file a
	// container that no longer matches its plan.
	labelFiles = "io.pando.files"

	// labelTrial marks everything a trial run creates (R-097), so that a trial
	// interrupted by Pando restarting can be found and removed rather than
	// leaving a running copy of someone's app with nothing tracking it.
	labelTrial = "io.pando.trial"
)

// Adapter runs workloads as Docker containers.
//
// Its isolation class is `container`: a shared kernel. That is reported
// honestly rather than optimistically, because policy floors compare against it
// (R-024, R-114) and an adapter that overstated its class would let a hardened
// install run work it meant to exclude.
type Adapter struct {
	cli    *client.Client
	config Config

	// Volume sizes, reused briefly across usage readings (usage.go).
	usageMu          sync.Mutex
	volumeSizesCache map[string]int64
	volumeSizesAt    time.Time

	// subnetMu serializes choosing an app network's address block.
	subnetMu sync.Mutex

	// sampling bounds the usage samples in flight (usage.go).
	sampling     chan struct{}
	samplingOnce sync.Once

	// What the egress gateway runs from when it is Pando's own image, read
	// once from Pando's container (egress.go).
	selfMu      sync.Mutex
	selfKnown   bool
	selfGateway gatewayImage

	// The daemon's platform, read once (Capabilities).
	platformMu   sync.Mutex
	hostPlatform string

	// What app containers are limited to, read briefly (hosts.go).
	committed committedCache
}

// Config is the adapter's configuration.
type Config struct {
	// Host is the Docker endpoint. Empty uses the environment, which is what
	// the bundled Compose file relies on.
	Host string `json:"host,omitempty"`

	// TotalCPUMillis and TotalMemoryBytes let an operator tell Pando how much
	// of the machine it may use.
	//
	// Capacity is adapter-reported, never host-inspected (R-243): core does not
	// read /proc and has no concept of a host. When these are unset the adapter
	// asks the Docker daemon what it has, which is the same principle — the
	// adapter answers for itself.
	TotalCPUMillis   int   `json:"total_cpu_millis,omitempty"`
	TotalMemoryBytes int64 `json:"total_memory_bytes,omitempty"`
	TotalDiskBytes   int64 `json:"total_disk_bytes,omitempty"`

	// ProxyContainer is the container Pando's proxy runs in, which this adapter
	// attaches to every bundle network it creates.
	//
	// Empty means "this process's own container", detected from the hostname —
	// which is what Docker sets to the container ID, and what the bundled
	// Compose topology relies on.
	ProxyContainer string `json:"proxy_container,omitempty"`

	// NetworkPool is the IPv4 range app networks take their addresses from.
	// Empty uses 10.213.0.0/16; "off" leaves allocation to Docker's default
	// pool, which holds about thirty networks (see subnets.go). Any range up to
	// a /24 is accepted, so a wider one such as 10.208.0.0/12 holds more apps.
	NetworkPool string `json:"network_pool,omitempty"`

	// NetworkBlockBits is the size of each app network as a prefix length,
	// 24 to 29. Zero means 28: 13 containers, 4,096 networks in the default
	// pool. A bundle with more workloads than fit gets a larger block.
	NetworkBlockBits int `json:"network_block_bits,omitempty"`

	// OCIRuntime is the runtime Docker starts app containers with, by the name
	// the daemon registered it under: "runsc" for gVisor, "kata" for Kata
	// Containers. Empty is the daemon's default, which is runc.
	//
	// It decides the isolation class this adapter reports (isolationOf), and
	// that class is what host policy floors compare against (R-024, R-114).
	// So the class is derived from the name here, never stated by the operator,
	// and HealthCheck refuses a name the daemon does not have — a policy floor
	// of `sandboxed` satisfied by a runtime that was never installed would
	// admit exactly the work it was set to keep out.
	OCIRuntime string `json:"oci_runtime,omitempty"`

	// EgressGatewayImage is the image a restricted app's egress gateway runs
	// from (R-187, egress.go): any image with Pando's binary at
	// /usr/local/bin/pando. Empty is the image Pando's own container runs,
	// which is right for the shipped Compose file. Pando run on the host has
	// no container to take it from, and without this set the adapter reports
	// that it cannot restrict egress (R-186).
	EgressGatewayImage string `json:"egress_gateway_image,omitempty"`
}

// New builds an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryRuntime }

func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a.config); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The Docker runtime's configuration could not be read.", err)
		}
	}

	// The client negotiates the API version by default, which is what lets it
	// talk to Podman's older one.
	opts := []client.Opt{client.FromEnv}
	if a.config.Host != "" {
		opts = append(opts, client.WithHost(a.config.Host))
	}

	cli, err := client.New(opts...)
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not set up the connection to Docker.", err)
	}
	a.cli = cli
	return nil
}

func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.cli == nil {
		return errs.New(errs.AdapterUnavailable, "The Docker runtime has not been set up.")
	}
	if _, err := a.cli.Ping(ctx, client.PingOptions{}); err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Docker is not responding.", err)
	}

	if a.config.OCIRuntime != "" {
		// Podman lists runtimes it will not apply through this API (engine.go,
		// verifyRuntime), so asking it which runtimes it has proves nothing.
		// Refused here, where the planner turns it into a readable refusal
		// before anything is created, rather than at the first container.
		if v, err := a.cli.ServerVersion(ctx, client.ServerVersionOptions{}); err == nil && isPodman(v) {
			return errs.Newf(errs.AdapterUnavailable,
				"The Docker runtime is set to start apps with %q, and this is Podman, whose Docker-compatible API does not apply a container runtime.",
				a.config.OCIRuntime).
				WithRemedy("Clear the container runtime setting to run apps on Podman under its default runtime. " +
					"To run apps under gVisor or Kata, use Docker with the runtime registered in /etc/docker/daemon.json.")
		}

		res, err := a.cli.Info(ctx, client.InfoOptions{})
		if err != nil {
			return errs.Wrap(errs.AdapterUnavailable, "Could not ask Docker which container runtimes it has.", err)
		}
		if _, ok := res.Info.Runtimes[a.config.OCIRuntime]; !ok {
			return errs.Newf(errs.AdapterUnavailable,
				"The Docker runtime is set to start apps with %q, and Docker has no container runtime by that name.",
				a.config.OCIRuntime).
				WithRemedy("Install it and register it under \"runtimes\" in /etc/docker/daemon.json, then " +
					"restart Docker — for gVisor, `runsc install` does both. Or clear the container runtime " +
					"setting to use Docker's default.")
		}
	}
	return nil
}

// observes reports whether the trial run can see what an app did.
//
// Both observations look at the container from outside: ports from a sidecar
// sharing its network namespace, writes from Docker's diff of its filesystem
// layer. A sandboxed runtime keeps its own network stack and its own filesystem
// overlay, so from outside there is nothing to see — and an observation that
// reports "no ports" when there are some is worse than none. Without them
// detection asks the port question instead of answering it (R-097).
func (a *Adapter) observes() bool {
	return isolationOf(a.config.OCIRuntime) == spec.IsolationContainer
}

// runsUnder reports whether a container created under ociRuntime is under the
// runtime this adapter is configured for.
//
// Part of matchesPlan because the class Capabilities reports changes the moment
// the setting does, and the containers do not. Without it, switching to runsc
// and redeploying an app compared image and environment, found them unchanged,
// and left the app on runc while the adapter called it sandboxed — which a
// policy floor would then have believed (R-114).
//
// Docker records a container created with no runtime under its default's name,
// usually "runc", so an unset setting matches any runtime that shares the
// kernel and refuses only a sandbox: switching the setting off moves apps out
// of the sandbox on their next deploy, and a daemon whose default is crun is
// not a reason to recreate anything.
func (a *Adapter) runsUnder(ociRuntime string) bool {
	if a.config.OCIRuntime == "" {
		return isolationOf(ociRuntime) == spec.IsolationContainer
	}
	return ociRuntime == a.config.OCIRuntime
}

// isolationOf is the class an OCI runtime provides (R-115).
//
// Only runtimes known to put a boundary between the app and the host kernel
// raise it: gVisor intercepts system calls in a user-space kernel, and Kata
// runs each container in a lightweight VM. R-115 places both in `sandboxed`.
// Any other name — crun, a GPU runtime — shares the kernel like runc, and
// reporting more than that would let a hardened install run work it meant to
// exclude.
func isolationOf(ociRuntime string) spec.IsolationClass {
	name := strings.ToLower(ociRuntime)
	switch {
	case name == "runsc" || strings.HasPrefix(name, "runsc-"):
		return spec.IsolationSandboxed
	case strings.HasPrefix(name, "kata") || strings.Contains(name, ".kata."):
		return spec.IsolationSandboxed
	default:
		return spec.IsolationContainer
	}
}

// Capabilities reports what this adapter can do (R-254).
func (a *Adapter) Capabilities(ctx context.Context) (api.RuntimeCapabilities, error) {
	class := isolationOf(a.config.OCIRuntime)
	observes := a.observes()

	return api.RuntimeCapabilities{
		// Shared kernel, unless the configured runtime is a sandbox. Stated
		// honestly: a policy floor above this must exclude this adapter, and it
		// can only do that if the class is true.
		IsolationClass: class,

		SupportsPersistentVolumes: true,
		SupportsCarriedFiles:      true,
		SupportsExec:              true,
		SupportsMultipleWorkloads: true,
		SupportsPrivateNetwork:    true,
		SupportsResourceLimits:    true,

		// CPU and memory from the daemon's stats, disk from the container's
		// own layer and its volumes (R-245).
		ReportsUsage: true,

		// Recreate only, for now. Start-then-swap needs the proxy to repoint
		// between two live bundles, which is phase 5 work — claiming it here
		// would turn a plan-time refusal into a mid-deploy failure (R-145).
		SupportsStartThenSwap: false,

		// R-222, and honestly. Docker caps a container's log at creation and
		// cannot change it afterwards without recreating the container — which
		// the reconciler may not do because an unrelated app turned chatty.
		// It also cannot report how much log space an app is using, which is
		// why R-224's aggregate is enforced against committed caps rather than
		// measured bytes (O-16).
		LogRetention: api.LogRetentionCapability{
			SupportsSizeCap:          true,
			CanChangeWithoutRecreate: false,
			ReportsUsage:             false,
			MinBytes:                 2 * minDockerLogBytes,
		},

		// A single daemon can load an image from a stream, which is how a build
		// reaches the runtime without a registry (O-34). It can also pull a
		// build from the install's registry by digest, when one is configured
		// and the install asks for that (the image registry adapter's always).
		ImageDelivery: []api.ImageDelivery{api.ImageDeliveryImport, api.ImageDeliveryRegistry},

		// Traefik reads route files from a directory Pando writes and the
		// edge mounts (edge.go).
		EdgeConfig: []api.EdgeConfig{api.EdgeConfigSharedMount},

		// Only when Pando is itself a container on this daemon: then it can
		// start the helper that replaces it (R-359).
		SupportsSelfUpgrade: a.inspectSelf(ctx) != nil,

		MaxWorkloadsPerBundle: 0,

		// The trial run (R-097). Port observation works on any image, including
		// one with no shell, because the sockets are read from a sidecar sharing
		// the container's network namespace rather than by exec-ing inside it.
		// Write observation is ContainerDiff, which the daemon computes itself.
		// Neither sees into a sandbox (above).
		SupportsTrialRun:        true,
		SupportsPortObservation: observes,

		// The daemon's event stream, filtered to Pando's containers (O-52,
		// events.go).
		SupportsBundleEvents:     true,
		SupportsWriteObservation: observes,

		// An edge in front of Pando (R-174, edge.go).
		SupportsEdge: true,

		// An internal network and a gateway in front of it (R-187, egress.go)
		// — when there is an image to run the gateway from. Pando on the host
		// with none configured cannot, and says so: the planner then refuses a
		// restricted plan rather than deploying it unrestricted (R-186).
		SupportsEgressRestriction: a.egressGateway(ctx).ref != "",

		// What the daemon runs images for, so the planner can refuse an image
		// with no build for it before anything is pulled (issue #41).
		Platform: a.platform(ctx),
	}, nil
}

// platform is the daemon's os/arch, read once: it does not change while the
// daemon runs, and Capabilities is called on every plan. Empty when the daemon
// cannot be asked, which leaves the check to the pull.
func (a *Adapter) platform(ctx context.Context) string {
	a.platformMu.Lock()
	defer a.platformMu.Unlock()
	if a.hostPlatform != "" {
		return a.hostPlatform
	}
	res, err := a.cli.Info(ctx, client.InfoOptions{})
	if err != nil || res.Info.OSType == "" || res.Info.Architecture == "" {
		return ""
	}
	a.hostPlatform = strings.ToLower(res.Info.OSType) + "/" + normalizeArch(res.Info.Architecture)
	return a.hostPlatform
}

// normalizeArch names an architecture the way registries do; the daemon
// reports the kernel's name.
func normalizeArch(arch string) string {
	switch strings.ToLower(arch) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "arm":
		return "arm"
	}
	return strings.ToLower(arch)
}

// Capacity is adapter-reported (R-243).
func (a *Adapter) Capacity(ctx context.Context) (api.Capacity, error) {
	capacity := api.Capacity{Reported: time.Now().UTC()}

	res, err := a.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return api.Capacity{}, errs.Wrap(errs.AdapterUnavailable, "Could not read how much room Docker has.", err)
	}
	info := res.Info

	capacity.TotalCPUMillis = info.NCPU * 1000
	capacity.TotalMemoryBytes = info.MemTotal
	if a.config.TotalCPUMillis > 0 {
		capacity.TotalCPUMillis = a.config.TotalCPUMillis
	}
	if a.config.TotalMemoryBytes > 0 {
		capacity.TotalMemoryBytes = a.config.TotalMemoryBytes
	}
	capacity.TotalDiskBytes = a.config.TotalDiskBytes

	capacity.RunningWorkloads = info.ContainersRunning

	// The rest of what the daemon says about itself, for whoever is looking
	// at why it is short of room. Docker's names, not Pando's (R-243).
	capacity.Details = map[string]any{
		"server_version":     info.ServerVersion,
		"operating_system":   info.OperatingSystem,
		"kernel_version":     info.KernelVersion,
		"storage_driver":     info.Driver,
		"docker_root_dir":    info.DockerRootDir,
		"containers":         info.Containers,
		"containers_running": info.ContainersRunning,
		"containers_stopped": info.ContainersStopped,
		"images":             info.Images,
	}
	if a.config.OCIRuntime != "" {
		capacity.Details["oci_runtime"] = a.config.OCIRuntime
	}

	// What is left of the totals by the containers' own limits (hosts.go).
	// One machine is one place, so this is also the largest.
	capacity.LargestFit = a.largestFit(ctx, capacity)

	return capacity, nil
}

// Apply converges the bundle toward the plan.
//
// Idempotent by construction: each workload is compared against what exists and
// recreated only when it differs. Calling Apply with an already-satisfied plan
// touches nothing, which is what lets the reconciler call it freely.
func (a *Adapter) Apply(ctx context.Context, p api.BundlePlan) (api.BundleHandle, error) {
	defer a.forgetCommitted()
	if !p.Network.Private {
		// R-026. The field is checked rather than assumed so that a caller
		// that built a plan wrongly fails here instead of silently placing
		// workloads where other apps can reach them.
		return api.BundleHandle{}, errs.New(errs.AdapterFailed,
			"Pando will not start an app on a shared network.")
	}

	// R-186, R-187: restricted rules put the bundle on a network with no
	// route out and a gateway in front of it; anything else runs exactly as
	// it did before egress rules existed. See egress.go.
	restricted := p.Network.Egress.Restricted()
	var (
		gateway gatewayImage
		rules   string
		proxy   map[string]string
	)
	if restricted {
		gateway = a.egressGateway(ctx)
		if gateway.ref == "" {
			// The planner refuses this plan first (SupportsEgressRestriction),
			// so reaching here is a plan made against another adapter's
			// capabilities. Refused rather than run with the rules ignored.
			return api.BundleHandle{}, errs.New(errs.PlanCapabilityUnsupported,
				"This app's egress rules restrict where it may connect, and the Docker runtime has no image to run Pando's egress gateway from, so it cannot enforce them.").
				WithRemedy("Run Pando in a container, as the shipped Compose file does, or set the Docker runtime's egress_gateway_image to an image of Pando.")
		}
		var err error
		if rules, err = egressRulesJSON(p.Network.Egress); err != nil {
			return api.BundleHandle{}, err
		}
		proxy = proxyEnv(p)
	}

	networkID, err := a.ensureNetwork(ctx, p.BundleID, restricted, len(p.Workloads))
	if err != nil {
		return api.BundleHandle{}, err
	}

	// The gateway before any workload, so a workload that connects out the
	// moment it starts finds it there.
	if restricted {
		if err := a.ensureGateway(ctx, p, gateway, rules, networkID); err != nil {
			return api.BundleHandle{}, err
		}
	} else if err := a.removeGateway(ctx, p.BundleID); err != nil {
		return api.BundleHandle{}, err
	}

	for _, v := range p.Volumes {
		if _, err := a.CreateVolume(ctx, api.VolumeRequest{
			VolumeID: v.VolumeID, BundleID: p.BundleID, Name: v.Name,
		}); err != nil {
			return api.BundleHandle{}, err
		}
	}

	planned := make(map[string]api.WorkloadPlan, len(p.Workloads))
	for _, w := range p.Workloads {
		planned[w.Name] = w
	}
	for _, w := range ordered(p.Workloads) {
		a.waitForDependencies(ctx, p.BundleID, w, planned)
		if err := a.applyWorkload(ctx, p, w, networkID, workloadEnv(w, proxy)); err != nil {
			return api.BundleHandle{}, err
		}
	}

	// The network of the other posture, if the bundle just switched. Its
	// workloads have moved, so it is removed if nothing is left on it, and
	// left for ReclaimNetworks if only Pando is (see egress.go).
	a.removeIdleNetwork(ctx, workloadNetworkName(p.BundleID, !restricted))

	return api.BundleHandle{BundleID: p.BundleID, Handle: networkID}, nil
}

func (a *Adapter) applyWorkload(ctx context.Context, p api.BundlePlan, w api.WorkloadPlan, networkID string, envVars map[string]string) error {
	name := containerName(p.BundleID, w.Name)
	networkName := workloadNetworkName(p.BundleID, p.Network.Egress.Restricted())

	existing, err := a.findContainer(ctx, p.BundleID, w.Name)
	if err != nil {
		return err
	}
	if existing != nil {
		matches, err := a.matchesPlan(ctx, existing.ID, w, networkName, envVars)
		if err != nil {
			return err
		}
		if matches {
			if !strings.HasPrefix(existing.State, "running") {
				if _, err := a.cli.ContainerStart(ctx, existing.ID, client.ContainerStartOptions{}); err != nil {
					return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not start %q.", w.Name), err)
				}
			}
			return nil
		}
		if err := a.removeContainer(ctx, existing.ID); err != nil {
			return err
		}
	}

	if err := a.ensureImageWith(ctx, w.Image, w.PullAuth, forBundle(p.BundleID)); err != nil {
		return err
	}

	env := make([]string, 0, len(envVars))
	for k, v := range envVars {
		env = append(env, k+"="+v)
	}

	exposed := network.PortSet{}
	for _, port := range w.Ports {
		p, err := containerPort(port.Number, port.Protocol)
		if err != nil {
			return errs.Wrap(errs.ValidInvalid, fmt.Sprintf("%d is not a usable port.", port.Number), err)
		}
		exposed[p] = struct{}{}
	}

	mounts := make([]string, 0, len(w.Mounts))
	for _, m := range w.Mounts {
		binding := volumeName(p.BundleID, m.VolumeID) + ":" + m.Path
		if m.ReadOnly {
			binding += ":ro"
		}
		mounts = append(mounts, binding)
	}

	cfg := &container.Config{
		Image:        w.Image,
		Cmd:          w.Command,
		Entrypoint:   w.Entrypoint,
		WorkingDir:   w.WorkingDir,
		Env:          env,
		ExposedPorts: exposed,
		Labels: map[string]string{
			labelApp:      p.Labels["pando.app"],
			labelBundle:   p.BundleID,
			labelWorkload: w.Name,
			labelManaged:  "true",

			// What the carried files were, so that changing one is a change
			// this container does not match. They go in after create and
			// leave no trace in the container's configuration, so without
			// this a spec whose only edit was a Caddyfile would converge to
			// "already running" and the edit would never ship.
			labelFiles: fileDigest(w.Files),
		},
	}
	limitLabels(cfg.Labels, w.Resources.CPUMillis, w.Resources.MemoryBytes)
	if w.Health != nil {
		cfg.Healthcheck = healthConfig(w.Health)
	}

	hostCfg := &container.HostConfig{
		Binds: mounts,

		// No restart policy. Restarting a workload that stopped is the
		// reconciler's job, and it is the only thing that can do it to Pando's
		// rules: back off between attempts (R-149), give up at the threshold
		// (R-150), and then leave the app alone (R-151).
		//
		// `unless-stopped` put Docker in that seat instead, with no backoff and
		// no end. An app that could not start was restarted every two seconds
		// forever; Pando gave up on it, said "Pando has stopped trying to start
		// this app", and the restart count kept climbing underneath the
		// message. The loop also hid itself — a container restarted that often
		// reports Running at almost every instant the reconciler looks.
		//
		// A container that exits is started again by the next tick, which is
		// fifteen seconds rather than instant, and that is the trade: a paced
		// restart Pando knows about beats an instant one it does not.
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},

		// The isolation class Capabilities reported (R-114). Empty is the
		// daemon's default.
		Runtime: a.config.OCIRuntime,

		Resources: container.Resources{
			NanoCPUs: int64(w.Resources.CPUMillis) * 1_000_000,
			Memory:   w.Resources.MemoryBytes,
		},
		LogConfig: logConfig(w.LogBytes),
	}

	// No ports are published to the host. Workloads are reachable only inside
	// the bundle network, and traffic arrives through Pando's proxy (R-023,
	// R-026). Publishing here would be a bypass.
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			networkName: {NetworkID: networkID, Aliases: []string{w.Name}},
		},
	}

	created, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: cfg, HostConfig: hostCfg, NetworkingConfig: netCfg, Name: name,
	})
	if err != nil {
		return createFailure(w, err)
	}
	if err := a.verifyRuntime(ctx, created.ID, w.Name); err != nil {
		return err
	}

	// Configuration files, placed before the workload runs.
	//
	// Between create and start, which is the only moment they can go in: the
	// container's filesystem exists and nothing has read it yet. A volume
	// cannot do this — Docker will not mount a directory over a file in the
	// image — and a bind mount from the host would mean reading the repository
	// at deploy time, which R-020 forbids. The bytes come from the spec.
	for _, f := range w.Files {
		if err := a.placeFile(ctx, created.ID, f); err != nil {
			return errs.Wrap(errs.AdapterFailed,
				fmt.Sprintf("Could not put %s into %q.", f.Path, w.Name), err)
		}
	}

	if _, err := a.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not start %q.", w.Name), err)
	}
	return nil
}

// fileDigest summarizes a workload's carried files.
//
// Sorted by path, and covering the mode as well as the content: a file made
// executable is a different container from the same file that is not.
func fileDigest(files []api.FilePlan) string {
	if len(files) == 0 {
		return ""
	}

	sorted := append([]api.FilePlan(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", f.Path, f.Mode, len(f.Content))
		h.Write([]byte(f.Content))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// placeFile copies one file into a created container.
//
// CopyToContainer extracts a tar stream at a path in the container, so the
// archive is rooted at / and carries the file at its full path. The directories
// above it go in as entries of their own: Docker does not create a missing
// parent, and an image whose /etc/caddy does not exist yet is an ordinary
// image, not a broken one. An entry for a directory that already exists is a
// no-op at the mode these are written with.
func (a *Adapter) placeFile(ctx context.Context, containerID string, f api.FilePlan) error {
	mode := f.Mode
	if mode == 0 {
		mode = 0o644
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	clean := path.Clean(f.Path)
	for _, dir := range ancestors(path.Dir(clean)) {
		if err := tw.WriteHeader(&tar.Header{
			Name:     strings.TrimPrefix(dir, "/") + "/",
			Typeflag: tar.TypeDir,
			Mode:     0o755,
			ModTime:  time.Now(),
		}); err != nil {
			return err
		}
	}

	if err := tw.WriteHeader(&tar.Header{
		Name:    strings.TrimPrefix(clean, "/"),
		Mode:    int64(mode),
		Size:    int64(len(f.Content)),
		ModTime: time.Now(),
	}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(f.Content)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}

	_, err := a.cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{DestinationPath: "/", Content: &buf})
	return err
}

// ancestors lists a directory and everything above it, outermost first, so a
// tar stream creates them in an order extraction can follow.
func ancestors(dir string) []string {
	var out []string
	for d := path.Clean(dir); d != "/" && d != "." && d != ""; d = path.Dir(d) {
		out = append([]string{d}, out...)
	}
	return out
}

// createFailure says why the daemon refused, in the app's own terms.
//
// One refusal is worth naming. Storage Pando manages is a directory, and Docker
// will not mount a directory over a file that exists in the image, so a mount
// whose path inside the container is a file fails at create with
// "source /var/lib/docker/rootfs/overlayfs/a083.../etc/caddy/Caddyfile is not
// directory" — a path on the host that appears in nothing the person
// configured. Naming the mount instead gives them the line to remove (R-105).
//
// The compose importer now refuses such a mount at discovery, which is where it
// belongs. This is for the apps that already carry one, and for a mount typed
// in by hand.
func createFailure(w api.WorkloadPlan, err error) error {
	if text := err.Error(); strings.Contains(text, "not directory") ||
		strings.Contains(text, "not a directory") {
		// Which mount, read out of the daemon's own path: it is the
		// container's rootfs with the mount's path on the end, so the mount
		// that appears in it is the one that failed. Guessing from the path's
		// shape instead does not work — "Caddyfile" has no extension.
		var files []string
		for _, m := range w.Mounts {
			if strings.Contains(text, m.Path) {
				files = append(files, m.Path)
			}
		}
		if len(files) > 0 {
			return errs.Wrap(errs.AdapterFailed, fmt.Sprintf(
				"Could not create %q: its storage is mounted at %s, which is a file inside the image.",
				w.Name, strings.Join(files, " and ")), err).
				WithRemedy("Storage Pando manages is a directory and cannot stand in for a single " +
					"file. Remove that mount in the app's storage settings, and copy the file into " +
					"the image in its Dockerfile instead.")
		}
	}

	return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not create %q.", w.Name), err)
}

// Observe reports what exists. It never remediates (design 05 §2.1).
func (a *Adapter) Observe(ctx context.Context, ref api.BundleRef) (api.ObservedBundle, error) {
	containers, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelBundle+"="+ref.BundleID),
	})
	if err != nil {
		return api.ObservedBundle{}, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}

	observed := api.ObservedBundle{Exists: len(containers.Items) > 0}
	for _, c := range containers.Items {
		if isGateway(c.Labels) {
			// Pando's, not the app's: it is not a workload the plan names,
			// and reporting it as one would be drift nothing could fix.
			continue
		}
		inspect, err := a.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			// A container that vanished between list and inspect is drift the
			// reconciler should see, not an error that aborts the whole
			// observation.
			continue
		}

		w := api.ObservedWorkload{
			Name:    c.Labels[labelWorkload],
			Present: true,
			Running: inspect.Container.State.Running,

			// Docker reports both at once: a crash-looping container inspects
			// as Running=true, Restarting=true. Reporting only Running would
			// tell the reconciler a looping app is fine.
			Restarting: inspect.Container.State.Restarting,

			ImageDigest:  inspect.Container.Image,
			RestartCount: inspect.Container.RestartCount,
		}
		if started, err := time.Parse(time.RFC3339Nano, inspect.Container.State.StartedAt); err == nil {
			w.StartedAt = started
		}
		if !inspect.Container.State.Running {
			code := inspect.Container.State.ExitCode
			w.ExitCode = &code
		}

		// Healthy stays nil when there is no health check. "No signal" and
		// "unhealthy" are different states and must not collapse (R-221): an
		// app with no health check is running, not perpetually degraded.
		if reportsHealth(inspect.Container.State.Health) {
			healthy := inspect.Container.State.Health.Status == "healthy"
			w.Healthy = &healthy
		}

		observed.Workloads = append(observed.Workloads, w)
	}

	volumes, err := a.cli.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", labelBundle+"="+ref.BundleID),
	})
	if err == nil {
		for _, v := range volumes.Items {
			observed.Volumes = append(observed.Volumes, api.ObservedVolume{
				VolumeID: v.Labels["io.pando.volume"],
				Present:  true,
				Handle:   v.Name,
			})
		}
	}

	return observed, nil
}

func (a *Adapter) Stop(ctx context.Context, ref api.BundleRef) error {
	containers, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelBundle+"="+ref.BundleID),
	})
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}
	for _, c := range containers.Items {
		if _, err := a.cli.ContainerStop(ctx, c.ID, client.ContainerStopOptions{}); err != nil {
			return errs.Wrap(errs.AdapterFailed, "Could not stop the app.", err)
		}
	}
	return nil
}

// Destroy removes the bundle's containers and network.
//
// Volumes are kept unless explicitly asked otherwise: they outlive the apps
// that mount them (R-204), and destroying them is a separate, deliberate act.
func (a *Adapter) Destroy(ctx context.Context, ref api.BundleRef, opts api.DestroyOptions) error {
	defer a.forgetCommitted()
	containers, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelBundle+"="+ref.BundleID),
	})
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}
	for _, c := range containers.Items {
		if err := a.removeContainer(ctx, c.ID); err != nil {
			return err
		}
	}

	// The images it pulled, which carry no label (images.go). First, because
	// an image carries this app's claim as a tag, and a built image with a tag
	// besides its own cannot be removed without force.
	a.releaseImages(ctx, ref.BundleID)

	// The images built for this app, every one it ever had. A rebuild moves the
	// tag and leaves the previous image untagged, so they are found by the label
	// the builder put on them rather than by name. Without force: an image some
	// other container still uses is kept, and one that fails to go is not a
	// reason to fail the teardown (issue #55).
	if images, err := a.cli.ImageList(ctx, client.ImageListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", api.ImageLabelBundle+"="+ref.BundleID),
	}); err == nil {
		for _, img := range images.Items {
			_, _ = a.cli.ImageRemove(ctx, img.ID, client.ImageRemoveOptions{PruneChildren: true})
		}
	}

	if !opts.KeepVolumes {
		volumes, err := a.cli.VolumeList(ctx, client.VolumeListOptions{
			Filters: make(client.Filters).Add("label", labelBundle+"="+ref.BundleID),
		})
		if err == nil {
			for _, v := range volumes.Items {
				_, _ = a.cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{})
			}
		}
	}

	// The network is removed only if nothing is still attached to it, and
	// **Pando never detaches itself to make that true.**
	//
	// It used to. Pando is joined to every bundle network — that is how the
	// proxy reaches an app (R-023) — so removing one meant disconnecting
	// first, and on Docker Desktop disconnecting a running container drops its
	// published ports. Measured, not guessed: healthz on the host went 200,
	// disconnect, 000, and stayed there until the container was restarted while
	// Pando kept happily serving inside it. The janitor could take the server
	// off the network, which is far worse than the leak it was reclaiming.
	//
	// So a network whose only remaining endpoint is Pando is left for
	// reclaimNetworks to collect after the next restart, when the container
	// holding it is gone and the removal needs no disconnect at all. The
	// containers — which hold the memory and CPU — are already gone by here,
	// which is the part that matters.
	//
	// Every network the bundle may have: the ordinary one, and the two a
	// restricted bundle adds (egress.go). The gateway's outbound network has
	// only the gateway on it, which went with the containers above.
	for _, name := range []string{
		bundleNetworkName(ref.BundleID), internalNetworkName(ref.BundleID), outboundNetworkName(ref.BundleID),
	} {
		// Not found is the usual answer for two of the three, and one still
		// holding Pando is left behind on purpose — not a failure, because the
		// teardown did succeed at everything that costs the host something to
		// keep.
		_, _ = a.cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	}
	return nil
}

// ReclaimNetworks removes bundle networks that nothing is attached to.
//
// Called at startup, which is the one moment this is safe: a network held by a
// previous Pando container has a dead endpoint on it, so Docker removes it
// without anybody disconnecting anything. Doing the same while serving would
// mean detaching the running Pando, and on Docker Desktop that drops its
// published ports.
//
// The cost of the timing is honest and worth stating: a network belonging to an
// app deleted since the last restart is reclaimed at the next one, not
// immediately. Docker's default pool holds about thirty, so an install that
// deletes thirty apps between restarts can still run out — better than leaking
// them permanently, and the remedy is a restart rather than a docker command.
//
// owns says whether a bundle belongs to this installation. A network whose
// bundle it does not own is another install's on the same Docker host, and is
// left alone; nil owns everything.
func (a *Adapter) ReclaimNetworks(ctx context.Context, owns func(bundleID string) bool) (int, error) {
	networks, err := a.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", labelManaged+"=true"),
	})
	if err != nil {
		return 0, errs.Wrap(errs.AdapterUnavailable, "Could not list the app networks.", err)
	}

	reclaimed := 0
	for _, n := range networks.Items {
		// Inspect rather than trusting the list: NetworkList does not populate
		// Containers, so the list alone cannot tell an empty network from a
		// busy one.
		full, err := a.cli.NetworkInspect(ctx, n.ID, client.NetworkInspectOptions{})
		if err != nil || len(full.Network.Containers) > 0 {
			continue
		}
		// Empty is not the same as unused. A stopped container is not an
		// endpoint, so a stopped app's network looks empty — and removing it
		// left the app's containers pointing at a network that no longer
		// existed, unable to start again (issue #55). Only a network no
		// container belongs to, running or not, is reclaimed.
		bundle := full.Network.Labels[labelBundle]
		if !ownedBundle(owns, bundle) || a.networkHasContainers(ctx, bundle, full.Network.Name) {
			continue
		}
		if _, err := a.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err == nil {
			reclaimed++
		}
	}
	return reclaimed, nil
}

// ownedBundle reports whether a network labeled with bundle is this install's to
// manage. A network with no bundle label — a trial's, which removes its own —
// is nobody's to reclaim or join here.
func ownedBundle(owns func(string) bool, bundle string) bool {
	if bundle == "" {
		return false
	}
	return owns == nil || owns(bundle)
}

// networkHasContainers reports whether any container of the bundle, in any
// state, belongs to the network. An error counts as yes: keeping a network is
// always safe.
//
// Per network rather than per bundle, because a bundle that switched egress
// posture has moved its containers to another network (egress.go), and the
// one it left is exactly what this exists to collect. A container whose
// networks the daemon did not list counts as on every one.
func (a *Adapter) networkHasContainers(ctx context.Context, bundleID, networkName string) bool {
	list, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelBundle+"="+bundleID),
	})
	if err != nil {
		return true
	}
	for _, c := range list.Items {
		if c.NetworkSettings == nil || len(c.NetworkSettings.Networks) == 0 {
			return true
		}
		if _, ok := c.NetworkSettings.Networks[networkName]; ok {
			return true
		}
	}
	return false
}

// RejoinNetworks puts this Pando container back on the network of every app
// that is still running (R-023, R-025).
//
// The set of networks Pando's container belongs to *is* the set of apps its
// proxy can reach, and that membership belongs to a container, not to an
// install. Replacing the Pando container — an upgrade, a `compose up --build`,
// any recreate — therefore starts one that is on none of them, while every app
// container carries on running perfectly. The apps are up; nothing can reach
// them; the reconciler sees a converged world and does nothing, because from
// its side the world *is* converged. Every app answers 502 until something
// happens to redeploy it.
//
// ensureNetwork already re-attaches, but only on the way through a deploy,
// which is the one thing that is not going to happen to an app that is already
// running the spec it is pinned to. So the attachment has to be restored at the
// moment it was lost: startup.
//
// Ordered after ReclaimNetworks deliberately. Reclaim removes the networks of
// apps that no longer exist, and it recognizes them by their being empty —
// joining first would put an endpoint on every one of them and make each look
// busy, turning a reclaim into a leak.
//
// owns is as for ReclaimNetworks: another install's app network is not joined.
func (a *Adapter) RejoinNetworks(ctx context.Context, owns func(bundleID string) bool) (int, error) {
	networks, err := a.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", labelManaged+"=true"),
	})
	if err != nil {
		return 0, errs.Wrap(errs.AdapterUnavailable, "Could not list the app networks.", err)
	}

	// Three calls to the daemon a pass, whatever the number of apps, plus one
	// connect for each network this container is missing (issue #72). This
	// runs every fifteen seconds on every replica, and inspecting every app
	// network and listing its containers each time was three calls per app
	// per pass — about two hundred a second from each replica at 1,000 apps.
	//
	// Which networks this container is already on, from one inspect of
	// itself. Unknown — the inspect failed, or Pando is not in a container —
	// is an empty set, and every network goes on to the checks below as it
	// did before; attachProxy treats "already exists" and a container that is
	// not there as success.
	already := a.proxyNetworks(ctx)

	// Which networks have one of their app's own containers on them, from one
	// list of every app container.
	occupied, err := a.bundleNetworks(ctx)
	if err != nil {
		return 0, errs.Wrap(errs.AdapterUnavailable, "Could not list the app containers.", err)
	}

	joined := 0
	for _, n := range networks.Items {
		if already[n.Name] || already[n.ID] {
			continue
		}
		bundle := n.Labels[labelBundle]
		if n.Labels[labelEgressNetwork] == egressNetworkOutbound {
			// The network an egress gateway leaves through holds nothing
			// Pando's proxy reaches (egress.go).
			continue
		}
		// Endpoints are not the same as an app. With several Pando replicas
		// each joined to every app network, a deleted app's network still has
		// endpoints — the other replicas — and joining it would keep it from
		// ever emptying, so ReclaimNetworks could never collect it. Only a
		// network one of the app's own containers is on is worth joining
		// (issue #72). An empty one is not worth an endpoint either: the
		// app's next deploy attaches us, and until then there is nothing to
		// reach — and an endpoint would make it look busy to ReclaimNetworks.
		if !occupied.has(bundle, n.Name) {
			continue
		}
		// Last, because it is the check that may cost a database query: in a
		// steady state every network is in already and this is never asked.
		if !ownedBundle(owns, bundle) {
			continue
		}
		if err := a.attachProxy(ctx, n.ID); err != nil {
			// One unreachable app is not a reason to leave the rest
			// unreachable, and the app's own next deploy will try again.
			continue
		}
		joined++
	}
	return joined, nil
}

// proxyNetworks is the set of networks, by name and by ID, Pando's own
// container is on. Empty when that cannot be told.
func (a *Adapter) proxyNetworks(ctx context.Context) map[string]bool {
	self := a.config.ProxyContainer
	if self == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil
		}
		self = host
	}
	inspect, err := a.cli.ContainerInspect(ctx, self, client.ContainerInspectOptions{})
	if err != nil || inspect.Container.NetworkSettings == nil {
		return nil
	}
	out := map[string]bool{}
	for name, ep := range inspect.Container.NetworkSettings.Networks {
		out[name] = true
		if ep != nil && ep.NetworkID != "" {
			out[ep.NetworkID] = true
		}
	}
	return out
}

// occupancy records which networks hold one of their bundle's own containers.
type occupancy struct {
	onNetwork map[string]map[string]bool // bundle -> network name
	// unknown are bundles with a container whose networks were not reported,
	// which counts as being on every one of the bundle's networks: keeping a
	// network is always safe (networkHasContainers).
	unknown map[string]bool
}

func (o occupancy) has(bundle, network string) bool {
	if bundle == "" {
		return false
	}
	return o.unknown[bundle] || o.onNetwork[bundle][network]
}

// bundleNetworks lists every container that belongs to a bundle, in any state,
// once, and records which networks each bundle's containers are on.
func (a *Adapter) bundleNetworks(ctx context.Context) (occupancy, error) {
	list, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelBundle),
	})
	if err != nil {
		return occupancy{}, err
	}
	o := occupancy{onNetwork: map[string]map[string]bool{}, unknown: map[string]bool{}}
	for _, c := range list.Items {
		bundle := c.Labels[labelBundle]
		if bundle == "" {
			continue
		}
		if c.NetworkSettings == nil || len(c.NetworkSettings.Networks) == 0 {
			o.unknown[bundle] = true
			continue
		}
		if o.onNetwork[bundle] == nil {
			o.onNetwork[bundle] = map[string]bool{}
		}
		for name := range c.NetworkSettings.Networks {
			o.onNetwork[bundle][name] = true
		}
	}
	return o, nil
}

func (a *Adapter) CreateVolume(ctx context.Context, req api.VolumeRequest) (api.VolumeHandle, error) {
	name := volumeName(req.BundleID, req.VolumeID)
	_, err := a.cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name: name,
		Labels: map[string]string{
			labelBundle:       req.BundleID,
			labelManaged:      "true",
			"io.pando.volume": req.VolumeID,
		},
	})
	if err != nil {
		return api.VolumeHandle{}, errs.Wrap(errs.AdapterFailed, "Could not create storage for the app.", err)
	}
	return api.VolumeHandle{VolumeID: req.VolumeID, Handle: name}, nil
}

func (a *Adapter) DestroyVolume(ctx context.Context, h api.VolumeHandle) error {
	if _, err := a.cli.VolumeRemove(ctx, h.Handle, client.VolumeRemoveOptions{}); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not remove the storage.", err)
	}
	return nil
}

// SnapshotVolume and RestoreVolume arrive with phase 9.
//
// They return a plan-time-shaped error rather than a silent no-op, because a
// backup that quietly does nothing is worse than one that refuses.
// SnapshotVolume and RestoreVolume are in volumes_backup.go (R-212).

// ImportImage loads an image tarball into the daemon and returns the loaded
// image's ID.
//
// The reference is read back from the daemon's own response rather than
// assumed, because the tag the build asked for and the tag the daemon actually
// recorded are not guaranteed to match, and running the wrong one would be
// silent.
//
// The ID, not the tag, is what is returned: it is content-addressed, so what a
// deployment records cannot be changed by a later build. A build refused after
// it was loaded (by the security scan or the port check) used to move
// pando/<app>:latest, and a workload recreated afterwards ran the refused image
// (R-146).
func (a *Adapter) ImportImage(ctx context.Context, r io.Reader) (string, error) {
	resp, err := a.cli.ImageLoad(ctx, r, client.ImageLoadWithQuiet(true))
	if err != nil {
		return "", errs.Wrap(errs.AdapterFailed, "Could not load the built image.", err)
	}
	defer func() { _ = resp.Close() }()

	body, err := io.ReadAll(resp)
	if err != nil {
		return "", errs.Wrap(errs.AdapterFailed, "Could not load the built image.", err)
	}

	ref := parseLoadedRef(string(body))
	if ref == "" {
		return "", errs.New(errs.AdapterFailed, "The built image could not be loaded.").
			WithRemedy("Check the build logs — the image may not have been produced correctly.")
	}
	inspect, err := a.cli.ImageInspect(ctx, ref)
	if err != nil || inspect.ID == "" {
		return "", errs.Wrap(errs.AdapterFailed, "The built image was loaded, and Pando could not read its ID.", err).
			WithRemedy("Check that the Docker daemon is responding, then deploy again.")
	}
	a.pruneBuildTags(ctx, ref)
	return inspect.ID, nil
}

// parseLoadedRef pulls the image reference out of Docker's load output, which
// is a stream of JSON objects whose stream field reads
// "Loaded image: name:tag".
//
// Decoded as JSON rather than cut out of the text. Cutting it out trimmed the
// exact ending Docker writes — an escaped newline before the closing quote —
// and Podman writes no newline there, so the reference came back with `"}`
// on the end and every built image failed to start as an invalid reference.
func parseLoadedRef(body string) string {
	dec := json.NewDecoder(strings.NewReader(body))
	for {
		var msg struct {
			Stream string `json:"stream"`
		}
		if err := dec.Decode(&msg); err != nil {
			break
		}
		if ref, ok := loadedRef(msg.Stream); ok {
			return ref
		}
	}
	// Not a JSON stream: read it as text.
	for _, line := range strings.Split(body, "\n") {
		if ref, ok := loadedRef(line); ok {
			return ref
		}
	}
	return ""
}

func loadedRef(s string) (string, bool) {
	const marker = "Loaded image: "
	i := strings.Index(s, marker)
	if i < 0 {
		return "", false
	}
	ref := strings.TrimSpace(s[i+len(marker):])
	return ref, ref != ""
}

func (a *Adapter) Logs(ctx context.Context, ref api.WorkloadRef, opts api.LogOptions) (io.ReadCloser, error) {
	c, err := a.findContainer(ctx, ref.BundleID, ref.Workload)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errs.Newf(errs.NotFound, "There is nothing running called %q.", ref.Workload)
	}

	logOpts := client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: opts.Follow}
	if !opts.Since.IsZero() {
		logOpts.Since = opts.Since.Format(time.RFC3339)
	}
	if opts.Tail > 0 {
		logOpts.Tail = fmt.Sprint(opts.Tail)
	}

	rc, err := a.cli.ContainerLogs(ctx, c.ID, logOpts)
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not read the app's logs.", err)
	}

	// Docker frames the output of a container that has no TTY: an 8-byte header
	// before every chunk, saying which stream it came from and how long it is.
	// Pando creates every workload without one (see apply), so this stream
	// always carries that framing, and a caller copying it to a response body —
	// which is exactly what the logs endpoint does — puts control bytes through
	// the middle of the log somebody is reading.
	//
	// trial.go has a strip-it-from-a-buffer version of this for crash capture.
	// This is the streaming one: stdcopy unpicks the frames as they arrive, so
	// a followed log stays live.
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		_ = rc.Close()
		// A closed reader ends the copy with an error that is not one: the
		// caller hung up, which is how following a log always ends.
		_ = pw.CloseWithError(err)
	}()
	return demuxed{PipeReader: pr, source: rc}, nil
}

// demuxed is the unframed stream, and closes the framed one behind it.
//
// Closing only the pipe would leave the connection to the daemon open and the
// goroutine copying into a reader nobody is holding — on a followed log, for as
// long as the container keeps printing.
type demuxed struct {
	*io.PipeReader
	source io.Closer
}

func (d demuxed) Close() error {
	_ = d.source.Close()
	return d.PipeReader.Close()
}

// Exec opens a session. Core has already checked app.exec, consulted policy, and
// written the audit event before this is called (R-084, R-085, R-228) — the
// adapter does not authorize.
func (a *Adapter) Exec(ctx context.Context, ref api.WorkloadRef, req api.ExecRequest) (api.ExecSession, error) {
	c, err := a.findContainer(ctx, ref.BundleID, ref.Workload)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errs.Newf(errs.NotFound, "There is nothing running called %q.", ref.Workload)
	}

	env := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}

	created, err := a.cli.ExecCreate(ctx, c.ID, client.ExecCreateOptions{
		Cmd:          req.Command,
		TTY:          req.TTY,
		Env:          env,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not open a terminal in the app.", err)
	}

	attached, err := a.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: req.TTY})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not open a terminal in the app.", err)
	}

	return &execSession{cli: a.cli, execID: created.ID, hijacked: attached.HijackedResponse}, nil
}

// Upstream is the workload's container name on its bundle network (R-023).
//
// The name, not the workload's alias. Pando's container is joined to every
// bundle network (attachProxy), so "web" would mean a different container on
// each of them; the container name is unique across all of them. It is also a
// name rather than an address, so it survives the container being recreated.
//
// Computed, not looked up: this runs on every proxied request.
func (a *Adapter) Upstream(_ context.Context, ref api.WorkloadRef, port int) (api.Upstream, error) {
	if port <= 0 {
		return api.Upstream{}, errs.Newf(errs.ValidInvalid, "%d is not a usable port.", port)
	}
	return api.Upstream{
		URL: fmt.Sprintf("http://%s:%d", containerName(ref.BundleID, ref.Workload), port),
	}, nil
}

var _ api.RuntimeAdapter = (*Adapter)(nil)

// --- helpers ---------------------------------------------------------------

// ensureNetwork makes the network a bundle's workloads run on and joins Pando
// to it. restricted chooses which: the ordinary bridge, or the internal one a
// restricted bundle runs on with no route out (egress.go). workloads sizes a
// new network's address block (blockBitsFor).
func (a *Adapter) ensureNetwork(ctx context.Context, bundleID string, restricted bool, workloads int) (string, error) {
	name := workloadNetworkName(bundleID, restricted)

	existing, err := a.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("name", name),
	})
	if err == nil {
		for _, n := range existing.Items {
			if n.Name == name {
				if restricted && !n.Internal {
					// A network by this name with a route out is not one
					// Pando made, and running a restricted app on it would
					// enforce nothing.
					return "", errs.Newf(errs.AdapterFailed,
						"A Docker network named %s exists and has a route out, so this app's egress rules could not be enforced on it.", name).
						WithRemedy("Remove that network with: docker network rm " + name + " — then deploy the app again.")
				}
				// Attached on every pass, not only when the network is new.
				//
				// Pando's container is joined to each app's private network —
				// that is the only way the proxy can reach an app (R-023). A
				// replaced Pando container is a *different* container, so a
				// network created by the old one has the old one attached and
				// the new one nowhere. Attaching only at creation therefore
				// meant every upgrade silently cut Pando off from every
				// existing app, and the app came back only if something
				// recreated its network.
				//
				// Idempotent: Docker answers "already exists" and attachProxy
				// treats that as success.
				if err := a.attachProxy(ctx, n.ID); err != nil {
					return "", err
				}
				return n.ID, nil
			}
		}
	}

	// An unrestricted bundle's network has a route out (Internal: false),
	// exactly as before egress rules existed (R-186); the isolation that
	// matters for R-025 is that each bundle gets its own network, so no app
	// can reach another's. A restricted bundle's has none, and its only way
	// out is the gateway (R-187).
	opts := client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: map[string]string{labelBundle: bundleID, labelManaged: "true"},
	}
	if restricted {
		opts.Internal = true
		opts.Labels[labelEgressNetwork] = egressNetworkInternal
	}
	created, err := a.createNetwork(ctx, name, a.blockBitsFor(workloads), opts)
	if err != nil {
		return "", networkFailure(err, "Could not set up the app's private network.")
	}

	if err := a.attachProxy(ctx, created.ID); err != nil {
		return "", err
	}
	return created.ID, nil
}

// joinedNetworkGwPriority is the gateway priority Pando's own container
// joins a bundle's network with: below the default of 0, so a joined network
// never becomes the container's default gateway.
//
// Docker gives the default gateway to the endpoint with the highest priority
// and breaks a tie by network name. "pando-app_…" sorts before the install's
// own "pando_default", so the first deploy moved Pando's default route onto
// that app's network: Docker re-bound the container's published ports while
// it did, and the API and every app port stopped answering for a second or two
// — long enough for the acceptance suite to declare the server gone. Pando's
// own traffic out (clones, pulls) then left through an app's network.
const joinedNetworkGwPriority = -1

// attachProxy joins Pando's own container to a bundle network.
//
// Every app sits on its own private network so no app can reach another
// (R-025), and nothing publishes a host port (R-026). That leaves exactly one
// way in — through Pando's proxy — which is R-023 made structural rather than
// promised. But it only works if the proxy can actually reach the bundle, and
// it cannot unless it is on that network too.
//
// So the set of networks Pando's container belongs to *is* the set of apps it
// can reach. Nothing else is joined to them.
//
// Attaching is the adapter's job rather than core's: how a workload becomes
// reachable is exactly the provider vocabulary core must never learn (R-251).
func (a *Adapter) attachProxy(ctx context.Context, networkID string) error {
	container := a.config.ProxyContainer
	if container == "" {
		// Docker sets a container's hostname to its own short ID.
		host, err := os.Hostname()
		if err != nil {
			//nolint:nilerr // Not knowing our own hostname means we are not in a
			// container, which is the same case as IsNotFound below: the app
			// deploys, the proxy cannot reach it from here, and that is a
			// local-development limitation rather than a deployment failure.
			return nil
		}
		container = host
	}

	_, err := a.cli.NetworkConnect(ctx, networkID, client.NetworkConnectOptions{
		Container: container, EndpointConfig: &network.EndpointSettings{GwPriority: joinedNetworkGwPriority},
	})
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "already exists"):
		return nil
	case cerrdefs.IsNotFound(err):
		// Pando is not running as a container — a developer running the binary
		// on the host. The app still deploys; the proxy simply cannot reach it
		// from here, which is a local-development limitation rather than a
		// deployment failure.
		return nil
	default:
		return errs.Wrap(errs.AdapterFailed,
			"Could not connect Pando to the app's network, so traffic could not reach it.", err)
	}
}

// logConfig caps a container's logs at creation (R-222, R-223).
//
// Set here and nowhere else, because Docker cannot change it on a running
// container — that is what LogRetention.CanChangeWithoutRecreate says, and why
// a changed cap takes effect on the next deploy rather than immediately.
//
// max-file is 2 rather than 1: Docker rotates to a second file before deleting
// the first, so a cap of N with one file keeps somewhere between 0 and N bytes,
// and with two keeps between N/2 and N. Half the cap is a floor worth having
// when the logs are what somebody is reading to find out why a deploy failed.
// The per-file size is therefore half the app's budget, so the total stays
// under it.
func logConfig(capBytes int64) container.LogConfig {
	if capBytes <= 0 {
		// No cap asked for. Left as the daemon's default rather than invented
		// here: an adapter that silently imposed a limit nobody configured
		// would lose logs for a reason nothing explains.
		return container.LogConfig{}
	}

	perFile := capBytes / 2
	if perFile < minDockerLogBytes {
		perFile = minDockerLogBytes
	}
	return container.LogConfig{
		Type: "json-file",
		Config: map[string]string{
			"max-size": strconv.FormatInt(perFile, 10) + "b",
			"max-file": "2",
		},
	}
}

// minDockerLogBytes is the smallest per-file cap worth setting.
//
// Below about this, rotation happens so often that the log is useless for
// reading a failure — which is the thing logs are for.
const minDockerLogBytes = 1 << 20 // 1 MiB

// ensureImage makes sure an image is on the host, pulling it if not.
//
// The claim says who the image is for (see images.go): an app's workloads
// claim it for their bundle, so deleting the app can release it; a trial
// claims nothing but still marks an image it had to fetch, so the app deployed
// after it can; Pando's own helper images pass the zero claim and are kept.
func (a *Adapter) ensureImage(ctx context.Context, ref string, claim imageClaim) error {
	return a.ensureImageWith(ctx, ref, nil, claim)
}

// ensureImageWith is ensureImage for an image that may be private: auth is
// what core resolved from the app's registry credential, nil for anonymous.
func (a *Adapter) ensureImageWith(ctx context.Context, ref string, auth *api.RegistryAuth, claim imageClaim) error {
	if ref == "" {
		return errs.New(errs.ValidInvalid, "This workload has no image to run.")
	}
	if _, err := a.cli.ImageInspect(ctx, ref); err == nil {
		a.claimImage(ctx, ref, claim, false)
		return nil
	}

	// Tried again when the daemon's own content store trips over a pull that
	// ran beside another — Docker Desktop's containerd answers concurrent pulls
	// with "lease does not exist" and failed ingest renames, and the same pull
	// a moment later succeeds (issue #55). A registry's refusal is final.
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = a.pullWith(ctx, ref, auth)
		if err == nil || !pullTransient(err) || ctx.Err() != nil || attempt == 3 {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
	}
	if err != nil && registrylimit.Mentioned(err.Error()) {
		signed := registrylimit.Anonymous
		if auth != nil && (auth.Username != "" || !auth.IdentityToken.IsZero()) {
			signed = registrylimit.SignedIn
		}
		r := registrylimit.FromText(err.Error(), ref)
		r.Signed = signed
		return r.Error(time.Now(), err)
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not fetch the image %q.", ref), err).
			WithRemedy("Check the image name and tag, that the registry is reachable from this host, " +
				"and that the image is published for this host's CPU architecture.")
	}
	a.claimImage(ctx, ref, claim, true)
	return nil
}

// pullTransient reports a pull that failed inside the daemon rather than at
// the registry.
func pullTransient(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"lease does not exist", "failed commit on ref", "failed to extract layer", "unexpected eof",
		"connection reset", "i/o timeout", "tls handshake timeout"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func (a *Adapter) pullOnce(ctx context.Context, ref string) error {
	return a.pullWith(ctx, ref, nil)
}

func (a *Adapter) pullWith(ctx context.Context, ref string, auth *api.RegistryAuth) error {
	opts := client.ImagePullOptions{}
	if auth != nil && (auth.Username != "" || !auth.IdentityToken.IsZero()) {
		encoded, err := registryAuthHeader(auth)
		if err != nil {
			return err
		}
		opts.RegistryAuth = encoded
	}
	rc, err := a.cli.ImagePull(ctx, ref, opts)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	return pullError(rc)
}

// registryAuthHeader encodes credentials the way the daemon reads them: JSON,
// base64url. Handed to the daemon for this one pull and kept nowhere — the
// daemon does not store credentials passed this way, and Pando does not write
// them to the daemon's config.
func registryAuthHeader(auth *api.RegistryAuth) (string, error) {
	// G117: this struct exists to carry the password to the daemon, which is
	// the only place it goes; it is never logged or stored.
	raw, err := json.Marshal(struct { //nolint:gosec
		Username      string `json:"username,omitempty"`
		Password      string `json:"password,omitempty"`
		ServerAddress string `json:"serveraddress,omitempty"`
		IdentityToken string `json:"identitytoken,omitempty"`
	}{auth.Username, auth.Password.Reveal(), auth.Registry, auth.IdentityToken.Reveal()})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(raw), nil
}

// pullError reads a pull's progress stream to the end and returns the error it
// reported, if any.
//
// The daemon answers a pull with 200 and reports failure inside the stream —
// an unknown tag, a denied registry, no image for this architecture. The stream
// was drained and discarded, so a failed pull surfaced later as "No such image"
// from the create, with the reason gone (issue #55).
func pullError(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if msg.ErrorDetail.Message != "" {
			return errors.New(msg.ErrorDetail.Message)
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
	}
}

type containerSummary struct {
	ID     string
	State  string
	Labels map[string]string
}

func (a *Adapter) findContainer(ctx context.Context, bundleID, workload string) (*containerSummary, error) {
	list, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelBundle+"="+bundleID).Add("label", labelWorkload+"="+workload),
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &containerSummary{ID: list.Items[0].ID, State: string(list.Items[0].State), Labels: list.Items[0].Labels}, nil
}

// matchesPlan reports whether a running container already satisfies the plan.
//
// Compared on image, command, and environment. Environment is included because
// a rotated secret must cause a recreate (R-193) and the container's own config
// is the only place the adapter can see it — core detects the same drift
// state-side by fingerprint, and this is the adapter's half. env is the
// environment the container should have: the plan's, with the egress proxy
// variables when the bundle is restricted (workloadEnv).
//
// And on network: a bundle that switched egress posture runs on another
// network (egress.go), and a container still on the old one is on a network
// with the wrong route out.
func (a *Adapter) matchesPlan(ctx context.Context, containerID string, w api.WorkloadPlan, networkName string, env map[string]string) (bool, error) {
	inspect, err := a.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return false, errs.Wrap(errs.AdapterUnavailable, "Could not read the app's configuration.", err)
	}
	if inspect.Container.Config == nil {
		return false, nil
	}
	if !sameImage(inspect.Container.Config.Image, w.Image) {
		return false, nil
	}
	if inspect.Container.Config.Labels[labelFiles] != fileDigest(w.Files) {
		return false, nil
	}
	if inspect.Container.HostConfig != nil && !a.runsUnder(inspect.Container.HostConfig.Runtime) {
		return false, nil
	}

	if inspect.Container.NetworkSettings != nil && inspect.Container.NetworkSettings.Networks != nil {
		if _, ok := inspect.Container.NetworkSettings.Networks[networkName]; !ok {
			return false, nil
		}
	}

	existing := map[string]string{}
	for _, kv := range inspect.Container.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			existing[k] = v
		}
	}
	for k, want := range env {
		if existing[k] != want {
			return false, nil
		}
	}
	return true, nil
}

// removeContainer removes a container and its anonymous volumes.
//
// An image that declares VOLUME — redis, mysql, postgres and many more — gets
// an unnamed volume from Docker whenever nothing is mounted there, and it
// outlived the container: one more on every redeploy, and every one left after
// the app was deleted (issue #55, R-224). The next container never reattached
// it, so it held nothing anybody could reach. RemoveVolumes takes only those;
// the named volumes Pando manages are kept (R-204).
func (a *Adapter) removeContainer(ctx context.Context, id string) error {
	_, err := a.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return errs.Wrap(errs.AdapterFailed, "Could not remove the old container.", err)
	}
	return nil
}

// dependencyWait bounds how long a workload waits for what it depends on to
// report healthy. A dependency that is still not healthy after it is started
// anyway: the deploy's own wait restarts what then stops, and a bundle that
// never comes up is reported by that wait, not hidden here.
const dependencyWait = 2 * time.Minute

// waitForDependencies holds a workload back until every dependency that has a
// health check reports healthy (R-096).
//
// Starting dependencies first was only half of it. A backend started the
// moment its database's container existed, while the database was still
// initializing, and crashed on a refused connection — and a proxy in front of
// it then failed on a host that had gone. This is compose's `depends_on:
// condition: service_healthy`, and it is also what makes the health check on a
// provisioned database mean anything: the app waits for the database to accept
// connections (issue #55). A dependency with no health check has nothing to
// wait for beyond being started, which ordering already does.
func (a *Adapter) waitForDependencies(ctx context.Context, bundleID string, w api.WorkloadPlan, planned map[string]api.WorkloadPlan) {
	for _, dep := range w.DependsOn {
		if d, ok := planned[dep]; !ok || d.Health == nil {
			continue
		}
		deadline := time.Now().Add(dependencyWait)
		for time.Now().Before(deadline) {
			if a.settled(ctx, bundleID, dep) {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// settled reports a dependency that is healthy, or that there is no point
// waiting for: gone, stopped, or reporting no health at all.
func (a *Adapter) settled(ctx context.Context, bundleID, workload string) bool {
	c, err := a.findContainer(ctx, bundleID, workload)
	if err != nil || c == nil {
		return true
	}
	inspect, err := a.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
	if err != nil || inspect.Container.State == nil || !inspect.Container.State.Running || !reportsHealth(inspect.Container.State.Health) {
		return true
	}
	return inspect.Container.State.Health.Status == "healthy"
}

// ordered sorts workloads so dependencies start first (R-096).
//
// A cycle is impossible here — validation rejects one — so a workload whose
// dependencies cannot be satisfied simply lands at the end rather than hanging.
func ordered(workloads []api.WorkloadPlan) []api.WorkloadPlan {
	byName := make(map[string]api.WorkloadPlan, len(workloads))
	for _, w := range workloads {
		byName[w.Name] = w
	}

	var out []api.WorkloadPlan
	placed := map[string]bool{}

	var place func(api.WorkloadPlan)
	place = func(w api.WorkloadPlan) {
		if placed[w.Name] {
			return
		}
		placed[w.Name] = true
		for _, dep := range w.DependsOn {
			if d, ok := byName[dep]; ok {
				place(d)
			}
		}
		out = append(out, w)
	}
	for _, w := range workloads {
		place(w)
	}
	return out
}

func healthConfig(h *api.HealthPlan) *container.HealthConfig {
	cfg := &container.HealthConfig{Retries: h.Retries}
	switch {
	case len(h.Command) > 0:
		cfg.Test = append([]string{"CMD"}, h.Command...)
	case h.Path != "" && h.Port > 0:
		cfg.Test = []string{"CMD-SHELL", httpProbe(h.Port, h.Path)}
	case h.Port > 0:
		cfg.Test = []string{"CMD-SHELL", fmt.Sprintf("nc -z 127.0.0.1 %d || exit 1", h.Port)}
	default:
		return nil
	}
	if h.IntervalSeconds > 0 {
		cfg.Interval = time.Duration(h.IntervalSeconds) * time.Second
	}
	if h.TimeoutSeconds > 0 {
		cfg.Timeout = time.Duration(h.TimeoutSeconds) * time.Second
	}
	return cfg
}

// httpProbe asks the app whether it is serving, with whatever the image has.
//
// It ran `wget` alone, which is not in every image — and an image without it
// answered "/bin/sh: 1: wget: not found" every thirty seconds until the deploy
// gave up, for an app that was serving perfectly well. The image belongs to
// the person who wrote the app, not to Pando, so the probe asks what is there
// rather than assuming: curl, then wget, then a TCP connection, which says
// less than an HTTP status but says it without needing anything installed.
//
// The last rung is bash's /dev/tcp, so it costs no package either. An image
// with none of the three is one this cannot probe, and it reports unhealthy —
// which is a worse answer than "unknown" and is why the rungs come first.
func httpProbe(port int, path string) string {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	return fmt.Sprintf(
		"if command -v curl >/dev/null 2>&1; then curl -fsS -o /dev/null %s; "+
			"elif command -v wget >/dev/null 2>&1; then wget --spider -q %s; "+
			"elif command -v nc >/dev/null 2>&1; then nc -z 127.0.0.1 %d; "+
			"else (exec 3<>/dev/tcp/127.0.0.1/%d) 2>/dev/null; fi || exit 1",
		url, url, port, port)
}

func protocolOf(p string) string {
	if p == "tcp" || p == "udp" {
		return p
	}
	return "tcp"
}

// containerPort is a port number and protocol as the engine names it. Parsed
// rather than built, so a number outside 1-65535 is refused instead of
// wrapping into some other port.
func containerPort(number int, protocol string) (network.Port, error) {
	return network.ParsePort(strconv.Itoa(number) + "/" + protocolOf(protocol))
}

func bundleNetworkName(bundleID string) string { return "pando-" + bundleID }
func volumeName(bundleID, volumeID string) string {
	return "pando-" + bundleID + "-" + volumeID
}
func containerName(bundleID, workload string) string {
	return "pando-" + bundleID + "-" + workload
}

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryRuntime,
		Kind:        Kind,
		Name:        "Docker",
		Description: "Runs apps as containers on a Docker host.",
		IDPrefix:    "rt_",
		Fields: []api.Field{
			{Key: "host", Label: "Docker host", Type: "string", Help: "The Docker endpoint. Empty uses the environment, which the bundled Compose file relies on.", Default: "DOCKER_HOST, or the local socket", Advanced: true},
			{Key: "total_cpu_millis", Label: "CPU available", Type: "int", Help: "Thousandths of a core Pando may allocate.", Default: "The whole machine", Advanced: true},
			{Key: "total_memory_bytes", Label: "Memory available", Type: "int", Help: "Bytes Pando may allocate.", Default: "The whole machine", Advanced: true},
			{Key: "total_disk_bytes", Label: "Disk available", Type: "int", Help: "Bytes of disk Pando may allocate.", Default: "No limit", Advanced: true},
			{Key: "network_pool", Label: "App network range", Type: "string", Help: "The IPv4 range each app's private network takes its addresses from. A /16 holds about 4,000 apps; a wider range such as 10.208.0.0/12 holds more. \"off\" uses Docker's own pool, which holds about 30 networks.", Default: defaultNetworkPool, Advanced: true},
			{Key: "network_block_bits", Label: "App network size", Type: "int", Help: "The size of each app's private network, as a prefix length from 24 to 29. 28 holds 13 containers; an app with more workloads than fit gets a larger network.", Default: "28", Advanced: true},
			{Key: "egress_gateway_image", Label: "Egress gateway image", Type: "string", Help: "The image an app's egress gateway runs from when its egress rules restrict anything: any image with Pando's binary at /usr/local/bin/pando. Without one, and with Pando not running in a container, apps whose egress is restricted cannot be deployed.", Default: "The image Pando's own container runs", Advanced: true},
			{Key: "oci_runtime", Label: "Container runtime", Type: "string", Help: "The runtime Docker starts apps with, by the name it is registered under in daemon.json. \"runsc\" (gVisor) or a Kata runtime makes this a sandboxed runtime, which host policy can require; the port and file-write checks when an app is added then cannot see inside the sandbox, so Pando asks for the port instead.", Default: "Docker's default, runc", Advanced: true},
		},
	}
}
