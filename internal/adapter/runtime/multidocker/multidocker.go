// Package multidocker runs apps on several Docker hosts
// (notes-multi-host-docker-issue-72.md, issue #72 PR 7).
//
// It is a router in front of one single-host Docker adapter per host: every
// operation on an app runs the existing Docker adapter's code against the
// app's host. What it adds is placement (R-256), the forwarding agent each
// host runs so Pando's proxy can reach an app on another host without routing
// around itself (O-45, design 06 §4), and capacity summed over hosts.
//
// Like every adapter it does not authorize, audit or touch Pando's state
// (R-027). Where an app runs is recorded on the hosts themselves: the app's
// network and volumes carry its bundle label, and the adapter reads them back.
package multidocker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

// Kind is the adapter's kind string.
const Kind = "docker-hosts"

// agentContainer is the name of each host's forwarding agent, and what each
// host's Docker adapter joins to every app network on that host in place of
// Pando's own container (attachProxy, ProxyContainer).
const agentContainer = "pando-agent"

// hostRuntime is what the adapter needs of one host's Docker adapter. The
// real one is *docker.Adapter; tests use a fake.
type hostRuntime interface {
	api.RuntimeAdapter
	Committed(ctx context.Context) (api.Fit, error)
	Bundles(ctx context.Context) (map[string]bool, error)
	RejoinNetworks(ctx context.Context, owns func(string) bool) (int, error)
	RoomFor(ctx context.Context, bundleID string) (*api.Fit, error)
	DetachProxy(ctx context.Context, bundleID string) error
	ReclaimNetworks(ctx context.Context, owns func(string) bool) (int, error)
}

// localRuntime is the control host's Docker adapter joined as Pando's own
// container.
type localRuntime interface {
	api.RuntimeAdapter
	api.SelfUpgrader
}

// host is one configured Docker host.
type host struct {
	cfg HostConfig
	rt  hostRuntime

	// agentAddr is where the proxy dials this host's agent.
	agentAddr string

	// agent keeps this host's forwarding agent running (agent.go). Nil in
	// tests that do not exercise it.
	agent *agentKeeper
}

// Adapter runs workloads on several Docker hosts.
type Adapter struct {
	config Config
	hosts  []*host

	// control is the control host, and local the Docker adapter on it that
	// is joined as Pando's own container: for the edge, trial runs and
	// self-upgrade, which run where Pando runs (O-47).
	control *host
	local   localRuntime

	authorities hostagent.Authorities
	clientCert  clientCert

	placement placements

	// owns is what core last said belongs to this install, kept from
	// RejoinNetworks for the joins an agent's re-creation needs.
	ownsMu sync.Mutex
	owns   func(string) bool

	// watchRetry overrides defaultWatchRetry (events.go). Tests shorten it.
	watchRetry []time.Duration
}

// New builds an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryRuntime }

// Configure reads the host list and the credentials, makes one Docker client
// per host and issues Pando's client certificate for the agents, in memory.
// Nothing is contacted: a host that is down at start is reported by
// HealthCheck and Observe, not by refusing to configure.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The Docker hosts runtime's configuration could not be read.", err)
		}
	}
	hosts, err := parseHosts(cfg.Hosts)
	if err != nil {
		return errs.Wrap(errs.ValidInvalid, "The Docker hosts runtime's host list could not be read.", err).
			WithRemedy(`List the hosts as JSON, such as [{"name":"control","control":true,"agent_address":"10.0.0.5:7443"},{"name":"app-1","endpoint":"tcp://10.0.0.6:2376"}].`)
	}
	if err := validate(cfg, hosts); err != nil {
		return errs.Newf(errs.ValidInvalid, "The Docker hosts runtime cannot be set up: %s.", err)
	}
	authorities, err := hostagent.ParseAuthorities([]byte(cfg.Credentials.AgentAuthority))
	if err != nil {
		return errs.Newf(errs.ValidInvalid, "The Docker hosts runtime cannot be set up: %s.", err).
			WithRemedy("Run `pando host-agent new-authority` and paste its output into the agent_authority credential.")
	}
	client, err := authorities.IssueClient()
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not issue Pando's certificate for the host agents.", err)
	}

	a.config = cfg
	a.authorities = authorities
	a.clientCert = clientCert{cert: client}
	a.hosts = nil
	pool, _ := networkPool(cfg)
	for _, hc := range hosts {
		cli, err := newClient(hc, cfg.Credentials)
		if err != nil {
			return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not set up the connection to Docker on %s.", hc.Name), err)
		}
		addr, _ := agentAddress(hc, agentPort(cfg))
		h := &host{
			cfg:       hc,
			agentAddr: addr,
		}
		dcfg := docker.Config{
			TotalCPUMillis:     hc.TotalCPUMillis,
			TotalMemoryBytes:   hc.TotalMemoryBytes,
			TotalDiskBytes:     hc.TotalDiskBytes,
			ProxyContainer:     agentContainer,
			NetworkPool:        pool.String(),
			NetworkBlockBits:   cfg.NetworkBlockBits,
			OCIRuntime:         cfg.OCIRuntime,
			EgressGatewayImage: cfg.AgentImage,
		}
		h.rt = docker.NewWithClient(cli, dcfg)
		h.agent = &agentKeeper{
			cli: cli, host: hc.Name, port: agentPort(cfg), pool: pool,
			authorities: authorities, image: a.agentImage,
		}
		if hc.Control {
			a.control = h
			local := dcfg
			local.ProxyContainer = "" // Pando's own container, as on one host.
			a.local = docker.NewWithClient(cli, local)
		}
		a.hosts = append(a.hosts, h)
	}
	a.placement = placements{byBundle: map[string]string{}}
	return nil
}

// agentImage is the image agents and egress gateways run: the configured
// one, or the image Pando's own container on the control host was started
// from, by the reference it was started with so other hosts can pull it.
func (a *Adapter) agentImage(ctx context.Context) string {
	if a.config.AgentImage != "" {
		return a.config.AgentImage
	}
	if a.local == nil {
		return ""
	}
	self, err := a.local.Self(ctx)
	if err != nil || strings.HasPrefix(self.Image, "sha256:") {
		return ""
	}
	return self.Image
}

func (a *Adapter) byName(name string) *host {
	for _, h := range a.hosts {
		if h.cfg.Name == name {
			return h
		}
	}
	return nil
}

// eachHost runs fn on every host at once and returns each host's error.
func (a *Adapter) eachHost(ctx context.Context, fn func(context.Context, *host) error) map[string]error {
	var mu sync.Mutex
	out := map[string]error{}
	var wg sync.WaitGroup
	for _, h := range a.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := fn(ctx, h)
			mu.Lock()
			out[h.cfg.Name] = err
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// HealthCheck asks every host. The adapter is healthy while the control host
// and at least one host open to new apps answer, and every answering host
// runs the same platform; a host that does not answer makes its own apps
// unobservable (Observe), not every app undeployable.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.control == nil {
		return errs.New(errs.AdapterUnavailable, "The Docker hosts runtime has not been set up.")
	}
	results := a.eachHost(ctx, func(ctx context.Context, h *host) error { return h.rt.HealthCheck(ctx) })
	if err := results[a.control.cfg.Name]; err != nil {
		return errs.Wrap(errs.AdapterUnavailable,
			fmt.Sprintf("Docker on the control host, %s, is not responding.", a.control.cfg.Name), err)
	}
	open := 0
	var down []string
	for _, h := range a.hosts {
		if results[h.cfg.Name] != nil {
			down = append(down, h.cfg.Name)
			continue
		}
		if !h.cfg.NoPlacement {
			open++
		}
	}
	if open == 0 {
		return errs.Newf(errs.AdapterUnavailable,
			"No Docker host open to new apps is responding. Not responding: %s.", strings.Join(down, ", "))
	}
	if platforms := a.platforms(ctx); len(platforms) > 1 {
		return errs.Newf(errs.AdapterUnavailable,
			"The Docker hosts run images for different platforms (%s), and an app built for one would not start on another.",
			strings.Join(platforms, ", ")).
			WithRemedy("Use hosts of one CPU architecture, or remove the others from the host list.")
	}
	return nil
}

// platforms is each distinct platform the answering hosts report.
func (a *Adapter) platforms(ctx context.Context) []string {
	var mu sync.Mutex
	seen := map[string]bool{}
	a.eachHost(ctx, func(ctx context.Context, h *host) error {
		caps, err := h.rt.Capabilities(ctx)
		if err == nil && caps.Platform != "" {
			mu.Lock()
			seen[caps.Platform] = true
			mu.Unlock()
		}
		return nil
	})
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Capabilities are the control host's, with what several hosts change
// (R-254).
func (a *Adapter) Capabilities(ctx context.Context) (api.RuntimeCapabilities, error) {
	caps, err := a.local.Capabilities(ctx)
	if err != nil {
		return caps, err
	}
	// A built image reaches whichever host placement chooses, after the
	// build, by being pulled from the registry the build pushed it to (PR 5,
	// notes-image-registry-issue-72.md). A tarball streamed to the control
	// host would not be on the app's host when the reconciler recreates it.
	caps.ImageDelivery = []api.ImageDelivery{api.ImageDeliveryRegistry}

	// The egress gateway runs on the app's host, from an image every host
	// can pull.
	caps.SupportsEgressRestriction = a.agentImage(ctx) != ""

	// One platform for every host, or none stated (HealthCheck refuses the
	// mix).
	if platforms := a.platforms(ctx); len(platforms) == 1 {
		caps.Platform = platforms[0]
	} else {
		caps.Platform = ""
	}
	return caps, nil
}

// Capacity sums the hosts open to new apps, with each host's figures in
// Details and the roomiest one's free space as LargestFit (R-243).
func (a *Adapter) Capacity(ctx context.Context) (api.Capacity, error) {
	type reading struct {
		capacity api.Capacity
		err      error
	}
	var mu sync.Mutex
	readings := map[string]reading{}
	a.eachHost(ctx, func(ctx context.Context, h *host) error {
		c, err := h.rt.Capacity(ctx)
		mu.Lock()
		readings[h.cfg.Name] = reading{c, err}
		mu.Unlock()
		return nil
	})

	out := api.Capacity{Reported: time.Now().UTC(), Details: map[string]any{}}
	var perHost []map[string]any
	answered := 0
	for _, h := range a.hosts {
		r := readings[h.cfg.Name]
		row := map[string]any{"name": h.cfg.Name, "control": h.cfg.Control, "open_to_new_apps": !h.cfg.NoPlacement}
		if r.err != nil {
			row["reachable"] = false
			perHost = append(perHost, row)
			continue
		}
		answered++
		row["reachable"] = true
		row["total_cpu_millis"] = r.capacity.TotalCPUMillis
		row["total_memory_bytes"] = r.capacity.TotalMemoryBytes
		row["running_workloads"] = r.capacity.RunningWorkloads
		if r.capacity.LargestFit != nil {
			row["free_cpu_millis"] = r.capacity.LargestFit.CPUMillis
			row["free_memory_bytes"] = r.capacity.LargestFit.MemoryBytes
		}
		perHost = append(perHost, row)
		if r.capacity.RunningWorkloads > 0 {
			out.RunningWorkloads += r.capacity.RunningWorkloads
		}
		if h.cfg.NoPlacement {
			continue
		}
		out.TotalCPUMillis += r.capacity.TotalCPUMillis
		out.TotalMemoryBytes += r.capacity.TotalMemoryBytes
		out.TotalDiskBytes += r.capacity.TotalDiskBytes
		if f := r.capacity.LargestFit; f != nil {
			if out.LargestFit == nil || f.MemoryBytes > out.LargestFit.MemoryBytes ||
				(f.MemoryBytes == out.LargestFit.MemoryBytes && f.CPUMillis > out.LargestFit.CPUMillis) {
				fit := *f
				out.LargestFit = &fit
			}
		}
	}
	if answered == 0 {
		return api.Capacity{}, errs.New(errs.AdapterUnavailable, "None of the Docker hosts is responding.")
	}
	out.Details["hosts"] = perHost
	return out, nil
}

// InUse sums every host, at once: each takes about a second.
func (a *Adapter) InUse(ctx context.Context) (api.InUse, error) {
	var mu sync.Mutex
	out := api.InUse{Reported: time.Now().UTC()}
	answered := 0
	a.eachHost(ctx, func(ctx context.Context, h *host) error {
		u, err := h.rt.InUse(ctx)
		if err != nil {
			return err
		}
		mu.Lock()
		out.CPUMillis += u.CPUMillis
		out.MemoryBytes += u.MemoryBytes
		answered++
		mu.Unlock()
		return nil
	})
	if answered == 0 {
		return api.InUse{}, errs.New(errs.AdapterUnavailable, "None of the Docker hosts is responding.")
	}
	return out, nil
}

// Apply places the bundle if it has no host (placement.go), makes sure the
// host's agent is running, and applies the plan there with the single-host
// adapter's code. The handle names the host.
func (a *Adapter) Apply(ctx context.Context, p api.BundlePlan) (api.BundleHandle, error) {
	h, err := a.locate(ctx, p.BundleID)
	if err != nil && !p.FirstDeploy {
		// A host is silent and the app may be on it. Never placed elsewhere:
		// that would be a second copy, or a move (O-46).
		return api.BundleHandle{}, err
	}
	if h == nil {
		// A first deploy has nothing on any host, the silent ones included,
		// so it is placed among the hosts that answer.
		if h, err = a.place(ctx, p); err != nil {
			return api.BundleHandle{}, err
		}
	}
	if err := a.ensureAgent(ctx, h); err != nil {
		return api.BundleHandle{}, err
	}
	handle, err := h.rt.Apply(ctx, p)
	if err != nil {
		return handle, err
	}
	handle.Handle = h.cfg.Name
	return handle, nil
}

// Observe reports the bundle on its host. An app whose host does not answer
// is an error, which the reconciler records as unobservable — never as
// failed (R-151, design 05 §2) — and never as absent, which would have the
// reconciler create it again on another host (O-46).
func (a *Adapter) Observe(ctx context.Context, ref api.BundleRef) (api.ObservedBundle, error) {
	h, err := a.locate(ctx, ref.BundleID)
	if err != nil {
		return api.ObservedBundle{}, err
	}
	if h == nil {
		return api.ObservedBundle{}, nil
	}
	observed, err := h.rt.Observe(ctx, ref)
	if err != nil {
		return observed, errs.Wrap(errs.AdapterUnavailable,
			fmt.Sprintf("Docker on %s, the host this app runs on, is not responding.", h.cfg.Name), err)
	}
	for i := range observed.Volumes {
		observed.Volumes[i].Handle = volumeHandle(h, observed.Volumes[i].Handle)
	}
	return observed, nil
}

func (a *Adapter) Usage(ctx context.Context, ref api.BundleRef) (api.BundleUsage, error) {
	h, err := a.mustLocate(ctx, ref.BundleID)
	if err != nil {
		return api.BundleUsage{}, err
	}
	return h.rt.Usage(ctx, ref)
}

func (a *Adapter) Stop(ctx context.Context, ref api.BundleRef) error {
	h, err := a.locate(ctx, ref.BundleID)
	if err != nil || h == nil {
		return err
	}
	return h.rt.Stop(ctx, ref)
}

// Destroy tears the bundle down on its host. With its volumes kept, the host
// stays the app's home (R-204): a re-created app returns to its data.
func (a *Adapter) Destroy(ctx context.Context, ref api.BundleRef, opts api.DestroyOptions) error {
	h, err := a.locate(ctx, ref.BundleID)
	if err != nil || h == nil {
		return err
	}
	if err := h.rt.Destroy(ctx, ref, opts); err != nil {
		return err
	}
	if !opts.KeepVolumes {
		a.forget(ref.BundleID)
	}
	// On an app host the agent leaves the app's networks, so they are removed
	// now rather than kept by the agent until it is replaced. Not on the
	// control host, which may be Docker Desktop, where disconnecting a
	// running container drops its published ports. DetachProxy touches only
	// networks labeled as this bundle's, never the agent's own.
	if !h.cfg.Control {
		return h.rt.DetachProxy(ctx, ref.BundleID)
	}
	return nil
}

// LargestFitFor is the roomiest place this bundle may go, for the planner
// (R-242 per host). A bundle already on a host may go only there, and what it
// holds there counts as free. A new one may go to any host open to new apps
// that answers. Nil when its host does not answer: Observe reports that, and
// the planner checks the totals alone.
func (a *Adapter) LargestFitFor(ctx context.Context, bundleID string) (*api.Fit, error) {
	h, err := a.locate(ctx, bundleID)
	if err == nil && h != nil {
		room, err := h.rt.RoomFor(ctx, bundleID)
		if err != nil {
			return nil, nil //nolint:nilerr // A silent host is Observe's to report, not a plan refusal.
		}
		return room, nil
	}
	c, err := a.Capacity(ctx)
	if err != nil {
		return nil, nil //nolint:nilerr // As above.
	}
	return c.LargestFit, nil
}

// Volume handles carry their host: "<host>/<docker volume name>". A Docker
// volume name has no slash, so the split is unambiguous.
func volumeHandle(h *host, name string) string { return h.cfg.Name + "/" + name }

func (a *Adapter) volumeHost(handle string) (*host, api.VolumeHandle, error) {
	name, volume, ok := strings.Cut(handle, "/")
	if !ok {
		return nil, api.VolumeHandle{}, errs.Newf(errs.AdapterFailed,
			"The storage %q does not say which Docker host it is on.", handle)
	}
	h := a.byName(name)
	if h == nil {
		return nil, api.VolumeHandle{}, errs.Newf(errs.AdapterFailed,
			"The storage %q is on %s, which is no longer in the Docker hosts runtime's host list.", volume, name).
			WithRemedy("Add the host back to restore from it, or restore the app from a backup instead.")
	}
	return h, api.VolumeHandle{Handle: volume}, nil
}

// CreateVolume makes storage on the bundle's host, placing the bundle first
// if it has none: the volume is then what keeps it there.
func (a *Adapter) CreateVolume(ctx context.Context, req api.VolumeRequest) (api.VolumeHandle, error) {
	h, err := a.locate(ctx, req.BundleID)
	if err != nil {
		return api.VolumeHandle{}, err
	}
	if h == nil {
		if h, err = a.place(ctx, api.BundlePlan{BundleID: req.BundleID}); err != nil {
			return api.VolumeHandle{}, err
		}
	}
	got, err := h.rt.CreateVolume(ctx, req)
	if err != nil {
		return got, err
	}
	got.Handle = volumeHandle(h, got.Handle)
	return got, nil
}

func (a *Adapter) DestroyVolume(ctx context.Context, vh api.VolumeHandle) error {
	h, local, err := a.volumeHost(vh.Handle)
	if err != nil {
		return err
	}
	local.VolumeID = vh.VolumeID
	return h.rt.DestroyVolume(ctx, local)
}

func (a *Adapter) SnapshotVolume(ctx context.Context, vh api.VolumeHandle, dst io.Writer) error {
	h, local, err := a.volumeHost(vh.Handle)
	if err != nil {
		return err
	}
	local.VolumeID = vh.VolumeID
	return h.rt.SnapshotVolume(ctx, local, dst)
}

func (a *Adapter) RestoreVolume(ctx context.Context, vh api.VolumeHandle, src io.Reader) error {
	h, local, err := a.volumeHost(vh.Handle)
	if err != nil {
		return err
	}
	local.VolumeID = vh.VolumeID
	return h.rt.RestoreVolume(ctx, local, src)
}

// ImportImage is not offered (ImageDelivery is registry only).
func (a *Adapter) ImportImage(context.Context, io.Reader) (string, error) {
	return "", errs.New(errs.PlanCapabilityUnsupported,
		"The Docker hosts runtime takes images from a registry, not from Pando's host.").
		WithRemedy("Add an image registry adapter under System → Adapters, or with pando adapter add image_registry/oci, so builds are pushed there and each host pulls what it runs.")
}

func (a *Adapter) Logs(ctx context.Context, ref api.WorkloadRef, opts api.LogOptions) (io.ReadCloser, error) {
	h, err := a.mustLocate(ctx, ref.BundleID)
	if err != nil {
		return nil, err
	}
	return h.rt.Logs(ctx, ref, opts)
}

func (a *Adapter) Exec(ctx context.Context, ref api.WorkloadRef, req api.ExecRequest) (api.ExecSession, error) {
	h, err := a.mustLocate(ctx, ref.BundleID)
	if err != nil {
		return nil, err
	}
	return h.rt.Exec(ctx, ref, req)
}

// Upstream is the workload's container name on its host, as one host's
// adapter answers, and a Dial through that host's agent (O-45). The proxy
// decides the request before it calls Dial; the agent only carries it.
//
// Answered from the placement map, which is in memory: this runs on every
// proxied request.
func (a *Adapter) Upstream(ctx context.Context, ref api.WorkloadRef, port int) (api.Upstream, error) {
	h, err := a.mustLocate(ctx, ref.BundleID)
	if err != nil {
		return api.Upstream{}, err
	}
	direct, err := h.rt.Upstream(ctx, ref, port)
	if err != nil {
		return api.Upstream{}, err
	}
	u, err := url.Parse(direct.URL)
	if err != nil {
		return api.Upstream{}, errs.Wrap(errs.AdapterFailed, "The app's address could not be read.", err)
	}
	name := u.Hostname()
	targetPort, _ := strconv.Atoi(u.Port())
	addr := h.agentAddr
	tlsConfig := hostagent.ClientTLS(a.authorities, a.clientCert.cert, h.cfg.Name)
	return api.Upstream{
		URL: direct.URL,
		Dial: func(ctx context.Context) (net.Conn, error) {
			return hostagent.Dial(ctx, addr, tlsConfig, name, targetPort)
		},
		PoolKey: h.cfg.Name + "/" + name + ":" + strconv.Itoa(targetPort),
	}, nil
}

// Trial runs on the control host [P]: it is thrown away, so it needs no
// placement.
func (a *Adapter) Trial(ctx context.Context, req api.TrialRequest) (api.TrialResult, error) {
	return a.local.Trial(ctx, req)
}

// The edge runs on the control host beside Pando's replicas, as on one host.
func (a *Adapter) ApplyEdge(ctx context.Context, p api.EdgePlan) error {
	return a.local.ApplyEdge(ctx, p)
}
func (a *Adapter) ObserveEdge(ctx context.Context, name string) (api.EdgeState, error) {
	return a.local.ObserveEdge(ctx, name)
}
func (a *Adapter) RemoveEdge(ctx context.Context, name string) error {
	return a.local.RemoveEdge(ctx, name)
}
func (a *Adapter) Edges(ctx context.Context) ([]string, error) { return a.local.Edges(ctx) }
func (a *Adapter) EdgeVolumes(ctx context.Context) ([]api.VolumeHandle, error) {
	vols, err := a.local.EdgeVolumes(ctx)
	for i := range vols {
		vols[i].Handle = volumeHandle(a.control, vols[i].Handle)
	}
	return vols, err
}

// Self-upgrade replaces Pando's container on the control host, as on one
// host (R-359); core allows it only with a single replica.
func (a *Adapter) Self(ctx context.Context) (api.SelfWorkload, error) { return a.local.Self(ctx) }
func (a *Adapter) PullImage(ctx context.Context, ref string) error {
	return a.local.PullImage(ctx, ref)
}
func (a *Adapter) StartHelper(ctx context.Context, spec api.HelperSpec) (string, error) {
	return a.local.StartHelper(ctx, spec)
}
func (a *Adapter) RemoveHelpers(ctx context.Context) error { return a.local.RemoveHelpers(ctx) }

// RejoinNetworks keeps each host's agent running and joined to that host's
// app networks, and refreshes the placement map. Called at start and every
// fifteen seconds on every replica (cmd/pando); Pando's own replicas join no
// app network on this runtime.
func (a *Adapter) RejoinNetworks(ctx context.Context, owns func(string) bool) (int, error) {
	a.setOwns(owns)
	var mu sync.Mutex
	joined := 0
	results := a.eachHost(ctx, func(ctx context.Context, h *host) error {
		if err := a.ensureAgent(ctx, h); err != nil {
			return err
		}
		n, err := h.rt.RejoinNetworks(ctx, owns)
		mu.Lock()
		joined += n
		mu.Unlock()
		return err
	})
	a.refresh(ctx)
	return joined, firstError(a.hosts, results)
}

// ReclaimNetworks removes each host's app networks nothing is attached to.
func (a *Adapter) ReclaimNetworks(ctx context.Context, owns func(string) bool) (int, error) {
	a.setOwns(owns)
	var mu sync.Mutex
	reclaimed := 0
	results := a.eachHost(ctx, func(ctx context.Context, h *host) error {
		n, err := h.rt.ReclaimNetworks(ctx, owns)
		mu.Lock()
		reclaimed += n
		mu.Unlock()
		return err
	})
	return reclaimed, firstError(a.hosts, results)
}

func (a *Adapter) setOwns(owns func(string) bool) {
	a.ownsMu.Lock()
	a.owns = owns
	a.ownsMu.Unlock()
}

func (a *Adapter) ownsFunc() func(string) bool {
	a.ownsMu.Lock()
	defer a.ownsMu.Unlock()
	return a.owns
}

func firstError(hosts []*host, results map[string]error) error {
	for _, h := range hosts {
		if err := results[h.cfg.Name]; err != nil {
			return fmt.Errorf("%s: %w", h.cfg.Name, err)
		}
	}
	return nil
}

var (
	_ api.RuntimeAdapter = (*Adapter)(nil)
	_ api.SelfUpgrader   = (*Adapter)(nil)
)
