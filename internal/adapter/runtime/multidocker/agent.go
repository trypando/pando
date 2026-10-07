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
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

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

	mu      sync.Mutex
	checked time.Time
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
func (k *agentKeeper) digest(image string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		image, strconv.Itoa(k.port), k.pool.String(), k.authorities.Fingerprint(), k.host,
	}, "\n")))
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
			WithRemedy("Set agent_image in the Docker hosts runtime's settings to the image of this version of Pando, such as one in the install's registry.")
	}
	digest := k.digest(image)

	existing, err := k.cli.ContainerInspect(ctx, agentContainer, client.ContainerInspectOptions{})
	switch {
	case err == nil:
		c := existing.Container
		current := c.Config != nil && c.Config.Labels[labelAgentDigest] == digest && !stale(c.Config.Labels[labelAgentIssued])
		if current {
			if c.State == nil || !c.State.Running {
				if _, err := k.cli.ContainerStart(ctx, c.ID, client.ContainerStartOptions{}); err != nil {
					return false, errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not start the host agent on %s.", k.host), err)
				}
			}
			k.checked = time.Now()
			return false, nil
		}
		// An older version, another configuration, or a certificate due for
		// renewal: replaced. Its connections drop, and clients reconnect.
		if _, err := k.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return false, errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not replace the host agent on %s.", k.host), err)
		}
	case cerrdefs.IsNotFound(err):
	default:
		return false, errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("Docker on %s is not responding.", k.host), err)
	}

	networkID, err := k.ensureNetwork(ctx)
	if err != nil {
		return false, err
	}
	if err := k.pull(ctx, image); err != nil {
		return false, err
	}

	certPEM, keyPEM, err := k.authorities.IssueServer(k.host)
	if err != nil {
		return false, errs.Wrap(errs.AdapterFailed, "Could not issue the host agent's certificate.", err)
	}
	port, err := network.ParsePort(strconv.Itoa(k.port) + "/tcp")
	if err != nil {
		return false, errs.Wrap(errs.ValidInvalid, "The agent port is not usable.", err)
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
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{agentNetwork: {NetworkID: networkID}},
	}
	created, err := k.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: cfg, HostConfig: hostCfg, NetworkingConfig: netCfg, Name: agentContainer,
	})
	if err != nil {
		return false, errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not create the host agent on %s.", k.host), err)
	}
	if _, err := k.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return false, errs.Wrap(errs.AdapterFailed,
			fmt.Sprintf("Could not start the host agent on %s. The likeliest cause is something else on the host using port %d.", k.host, k.port), err).
			WithRemedy("Free that port on the host, or set agent_port in the Docker hosts runtime's settings to one that is free.")
	}
	k.checked = time.Now()
	return true, nil
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
