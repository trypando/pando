// Package incus runs each app in its own Incus virtual machine
// (notes-incus-runtime.md).
//
// The adapter owns the VM: its size, its one NIC and the ACL on it, its
// volumes, and where Incus placed it. Inside the VM, Podman runs the app's
// workloads, and the existing Docker adapter drives it through Podman's
// Docker-compatible API, reached through the Incus API rather than the
// network. Pando's proxy reaches a workload through the forwarding agent each
// VM runs (O-45), so nothing about the proxy or routing changes (R-023).
//
// Like every adapter it does not authorize, audit or touch Pando's state
// (R-027). Which VM is which app's is recorded on the VM, in its
// user.pando.bundle key, and read back.
package incus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
	"github.com/trypando/pando/internal/adapter/runtime/incus/incusapi"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

// Kind is the adapter's kind string.
const Kind = "incus"

// Keys on everything the adapter makes in Incus.
const (
	keyBundle    = "user.pando.bundle"
	keyWorkloads = "user.pando.workloads"
	keyTrial     = "user.pando.trial"
	keyVolume    = "user.pando.volume"
	keyManaged   = "user.pando.managed"
)

// volumeRoot is where a VM's volumes are attached, and the Docker adapter's
// VolumeRoot inside it.
const volumeRoot = "/srv/pando/volumes"

// inner is what the adapter needs of the Docker adapter in one VM. The real
// one is *docker.Adapter; tests use a fake.
type inner interface {
	api.RuntimeAdapter
}

// agentEnsurer keeps one VM's forwarding agent running.
type agentEnsurer interface {
	Ensure(ctx context.Context) (bool, error)
}

// vm is one app's VM, as far as this process knows it.
type vm struct {
	name  string
	rt    inner
	agent agentEnsurer
	addr  netip.Addr
}

// Adapter runs each app in an Incus VM.
type Adapter struct {
	config  Config
	api     incusAPI
	sources []netip.Prefix
	pool    netip.Prefix

	authorities hostagent.Authorities
	clientCert  tls.Certificate

	mu  sync.Mutex
	vms map[string]*vm // by bundle ID

	// aclOnNetwork is where the ingress ACL goes (placeACL), read from the
	// network by HealthCheck and Apply.
	aclOnNetwork bool

	// bridge is whether the network is a bridge, whose NICs get port
	// isolation (nic).
	bridge bool

	guestMu    sync.Mutex
	guestAlias string

	// Seams, set by use; tests replace them.
	connect   func(Config) (incusAPI, error)
	newDocker func(api incusAPI, instance string) (*client.Client, error)
	newInner  func(cli *client.Client, cfg docker.Config) inner
	newAgent  func(cli *client.Client, instance string) agentEnsurer
	readyWait time.Duration
	now       func() time.Time
	sleep     func(time.Duration) <-chan time.Time
}

// New builds an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryRuntime }

// Configure reads the settings and credentials and opens the Incus API.
// Over a unix socket nothing is contacted; a server that is down is reported
// by HealthCheck, not by refusing to configure.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The Incus runtime's configuration could not be read.", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	a.use(cfg)
	authorities, err := hostagent.ParseAuthorities([]byte(cfg.Credentials.AgentAuthority))
	if err != nil {
		return errs.Newf(errs.ValidInvalid, "The agents' certificate authority is not usable: %s.", err).
			WithRemedy("Run `pando host-agent new-authority` and paste its output into the agent certificate authority credential.")
	}
	cert, err := authorities.IssueClient()
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not issue Pando's certificate for the agents.", err)
	}
	conn, err := a.connect(cfg)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not set up the connection to Incus at "+cfg.Address+".", err).
			WithRemedy("Check that Incus is running and that the address and certificates in " + settingIn + " are right.")
	}
	a.api = conn
	a.authorities = authorities
	a.clientCert = cert
	return nil
}

// use sets the configuration and the seams a test has not set. Validated
// already.
func (a *Adapter) use(cfg Config) {
	a.config = cfg
	a.sources, _ = cfg.proxySources()
	a.pool, _ = netip.ParsePrefix(cfg.NetworkPool)
	a.pool = a.pool.Masked()
	a.vms = map[string]*vm{}
	if a.connect == nil {
		a.connect = func(c Config) (incusAPI, error) {
			conn, err := connect(c)
			if err != nil {
				return nil, err
			}
			return &server{c: conn}, nil
		}
	}
	if a.newDocker == nil {
		a.newDocker = dockerClient
	}
	if a.newInner == nil {
		a.newInner = func(cli *client.Client, c docker.Config) inner { return docker.NewWithClient(cli, c) }
	}
	if a.newAgent == nil {
		a.newAgent = func(cli *client.Client, instance string) agentEnsurer {
			return multidocker.NewAgentKeeper(cli, instance, a.config.AgentPort, a.pool, a.authorities,
				func(context.Context) string { return a.config.AgentImage }, settingIn)
		}
	}
	if a.readyWait == 0 {
		a.readyWait = 5 * time.Minute
	}
	if a.now == nil {
		a.now = time.Now
	}
}

// required are the API extensions the adapter uses.
var required = []string{
	"virtual-machines",           // the whole point
	"instance_get_full",          // one call for every VM and its state
	"instances_state_total",      // memory totals in instance state
	"network_acl",                // the ACL on each VM's NIC (R-026)
	"network_bridge_acl_devices", // the ACL on a NIC on a bridge network
	"custom_volume_sftp",         // volume backups while the VM is stopped (R-212)
	"clustering_groups",          // target "@group"
}

// HealthCheck asks Incus whether everything the adapter relies on is there.
// It changes nothing: the ACL and the guest image are made by Apply.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.api == nil {
		return errs.New(errs.AdapterUnavailable, "The Incus runtime has not been set up.")
	}
	srv, err := a.api.Server(ctx)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Incus at "+a.config.Address+" is not responding.", err).
			WithRemedy("Check that Incus is running and reachable, and that the address and certificates in " + settingIn + " are right.")
	}
	have := map[string]bool{}
	for _, e := range srv.APIExtensions {
		have[e] = true
	}
	for _, e := range required {
		if !have[e] {
			return errs.Newf(errs.AdapterUnavailable,
				"Incus at %s is version %s, which lacks the %s feature the Incus runtime needs.", a.config.Address, srv.Environment.ServerVersion, e).
				WithRemedy("Upgrade Incus to 6.3 or later.")
		}
	}
	if srv.Environment.Project != "" && srv.Environment.Project != a.config.Project {
		return errs.Newf(errs.AdapterUnavailable,
			"Pando asked Incus for the project %s and was given %s. The client certificate is probably restricted to other projects.",
			a.config.Project, srv.Environment.Project).
			WithRemedy("Allow the certificate the project with `incus config trust edit`, or set project in " + settingIn + ".")
	}
	if err := a.checkPlacement(ctx); err != nil {
		return err
	}
	if _, err := a.platform(ctx); err != nil {
		return err
	}
	return nil
}

// checkPlacement refuses a network or a storage pool that exists on one
// member at a time, on a cluster where VMs could be placed on another
// (notes-incus-runtime.md, Which network).
func (a *Adapter) checkPlacement(ctx context.Context) error {
	n, _, err := a.api.Network(ctx, a.config.Network)
	if errors.Is(err, errNotFound) {
		return errs.Newf(errs.AdapterUnavailable, "Incus has no network named %s.", a.config.Network).
			WithRemedy("Create it, or set network in " + settingIn + " to one that exists. `incus network list` shows them.")
	}
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the network "+a.config.Network+" from Incus.", err)
	}
	if err := a.checkDedicated(n); err != nil {
		return err
	}
	pool, err := a.storagePool(ctx)
	if err != nil {
		return err
	}
	members, err := a.members(ctx)
	if err != nil {
		return err
	}
	if len(members) <= 1 {
		return nil
	}
	pinned := a.config.Target != "" && !strings.HasPrefix(a.config.Target, "@")
	if n.Type == "bridge" && !pinned {
		return errs.Newf(errs.AdapterUnavailable,
			"The network %s is a bridge, which exists separately on each of the cluster's %d members, so Pando could not reach a VM Incus placed on a member other than its own.",
			a.config.Network, len(members)).
			WithRemedy("Use an OVN network, which spans the members, or set target in " + settingIn + " to the one member VMs should run on.")
	}
	if local, ok := a.localPool(ctx, pool); ok && local && !pinned {
		return errs.Newf(errs.AdapterUnavailable,
			"The storage pool %s keeps each volume on one cluster member, and VMs could be placed on any of %d, so a VM could start where its volumes are not.",
			pool, len(members)).
			WithRemedy("Use a pool shared by the members, such as Ceph or clustered LVM, or set target in " + settingIn + " to one member.")
	}
	return nil
}

// Capabilities reports what this adapter can do (R-254).
func (a *Adapter) Capabilities(ctx context.Context) (api.RuntimeCapabilities, error) {
	platform, _ := a.platform(ctx)
	return api.RuntimeCapabilities{
		// A hypervisor between apps (R-115). Two workloads of one app share
		// the VM's kernel, as on every runtime; two apps never do.
		IsolationClass: spec.IsolationVM,

		SupportsPersistentVolumes: true,
		SupportsCarriedFiles:      true,
		SupportsExec:              true,
		SupportsMultipleWorkloads: true,
		SupportsPrivateNetwork:    true,

		// The VM's limits by the hypervisor, the workloads' by Podman.
		SupportsResourceLimits: true,
		ReportsUsage:           true,
		SupportsStartThenSwap:  false,

		// The Docker adapter's, inside the VM.
		LogRetention: api.LogRetentionCapability{
			SupportsSizeCap:          true,
			CanChangeWithoutRecreate: false,
			ReportsUsage:             false,
			MinBytes:                 2 << 20,
		},

		// Registry only: a tarball would need storing where every member can
		// read it, and collecting by an adapter that does not know which
		// deployments still need it.
		ImageDelivery: []api.ImageDelivery{api.ImageDeliveryRegistry},

		// The edge and Pando itself run on the install's Docker host
		// (notes-incus-runtime.md, An install that runs apps on Incus).
		SupportsEdge:        false,
		SupportsSelfUpgrade: false,

		SupportsTrialRun:         true,
		SupportsPortObservation:  true,
		SupportsWriteObservation: true,

		// The Docker adapter's gateway, inside the VM, from agent_image.
		SupportsEgressRestriction: true,

		// Incus's event stream reports the VM, not the workloads in it.
		SupportsBundleEvents: false,

		Platform: platform,
	}, nil
}

// platform is the architecture every VM runs, refused when members differ:
// an image pulled for one would not start on another.
func (a *Adapter) platform(ctx context.Context) (string, error) {
	srv, err := a.api.Server(ctx)
	if err != nil {
		return "", errs.Wrap(errs.AdapterUnavailable, "Incus is not responding.", err)
	}
	arches := map[string]bool{}
	members, err := a.members(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range members {
		arches[m.Architecture] = true
	}
	if len(arches) == 0 && len(srv.Environment.Architectures) > 0 {
		arches[srv.Environment.Architectures[0]] = true
	}
	if len(arches) > 1 {
		return "", errs.New(errs.AdapterUnavailable,
			"The Incus cluster's members have different processor architectures, so an image pulled for one would not run on another.").
			WithRemedy("Set target in " + settingIn + " to a cluster group whose members share an architecture.")
	}
	for arch := range arches {
		return "linux/" + goArch(arch), nil
	}
	return "", nil
}

// goArch names an Incus architecture as a registry does.
func goArch(arch string) string {
	switch arch {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	}
	return arch
}

// members is the cluster's members that VMs may be placed on: all of them,
// the target's group, or the target. Nil on a server that is not clustered.
func (a *Adapter) members(ctx context.Context) ([]incusapi.ClusterMember, error) {
	all, err := a.api.Members(ctx)
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not read the Incus cluster's members.", err)
	}
	if a.config.Target == "" || all == nil {
		return all, nil
	}
	var out []incusapi.ClusterMember
	group, isGroup := strings.CutPrefix(a.config.Target, "@")
	for _, m := range all {
		if (!isGroup && m.ServerName == a.config.Target) || (isGroup && contains(m.Groups, group)) {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, errs.Newf(errs.AdapterUnavailable, "The Incus cluster has no member or group %s.", a.config.Target).
			WithRemedy("Set target in " + settingIn + " to a member or @group that exists. `incus cluster list` shows them.")
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// storagePool is the configured pool, or the project's default profile's.
func (a *Adapter) storagePool(ctx context.Context) (string, error) {
	if a.config.StoragePool != "" {
		return a.config.StoragePool, nil
	}
	p, err := a.api.Profile(ctx, "default")
	if err != nil {
		return "", errs.Wrap(errs.AdapterUnavailable, "Could not read the project's default profile from Incus.", err)
	}
	for _, d := range p.Devices {
		if d["type"] == "disk" && d["path"] == "/" && d["pool"] != "" {
			return d["pool"], nil
		}
	}
	return "", errs.Newf(errs.AdapterUnavailable,
		"The Incus project %s's default profile has no root disk, so Pando cannot tell which storage pool to use.", a.config.Project).
		WithRemedy("Set storage_pool in " + settingIn + ".")
}

// localPool reports whether a pool keeps each volume on one member. ok is
// false when Incus does not say.
func (a *Adapter) localPool(ctx context.Context, pool string) (local, ok bool) {
	driver, err := a.api.PoolDriver(ctx, pool)
	if err != nil {
		return false, false
	}
	switch driver {
	case "ceph", "cephfs", "lvmcluster", "linstor":
		return false, true
	}
	return true, true
}

// The edge runs on the install's Docker host (SupportsEdge is false).

func (a *Adapter) ApplyEdge(context.Context, api.EdgePlan) error { return errNoEdge() }
func (a *Adapter) ObserveEdge(context.Context, string) (api.EdgeState, error) {
	return api.EdgeState{}, errNoEdge()
}
func (a *Adapter) RemoveEdge(context.Context, string) error                { return errNoEdge() }
func (a *Adapter) Edges(context.Context) ([]string, error)                 { return nil, nil }
func (a *Adapter) EdgeVolumes(context.Context) ([]api.VolumeHandle, error) { return nil, nil }

func errNoEdge() error {
	return errs.New(errs.PlanCapabilityUnsupported, "The Incus runtime does not run an edge; the install's Docker runtime does.").
		WithRemedy("Keep the Docker runtime as the install's default runtime, and choose the Incus runtime per app.")
}

// WatchBundles is not offered (SupportsBundleEvents is false).
func (a *Adapter) WatchBundles(context.Context, func(api.BundleEvent)) error {
	return errs.New(errs.PlanCapabilityUnsupported, "The Incus runtime does not report workload events; the reconciler looks at each app on every pass.")
}

var _ api.RuntimeAdapter = (*Adapter)(nil)
