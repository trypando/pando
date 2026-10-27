package multidocker

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

// Each host's forwarding agent (O-45, design 06 §4): one Pando-owned
// container, running Pando's image as `pando host-agent serve`, on a network
// of its own outside the app range with one published port, and joined by the
// host's Docker adapter to every app network on that host — in the place
// Pando's container takes on one host (ProxyContainer).

const (
	// agentNetwork is the network the agent's port is published from. Made
	// with Docker's own addresses, never the app range: the agent refuses to
	// forward into anything outside the app range, so this network is one it
	// cannot forward into.
	agentNetwork = "pando-agent"

	labelManaged     = "io.pando.managed"
	labelRole        = "io.pando.role"
	roleAgent        = "host-agent"
	labelAgentDigest = "io.pando.agent.digest"
	labelAgentIssued = "io.pando.agent.issued"

	// agentBinary is where Pando's image has its binary.
	agentBinary = "/usr/local/bin/pando"
	agentUser   = "65532:65532"

	// The agent's certificate is issued for 90 days and the agent re-created
	// with a new one after 30.
	agentReissueAfter = 30 * 24 * time.Hour

	// agentCheckEvery bounds how often the agent's container is inspected.
	agentCheckEvery = 30 * time.Second
)

// Environment the agent reads its certificate, key and the authorities from.
// Set on its container by this adapter; the agent's key is on its own host
// only, and it is a server key: it cannot open a connection through any agent.
const (
	EnvAgentCert        = "PANDO_AGENT_CERT"
	EnvAgentKey         = "PANDO_AGENT_KEY"
	EnvAgentAuthorities = "PANDO_AGENT_AUTHORITIES"
	EnvAgentPool        = "PANDO_AGENT_NETWORK_POOL"
	EnvAgentListen      = "PANDO_AGENT_LISTEN"

	// EnvAgentRuntimeSocket is the host's own container runtime socket, on a
	// host that joined a cluster: the agent then carries a control host's
	// runtime calls to it (R-416). This adapter sets it only on the agent it
	// keeps on the install's own machine (carryRuntime).
	EnvAgentRuntimeSocket = "PANDO_AGENT_RUNTIME_SOCKET"
)

// clientCert is Pando's certificate for the agents, issued at Configure from
// the authority and held in memory only.
type clientCert struct{ cert tls.Certificate }

// agentKeeper keeps one host's agent running at the right image and
// configuration.
type agentKeeper struct {
	cli         *client.Client
	host        string
	port        int
	pool        netip.Prefix
	authorities hostagent.Authorities
	image       func(context.Context) string

	// settings names where agent_image and agent_port are set, for remedies.
	// Empty is this adapter's.
	settings string

	// socket is the runtime socket the agent carries runtime calls to: set
	// only for the agent on the install's own machine, from Pando's own
	// container there (R-416). Nil, or false, carries none.
	socket func(context.Context) (docker.OwnSocket, bool)

	mu      sync.Mutex
	checked time.Time
}

// AgentContainer is the agent's container name, and what a Docker adapter
// serving it is given as ProxyContainer.
const AgentContainer = agentContainer

// AgentKeeper keeps a forwarding agent running on one Docker API. The Incus
// runtime runs one in each app's VM (notes-incus-runtime.md), with this
// adapter's code, so the two keep one agent and one protocol.
type AgentKeeper struct{ k *agentKeeper }

// NewAgentKeeper keeps an agent on cli whose certificate names host, listening
// on port and forwarding only into pool. settings is where the operator sets
// agent_image and agent_port, as a remedy names it.
func NewAgentKeeper(cli *client.Client, host string, port int, pool netip.Prefix, authorities hostagent.Authorities, image func(context.Context) string, settings string) *AgentKeeper {
	return &AgentKeeper{k: &agentKeeper{
		cli: cli, host: host, port: port, pool: pool, authorities: authorities, image: image, settings: settings,
	}}
}

// Ensure makes sure the agent runs, and reports whether it had to create it.
func (a *AgentKeeper) Ensure(ctx context.Context) (bool, error) { return a.k.ensure(ctx) }

func (k *agentKeeper) settingsName() string {
	if k.settings == "" {
		return "the Docker hosts runtime's settings"
	}
	return k.settings
}

// ensureAgent makes sure the host's agent is running and, when it had to be
// created again, joins it to the host's app networks at once rather than at
// the next rejoin.
func (a *Adapter) ensureAgent(ctx context.Context, h *host) error {
	if h.agent == nil {
		return nil
	}
	created, err := h.agent.ensure(ctx)
	if err != nil {
		return err
	}
	if created {
		owns := a.ownsFunc()
		if owns == nil {
			// Not yet told what this install owns; the app being applied
			// is joined by its own Apply, and the next rejoin does the rest.
			return nil
		}
		if _, err := h.rt.RejoinNetworks(ctx, owns); err != nil {
			return err
		}
	}
	return nil
}

// digest names what the agent is run with, apart from its certificate.
func (k *agentKeeper) digest(image string) string { return k.digestWith(image, docker.OwnSocket{}) }

// digestWith is digest for an agent given sock, the runtime socket it
// carries calls to, if any: one that gains or loses it is made again.
func (k *agentKeeper) digestWith(image string, sock docker.OwnSocket) string {
	parts := []string{image, strconv.Itoa(k.port), k.pool.String(), k.authorities.Fingerprint(), k.host}
	if sock.Source != "" {
		parts = append(parts, "runtime", sock.Source, sock.Group)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:8])
}

// ensure reports whether it created the agent.
func (k *agentKeeper) ensure(ctx context.Context) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.checked.IsZero() && time.Since(k.checked) < agentCheckEvery {
		return false, nil
	}

	image := k.image(ctx)
	if image == "" {
		return false, errs.Newf(errs.AdapterFailed,
			"Pando could not tell which image to run the host agent on %s from.", k.host).
			WithRemedy("Set agent_image in " + k.settingsName() + " to the image of this version of Pando, such as one in the install's registry.")
	}
	var sock docker.OwnSocket
	if k.socket != nil {
		sock, _ = k.socket(ctx)
	}
	digest := k.digestWith(image, sock)

	kept, err := k.keepCurrent(ctx, digest)
	if err != nil || kept {
		return false, err
	}
	if err := k.create(ctx, image, digest, sock); err != nil {
		return false, err
	}
	k.checked = time.Now()
	return true, nil
}

// keepCurrent looks at the container by the agent's name: one made with
// digest is started if stopped and kept; one of another version,
// configuration or a certificate due for renewal is removed. It removes or
// replaces only a container this keeper made, by its labels: on a host that
// joined the cluster the container by that name is the host's own Pando,
// which runs the host's agent itself (R-416), and Pando never removes itself.
func (k *agentKeeper) keepCurrent(ctx context.Context, digest string) (bool, error) {
	existing, err := k.cli.ContainerInspect(ctx, agentContainer, client.ContainerInspectOptions{})
	switch {
	case cerrdefs.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("Docker on %s is not responding.", k.host), err)
	}
	c := existing.Container
	if !keptAgent(c.Config) {
		return false, errs.Newf(errs.AdapterFailed,
			"The container named %s on %s was not made by Pando's Docker hosts runtime as that host's agent, so Pando does not replace or remove it. "+
				"On a host that joined the cluster, that container is the host's own Pando, which runs the host's agent itself.",
			agentContainer, k.host).
			WithRemedy("If " + k.host + " joined the cluster, reach it in the runtime's host list as agent://<address>:<port>, as Pando writes it. Otherwise rename that container.")
	}
	if c.Config.Labels[labelAgentDigest] == digest && !stale(c.Config.Labels[labelAgentIssued]) {
		if c.State == nil || !c.State.Running {
			if _, err := k.cli.ContainerStart(ctx, c.ID, client.ContainerStartOptions{}); err != nil {
				return false, errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not start the host agent on %s.", k.host), err)
			}
		}
		k.checked = time.Now()
		return true, nil
	}
	// An older version, another configuration, or a certificate due for
	// renewal: replaced. Its connections drop, and clients reconnect.
	if _, err := k.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return false, errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not replace the host agent on %s.", k.host), err)
	}
	return false, nil
}

// keptAgent reports whether a container's labels say an agent keeper made
// it.
func keptAgent(cfg *container.Config) bool {
	return cfg != nil && cfg.Labels[labelManaged] == "true" && cfg.Labels[labelRole] == roleAgent
}

// create makes the agent's container and starts it.
func (k *agentKeeper) create(ctx context.Context, image, digest string, sock docker.OwnSocket) error {
	networkID, err := k.ensureNetwork(ctx)
	if err != nil {
		return err
	}
	if err := k.pull(ctx, image); err != nil {
		return err
	}

	certPEM, keyPEM, err := k.authorities.IssueServer(k.host)
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not issue the host agent's certificate.", err)
	}
	port, err := network.ParsePort(strconv.Itoa(k.port) + "/tcp")
	if err != nil {
		return errs.Wrap(errs.ValidInvalid, "The agent port is not usable.", err)
	}
	cfg := &container.Config{
		Image:      image,
		Entrypoint: []string{agentBinary},
		Cmd:        []string{"host-agent", "serve"},
		User:       agentUser,
		Env: []string{
			EnvAgentCert + "=" + string(certPEM),
			EnvAgentKey + "=" + string(keyPEM),
			EnvAgentAuthorities + "=" + string(k.authorities.PoolPEM()),
			EnvAgentPool + "=" + k.pool.String(),
			EnvAgentListen + "=:" + strconv.Itoa(k.port),
		},
		ExposedPorts: network.PortSet{port: struct{}{}},
		Labels: map[string]string{
			labelManaged:     "true",
			labelRole:        roleAgent,
			labelAgentDigest: digest,
			labelAgentIssued: time.Now().UTC().Format(time.RFC3339),
		},
	}
	pids := int64(256)
	hostCfg := &container.HostConfig{
		// The one port an app host publishes (R-026 concerns app workloads).
		// Operators restrict it to the control host's address with the host
		// firewall (operator guide).
		PortBindings:   network.PortMap{port: []network.PortBinding{{HostPort: strconv.Itoa(k.port)}}},
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Resources: container.Resources{
			NanoCPUs:  1_000_000_000,
			Memory:    256 << 20,
			PidsLimit: &pids,
		},
		LogConfig: container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "5m", "max-file": "2"}},
	}
	carryRuntime(cfg, hostCfg, sock)
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{agentNetwork: {NetworkID: networkID}},
	}
	created, err := k.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: cfg, HostConfig: hostCfg, NetworkingConfig: netCfg, Name: agentContainer,
	})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not create the host agent on %s.", k.host), err)
	}
	if _, err := k.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return errs.Wrap(errs.AdapterFailed,
			fmt.Sprintf("Could not start the host agent on %s. The likeliest cause is something else on the host using port %d.", k.host, k.port), err).
			WithRemedy("Free that port on the host, or set agent_port in " + k.settingsName() + " to one that is free.")
	}
	return nil
}

// agentRuntimeSocket is where the agent on the install's own machine finds
// the machine's runtime socket.
const agentRuntimeSocket = "/var/run/docker.sock"

// carryRuntime gives the agent the machine's runtime socket, when it has one
// to give: the agent on the install's own machine then carries the runtime
// calls of the cluster's other control hosts, as a joined host's agent does
// (R-416). The socket is mounted into the agent alone and never published:
// the agent publishes only its mutual-TLS port, and carries runtime calls
// only for a control host's certificate (hostagent.IsControl).
func carryRuntime(cfg *container.Config, hostCfg *container.HostConfig, sock docker.OwnSocket) {
	if sock.Source == "" {
		return
	}
	cfg.Env = append(cfg.Env, EnvAgentRuntimeSocket+"="+agentRuntimeSocket)
	hostCfg.Mounts = append(hostCfg.Mounts, mount.Mount{Type: mount.TypeBind, Source: sock.Source, Target: agentRuntimeSocket})
	if sock.Group != "" {
		hostCfg.GroupAdd = append(hostCfg.GroupAdd, sock.Group)
	}
}

func stale(issued string) bool {
	at, err := time.Parse(time.RFC3339, issued)
	return err != nil || time.Since(at) > agentReissueAfter
}

// ensureNetwork makes the agent's own network and refuses one inside the
// app range, which would put the published port's network among those the
// agent forwards into.
func (k *agentKeeper) ensureNetwork(ctx context.Context) (string, error) {
	list, err := k.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("name", agentNetwork),
	})
	if err != nil {
		return "", errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("Docker on %s is not responding.", k.host), err)
	}
	for _, n := range list.Items {
		if n.Name != agentNetwork {
			continue
		}
		for _, c := range n.IPAM.Config {
			if c.Subnet.IsValid() && c.Subnet.Overlaps(k.pool) {
				return "", errs.Newf(errs.AdapterFailed,
					"The Docker network %s on %s uses addresses in the app network range %s, so the host agent could forward into it.",
					agentNetwork, k.host, k.pool).
					WithRemedy("Remove it with: docker network rm " + agentNetwork + " — Pando makes it again outside the range.")
			}
		}
		return n.ID, nil
	}
	created, err := k.cli.NetworkCreate(ctx, agentNetwork, client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: map[string]string{labelManaged: "true", labelRole: roleAgent},
	})
	if err != nil {
		return "", errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not create the host agent's network on %s.", k.host), err)
	}
	return created.ID, nil
}

// pull fetches the agent's image unless the host has it.
func (k *agentKeeper) pull(ctx context.Context, image string) error {
	if _, err := k.cli.ImageInspect(ctx, image); err == nil {
		return nil
	}
	rc, err := k.cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not pull %s on %s for the host agent.", image, k.host), err)
	}
	defer func() { _ = rc.Close() }()
	if err := rc.Wait(ctx); err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not pull %s on %s for the host agent.", image, k.host), err)
	}
	return nil
}
