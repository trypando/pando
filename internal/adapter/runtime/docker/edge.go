package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// The edge: an install-scoped workload in front of Pando (R-174, design 03
// §4.4). Everything here is a separate path from Apply on purpose. An edge
// publishes host ports and an app's workloads never do (R-026); sharing code
// with the app path would put the ability to publish one step away from every
// app plan.

const (
	// labelEdge names the edge a container, volume or network belongs to.
	labelEdge = "io.pando.edge"

	// labelEdgeDigest summarizes everything in the plan but its environment,
	// so a changed plan is a container that no longer matches. The
	// environment is compared on the container itself, as matchesPlan does,
	// rather than hashed into a label: a label is readable by anyone who can
	// list containers, and a hash of a short token is a guess away from it.
	labelEdgeDigest = "io.pando.edge.digest"

	// edgeNetwork holds the edges and Pando's own container, and nothing
	// else. An edge can reach Pando's proxy and no workload (R-023).
	edgeNetwork = "pando-edge"

	// edgeLogBytes caps an edge's logs. The edge has no app to take a budget
	// from, and it is the log an operator reads when a certificate did not
	// issue, so it is kept rather than left to the daemon's default.
	edgeLogBytes = 20 << 20
)

func edgeContainerName(name string) string { return "pando-edge-" + name }
func edgeVolumeName(name, volume string) string {
	return "pando-edge-" + name + "-" + volume
}

// ApplyEdge converges an edge toward its plan.
func (a *Adapter) ApplyEdge(ctx context.Context, p api.EdgePlan) error {
	if p.Name == "" || p.Image == "" {
		return errs.New(errs.Internal, "An edge plan needs a name and an image.")
	}

	networkID, reachable, err := a.ensureEdgeNetwork(ctx, p.ProxyAlias)
	if err != nil {
		return err
	}

	binds, err := a.edgeBinds(ctx, p)
	if err != nil {
		return err
	}

	digest := edgeDigest(p, binds)
	existing, err := a.findEdge(ctx, p.Name)
	if err != nil {
		return err
	}
	if existing != nil {
		matches, err := a.edgeMatches(ctx, existing.ID, p, digest)
		if err != nil {
			return err
		}
		if matches {
			if !strings.HasPrefix(existing.State, "running") {
				if _, err := a.cli.ContainerStart(ctx, existing.ID, client.ContainerStartOptions{}); err != nil {
					return edgeStartFailure(p, err)
				}
			}
			return nil
		}
		if err := a.removeContainer(ctx, existing.ID); err != nil {
			return err
		}
	}

	// The zero claim: an edge's image is Pando's own helper and is kept, not
	// released with an app (images.go).
	if err := a.ensureImage(ctx, p.Image, imageClaim{}); err != nil {
		return err
	}

	env := make([]string, 0, len(p.Env))
	for k, v := range p.Env {
		// Revealed here and nowhere earlier, into the edge's own
		// configuration (R-194).
		env = append(env, k+"="+v.Reveal())
	}
	sort.Strings(env)

	exposed := network.PortSet{}
	published := network.PortMap{}
	for _, port := range p.Ports {
		cp, err := containerPort(port.Container, port.Protocol)
		if err != nil {
			return errs.Wrap(errs.ValidInvalid, fmt.Sprintf("%d is not a usable port.", port.Container), err)
		}
		exposed[cp] = struct{}{}
		published[cp] = append(published[cp], network.PortBinding{HostPort: strconv.Itoa(port.Host)})
	}

	cfg := &container.Config{
		Image:        p.Image,
		Cmd:          p.Args,
		Env:          env,
		ExposedPorts: exposed,
		Labels: map[string]string{
			labelEdge:       p.Name,
			labelEdgeDigest: digest,
			labelManaged:    "true",
		},
	}

	hostCfg := &container.HostConfig{
		Binds:        binds,
		PortBindings: published,

		// Unlike an app's workloads, which have no restart policy so the
		// reconciler can back off and give up (R-149 – R-151). An edge has no
		// backoff state and no failed state, and every app behind it is
		// unreachable while it is down.
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		LogConfig:     logConfig(edgeLogBytes),
	}
	if !reachable && p.ProxyAlias != "" {
		// Pando is not in a container — a developer running the binary on the
		// host — so it cannot join the edge network under an alias. The host
		// is where it is listening.
		hostCfg.ExtraHosts = []string{p.ProxyAlias + ":host-gateway"}
	}

	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			edgeNetwork: {NetworkID: networkID},
		},
	}

	created, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: cfg, HostConfig: hostCfg, NetworkingConfig: netCfg, Name: edgeContainerName(p.Name),
	})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not create the %s edge.", p.Name), err)
	}
	if _, err := a.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		// Left in place rather than removed: the next pass retries the start,
		// and an operator reading `docker ps -a` sees what failed.
		return edgeStartFailure(p, err)
	}
	return nil
}

// edgeStartFailure turns a failed start into something an operator can act
// on. By far the likeliest cause is a port something else already holds.
func edgeStartFailure(p api.EdgePlan, err error) error {
	msg := err.Error()
	low := false
	for _, port := range p.Ports {
		low = low || port.Host < 1024
	}
	if low && (strings.Contains(msg, "permission denied") || strings.Contains(msg, "rootlessport")) {
		// Rootless Podman: an unprivileged user cannot bind below 1024.
		return errs.Wrap(errs.AdapterFailed,
			fmt.Sprintf("The %s edge could not start because this container runtime is not allowed to use ports below 1024.", p.Name), err).
			WithRemedy("Set net.ipv4.ip_unprivileged_port_start=80 with sysctl on this machine, or set the routing adapter's HTTP and HTTPS ports to 8080 and 8443 or higher, then restart Pando.")
	}
	if strings.Contains(msg, "address already in use") || strings.Contains(msg, "port is already allocated") {
		ports := make([]string, 0, len(p.Ports))
		for _, port := range p.Ports {
			ports = append(ports, strconv.Itoa(port.Host))
		}
		return errs.Wrap(errs.AdapterFailed,
			fmt.Sprintf("The %s edge could not start because another program on this machine is already using port %s.",
				p.Name, strings.Join(ports, " or ")), err).
			WithRemedy("Stop the program using that port, or change the port in the routing adapter's settings and restart Pando.")
	}
	return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not start the %s edge.", p.Name), err)
}

// ObserveEdge reports what exists. It never remediates (design 05).
func (a *Adapter) ObserveEdge(ctx context.Context, name string) (api.EdgeState, error) {
	found, err := a.findEdge(ctx, name)
	if err != nil {
		return api.EdgeState{}, err
	}
	if found == nil {
		return api.EdgeState{}, nil
	}
	state := api.EdgeState{Present: true, Running: strings.HasPrefix(found.State, "running")}
	if !state.Running {
		state.Detail = fmt.Sprintf("The %s edge exists but is %s.", name, found.State)
	}
	return state, nil
}

// RemoveEdge removes an edge's container. Its volumes are kept: a certificate
// store outlives a restart of Pando with the edge briefly switched off.
func (a *Adapter) RemoveEdge(ctx context.Context, name string) error {
	found, err := a.findEdge(ctx, name)
	if err != nil || found == nil {
		return err
	}
	return a.removeContainer(ctx, found.ID)
}

// Edges names every edge this runtime has.
func (a *Adapter) Edges(ctx context.Context) ([]string, error) {
	list, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelEdge),
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, c := range list.Items {
		if n := c.Labels[labelEdge]; n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// EdgeVolumes is every volume an edge owns, for the full-host backup.
func (a *Adapter) EdgeVolumes(ctx context.Context) ([]api.VolumeHandle, error) {
	list, err := a.cli.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", labelEdge),
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not list the edge's storage.", err)
	}
	out := make([]api.VolumeHandle, 0, len(list.Items))
	for _, v := range list.Items {
		out = append(out, api.VolumeHandle{Handle: v.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
}

func (a *Adapter) findEdge(ctx context.Context, name string) (*containerSummary, error) {
	list, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelEdge+"="+name),
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &containerSummary{ID: list.Items[0].ID, State: string(list.Items[0].State), Labels: list.Items[0].Labels}, nil
}

// edgeMatches reports whether a container already satisfies the plan.
func (a *Adapter) edgeMatches(ctx context.Context, id string, p api.EdgePlan, digest string) (bool, error) {
	inspect, err := a.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return false, errs.Wrap(errs.AdapterUnavailable, "Could not read the edge's configuration.", err)
	}
	if inspect.Container.Config == nil || inspect.Container.Config.Labels[labelEdgeDigest] != digest {
		return false, nil
	}
	existing := map[string]string{}
	for _, kv := range inspect.Container.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			existing[k] = v
		}
	}
	for k, want := range p.Env {
		if existing[k] != want.Reveal() {
			return false, nil
		}
	}
	return true, nil
}

// edgeDigest summarizes the plan's non-secret parts and the storage its
// mounts resolved to.
func edgeDigest(p api.EdgePlan, binds []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "image=%s\n", p.Image)
	for _, arg := range p.Args {
		fmt.Fprintf(h, "arg=%s\n", arg)
	}
	for _, port := range p.Ports {
		fmt.Fprintf(h, "port=%d:%d/%s\n", port.Host, port.Container, protocolOf(port.Protocol))
	}
	for _, b := range binds {
		fmt.Fprintf(h, "bind=%s\n", b)
	}
	// The names of the environment, never the values: a variable added or
	// removed is a change, and a changed value is caught by edgeMatches.
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "env=%s\n", k)
	}
	fmt.Fprintf(h, "alias=%s\n", p.ProxyAlias)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// edgeBinds resolves an edge's mounts to Docker binds.
//
// A mount shared with Pando is resolved against Pando's own container: the
// same volume or host directory mounted there is mounted into the edge. That
// is the runtime answering what storage "the directory Pando writes routes to"
// is (R-251). Pando not being in a container means the path is on the host,
// and the host path is the storage.
func (a *Adapter) edgeBinds(ctx context.Context, p api.EdgePlan) ([]string, error) {
	var self *container.InspectResponse
	selfLoaded := false

	binds := make([]string, 0, len(p.Mounts))
	for _, m := range p.Mounts {
		var source string
		switch {
		case m.Volume != "":
			source = edgeVolumeName(p.Name, m.Volume)
			// Created explicitly, with labels, rather than left to Docker to
			// make on first mount: the label is how EdgeVolumes finds it for
			// the full-host backup (R-212). Idempotent.
			if _, err := a.cli.VolumeCreate(ctx, client.VolumeCreateOptions{
				Name:   source,
				Labels: map[string]string{labelEdge: p.Name, labelManaged: "true"},
			}); err != nil {
				return nil, errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not create storage for the %s edge.", p.Name), err)
			}
		case m.SharedWithPando != "":
			if !selfLoaded {
				self = a.inspectSelf(ctx)
				selfLoaded = true
			}
			if self == nil {
				source = m.SharedWithPando
				break
			}
			for _, mp := range self.Mounts {
				if mp.Destination != m.SharedWithPando {
					continue
				}
				if mp.Type == "volume" {
					source = mp.Name
				} else {
					source = mp.Source
				}
			}
			if source == "" {
				return nil, errs.New(errs.AdapterFailed,
					fmt.Sprintf("Pando's container has nothing mounted at %s, so the %s edge cannot see the routes Pando writes there.",
						m.SharedWithPando, p.Name)).
					WithRemedy(fmt.Sprintf("Mount a volume at %s in Pando's Compose file (the shipped file does), then restart Pando.", m.SharedWithPando))
			}
		default:
			return nil, errs.New(errs.Internal, "An edge mount needs a volume or a path shared with Pando.")
		}

		b := source + ":" + m.Path
		if m.ReadOnly {
			b += ":ro"
		}
		binds = append(binds, b)
	}
	return binds, nil
}

// inspectSelf returns Pando's own container, or nil when Pando is not in one.
func (a *Adapter) inspectSelf(ctx context.Context) *container.InspectResponse {
	id := a.config.ProxyContainer
	if id == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil
		}
		id = host
	}
	got, err := a.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil
	}
	return &got.Container
}

// ensureEdgeNetwork makes the network edges share with Pando and joins Pando
// to it under alias. reachable is false when Pando is not a container and so
// cannot be joined — the edge then reaches it on the host instead.
func (a *Adapter) ensureEdgeNetwork(ctx context.Context, alias string) (string, bool, error) {
	var networkID string
	existing, err := a.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("name", edgeNetwork),
	})
	if err != nil {
		return "", false, errs.Wrap(errs.AdapterUnavailable, "Could not list Docker networks.", err)
	}
	for _, n := range existing.Items {
		if n.Name == edgeNetwork {
			networkID = n.ID
		}
	}
	if networkID == "" {
		// Not through createNetwork's pool: this is one network for the whole
		// install, not one per app, and Docker's default pool has room for it.
		created, err := a.cli.NetworkCreate(ctx, edgeNetwork, client.NetworkCreateOptions{
			Driver: "bridge",
			Labels: map[string]string{labelEdge: "true", labelManaged: "true"},
		})
		if err != nil {
			return "", false, errs.Wrap(errs.AdapterFailed, "Could not set up the network Pando shares with its edge.", err)
		}
		networkID = created.ID
	}

	self := a.config.ProxyContainer
	if self == "" {
		host, err := os.Hostname()
		if err != nil {
			//nolint:nilerr // Not in a container; see ApplyEdge's ExtraHosts.
			return networkID, false, nil
		}
		self = host
	}
	var aliases []string
	if alias != "" {
		aliases = []string{alias}
	}
	_, err = a.cli.NetworkConnect(ctx, networkID, client.NetworkConnectOptions{
		Container: self, EndpointConfig: &network.EndpointSettings{Aliases: aliases},
	})
	switch {
	case err == nil, strings.Contains(err.Error(), "already exists"):
		return networkID, true, nil
	case cerrdefs.IsNotFound(err):
		return networkID, false, nil
	default:
		return "", false, errs.Wrap(errs.AdapterFailed,
			"Could not connect Pando to the network it shares with its edge, so the edge could not reach it.", err)
	}
}
