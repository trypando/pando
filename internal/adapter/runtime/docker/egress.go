package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/egress"
	"github.com/trypando/pando/internal/errs"
)

// Egress enforcement (R-185 – R-187).
//
// An app whose rules restrict nothing runs exactly as it always has (R-186):
// its bundle network is the ordinary bridge "pando-<bundle>", it has no
// gateway, and its workloads get no proxy variables. Nothing in this file
// touches it.
//
// An app whose rules restrict something runs on a different network,
// "pando-<bundle>-internal", created with Docker's Internal flag: no route out
// of it at all. One container on it can leave — the gateway,
// "pando-egress-<bundle>", which is also on a network of its own with a route
// out, "pando-<bundle>-outbound". The workloads are told to use it through
// HTTP_PROXY and HTTPS_PROXY (R-187), and it lets out only what the rules
// allow (egress.Gateway). Traffic that does not go through it — raw TCP, a
// client that ignores the variables — has nowhere to go.
//
// The gateway's outbound network is its own rather than one shared by every
// gateway on the host. A shared one would put every restricted app's gateway
// on one bridge, where an app whose rules allow private addresses could ask its
// gateway to connect to another app's gateway, and through it into that app's
// internal network (R-180).
//
// Switching an app between the two is a change of network. Docker cannot turn
// Internal on or off on an existing network, and removing the old one in place
// would mean detaching Pando's own container from it first, which on Docker
// Desktop drops Pando's published ports (see Destroy). So the two postures
// have two network names: a workload whose container is on the other one does
// not match its plan and is recreated on the right one, and the network left
// behind is removed when nothing is on it, or by ReclaimNetworks at the next
// restart when only Pando was.

const (
	// labelRole marks a container in a bundle that is not one of the app's
	// workloads, so Observe and Usage do not report it as one.
	labelRole = "io.pando.role"

	// roleEgressGateway is labelRole's value on the egress gateway.
	roleEgressGateway = "egress-gateway"

	// labelEgressDigest summarizes the gateway's image and rules, so changed
	// rules are a gateway that no longer matches and is recreated — and only
	// the gateway: the workloads' configuration does not depend on the rules.
	labelEgressDigest = "io.pando.egress.digest"

	// labelEgressNetwork is on the two networks a restricted bundle adds,
	// naming which one it is.
	labelEgressNetwork = "io.pando.egress.network"

	egressNetworkInternal = "internal"
	egressNetworkOutbound = "outbound"

	// gatewayAlias is the gateway's name on a restricted bundle's network, the
	// same in every bundle, and the host in its workloads' proxy variables.
	gatewayAlias = "pando-egress"

	// gatewayBinary is where Pando's image has its binary, and so where a
	// configured gateway image must have it (Config.EgressGatewayImage).
	gatewayBinary = "/usr/local/bin/pando"

	// gatewayUser runs the gateway: the hardened base image's nonroot user.
	// By number, so it needs no entry in the image's /etc/passwd. The gateway
	// listens above 1024 and needs no privilege at all.
	gatewayUser = "65532:65532"

	// Resources for one gateway. It copies bytes between sockets; a quarter
	// of a core and 128 MiB carry far more than one app sends.
	gatewayNanoCPUs   = 250_000_000
	gatewayMemory     = 128 << 20
	gatewayPids       = 256
	gatewayLogBytes   = 10 << 20
	gatewayListenAddr = ":3128"
)

// gatewayProxyURL is what HTTP_PROXY and HTTPS_PROXY name.
var gatewayProxyURL = "http://" + gatewayAlias + ":" + strconv.Itoa(egress.DefaultPort)

func internalNetworkName(bundleID string) string { return "pando-" + bundleID + "-internal" }
func outboundNetworkName(bundleID string) string { return "pando-" + bundleID + "-outbound" }
func gatewayContainerName(bundleID string) string {
	return "pando-egress-" + bundleID
}

// workloadNetworkName is the network a bundle's workloads run on.
func workloadNetworkName(bundleID string, restricted bool) string {
	if restricted {
		return internalNetworkName(bundleID)
	}
	return bundleNetworkName(bundleID)
}

// gatewayImage is the image and binary the egress gateway runs.
type gatewayImage struct {
	ref    string
	binary string
}

// egressGateway returns what the gateway runs from, or the zero value when
// this adapter does not know — which is what SupportsEgressRestriction
// reports (R-186, R-254).
//
// The configured image first. Otherwise Pando's own: the image of the
// container this process runs in, by ID so an upgrade that moves a tag cannot
// put an older gateway in front of an app, and the binary that container
// runs. A Pando run on the host — a developer's machine — has no container
// to read, so it does not know, and a restricted plan is refused at plan time
// rather than deployed with its rules ignored.
func (a *Adapter) egressGateway(ctx context.Context) gatewayImage {
	if a.config.EgressGatewayImage != "" {
		return gatewayImage{ref: a.config.EgressGatewayImage, binary: gatewayBinary}
	}

	if a.cli == nil {
		return gatewayImage{}
	}
	a.selfMu.Lock()
	defer a.selfMu.Unlock()
	if a.selfKnown {
		return a.selfGateway
	}

	id := a.config.ProxyContainer
	if id == "" {
		host, err := os.Hostname()
		if err != nil {
			a.selfKnown = true
			return gatewayImage{}
		}
		id = host
	}
	self, err := a.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	switch {
	case err == nil:
	case cerrdefs.IsNotFound(err):
		a.selfKnown = true
		return gatewayImage{}
	default:
		// The daemon did not answer. Not remembered: the next call asks again.
		return gatewayImage{}
	}
	a.selfKnown = true

	// The container's process must be Pando, or its image is not known to
	// have Pando in it. The shipped image runs it through entrypoint.sh, so
	// it is an argument rather than the path.
	binary := ""
	for _, arg := range append([]string{self.Container.Path}, self.Container.Args...) {
		if path.IsAbs(arg) && path.Base(arg) == "pando" {
			binary = arg
			break
		}
	}
	ref := self.Container.Image
	if ref == "" && self.Container.Config != nil {
		ref = self.Container.Config.Image
	}
	if binary == "" || ref == "" {
		return gatewayImage{}
	}
	a.selfGateway = gatewayImage{ref: ref, binary: binary}
	return a.selfGateway
}

// proxyEnv is what a restricted bundle's workloads are given (R-187).
//
// NO_PROXY names every workload in the bundle: they reach each other directly
// on the bundle network, and a database client asked to go through an HTTP
// proxy to reach the database beside it would fail. Both spellings, because
// curl reads only the lower-case http_proxy and much else reads only the upper.
func proxyEnv(p api.BundlePlan) map[string]string {
	noProxy := []string{"localhost", "127.0.0.1", "::1"}
	names := make([]string, 0, len(p.Workloads))
	for _, w := range p.Workloads {
		names = append(names, w.Name)
	}
	sort.Strings(names)
	noProxy = append(noProxy, names...)

	np := strings.Join(noProxy, ",")
	return map[string]string{
		"HTTP_PROXY":  gatewayProxyURL,
		"HTTPS_PROXY": gatewayProxyURL,
		"http_proxy":  gatewayProxyURL,
		"https_proxy": gatewayProxyURL,
		"NO_PROXY":    np,
		"no_proxy":    np,
	}
}

// workloadEnv is a workload's environment as its container will have it: the
// plan's, with the proxy variables over it when the bundle is restricted. An
// app's own NO_PROXY is kept alongside the bundle's; its own HTTP_PROXY is
// not, because on a network with no route out the gateway is the only proxy
// there is.
func workloadEnv(w api.WorkloadPlan, proxy map[string]string) map[string]string {
	env := make(map[string]string, len(w.Env)+len(proxy))
	for k, v := range w.Env {
		// Revealed here, at the edge, into the container's own configuration.
		// The adapter received secret.Value and never learned which entries
		// were sensitive.
		env[k] = v.Reveal()
	}
	for k, v := range proxy {
		if (k == "NO_PROXY" || k == "no_proxy") && env[k] != "" {
			v = v + "," + env[k]
		}
		env[k] = v
	}
	return env
}

// egressRulesJSON is the rules as the gateway reads them, checked first: a
// rule the gateway would refuse at startup is refused here, where the deploy
// can say so, rather than as a gateway that never starts.
func egressRulesJSON(r api.EgressRules) (string, error) {
	if _, err := r.Compile(); err != nil {
		return "", errs.Wrap(errs.ValidInvalid, "This app's egress rules have an entry Pando cannot enforce.", err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not write down this app's egress rules.", err)
	}
	return string(raw), nil
}

func egressDigest(img gatewayImage, rules string) string {
	h := sha256.New()
	fmt.Fprintf(h, "image=%s\nbinary=%s\nrules=%s\n", img.ref, img.binary, rules)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ensureOutboundNetwork makes the network a bundle's gateway leaves through.
// Pando's own container is not attached to it: nothing on it is Pando's to
// reach.
func (a *Adapter) ensureOutboundNetwork(ctx context.Context, bundleID string) (string, error) {
	name := outboundNetworkName(bundleID)
	if n, ok, err := a.findNetwork(ctx, name); err != nil {
		return "", err
	} else if ok {
		return n.ID, nil
	}
	created, err := a.createNetwork(ctx, name, outboundBlockBits, client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: map[string]string{
			labelBundle: bundleID, labelManaged: "true", labelEgressNetwork: egressNetworkOutbound,
		},
	})
	if err != nil {
		return "", networkFailure(err, "Could not set up the network this app's egress gateway leaves through.")
	}
	return created.ID, nil
}

// findNetwork looks a network up by its exact name.
func (a *Adapter) findNetwork(ctx context.Context, name string) (network.Summary, bool, error) {
	list, err := a.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("name", name),
	})
	if err != nil {
		return network.Summary{}, false, errs.Wrap(errs.AdapterUnavailable, "Could not list Docker networks.", err)
	}
	for _, n := range list.Items {
		// The name filter matches substrings: "pando-x" finds
		// "pando-x-internal" too.
		if n.Name == name {
			return n, true, nil
		}
	}
	return network.Summary{}, false, nil
}

// ensureGateway converges a restricted bundle's gateway: there, running, on
// both its networks, from the right image with the right rules.
func (a *Adapter) ensureGateway(ctx context.Context, p api.BundlePlan, img gatewayImage, rules, internalID string) error {
	outboundID, err := a.ensureOutboundNetwork(ctx, p.BundleID)
	if err != nil {
		return err
	}
	digest := egressDigest(img, rules)

	existing, err := a.findGateway(ctx, p.BundleID)
	if err != nil {
		return err
	}
	if existing != nil {
		ok, err := a.gatewayMatches(ctx, existing.ID, p.BundleID, digest)
		if err != nil {
			return err
		}
		if ok {
			if !strings.HasPrefix(existing.State, "running") {
				if _, err := a.cli.ContainerStart(ctx, existing.ID, client.ContainerStartOptions{}); err != nil {
					return errs.Wrap(errs.AdapterFailed, "Could not start this app's egress gateway.", err)
				}
			}
			return nil
		}
		if err := a.removeContainer(ctx, existing.ID); err != nil {
			return err
		}
	}

	// The zero claim: the gateway's image is Pando's own and is kept, never
	// released with an app (images.go).
	if err := a.ensureImage(ctx, img.ref, imageClaim{}); err != nil {
		return err
	}

	pids := int64(gatewayPids)
	cfg := &container.Config{
		Image:      img.ref,
		Entrypoint: []string{img.binary},
		Cmd:        []string{"egress-gateway"},
		User:       gatewayUser,
		Env: []string{
			"PANDO_EGRESS_RULES=" + rules,
			"PANDO_EGRESS_LISTEN=" + gatewayListenAddr,
		},
		Labels: map[string]string{
			labelApp:          p.Labels["pando.app"],
			labelBundle:       p.BundleID,
			labelManaged:      "true",
			labelRole:         roleEgressGateway,
			labelEgressDigest: digest,
		},
	}
	limitLabels(cfg.Labels, gatewayNanoCPUs/1_000_000, gatewayMemory)
	hostCfg := &container.HostConfig{
		// Unlike a workload, which has no restart policy so the reconciler can
		// back off and give up (R-149 – R-151). The gateway is Pando's own and
		// has no failed state: while it is down the app can reach nothing, and
		// the reconciler, which watches workloads, would not notice. As the
		// edge does (edge.go).
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},

		// Nothing to write and nothing to escalate to.
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},

		Resources: container.Resources{
			NanoCPUs:  gatewayNanoCPUs,
			Memory:    gatewayMemory,
			PidsLimit: &pids,
		},
		LogConfig: logConfig(gatewayLogBytes),
	}
	// Created on the bundle's internal network, under the alias its workloads'
	// proxy variables name, and joined to its outbound network before it
	// starts. One network at create, because Podman and older Docker APIs
	// accept only one there.
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			internalNetworkName(p.BundleID): {NetworkID: internalID, Aliases: []string{gatewayAlias}},
		},
	}
	created, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: cfg, HostConfig: hostCfg, NetworkingConfig: netCfg, Name: gatewayContainerName(p.BundleID),
	})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not create this app's egress gateway.", err)
	}
	if _, err := a.cli.NetworkConnect(ctx, outboundID, client.NetworkConnectOptions{Container: created.ID}); err != nil {
		_ = a.removeContainer(ctx, created.ID)
		return errs.Wrap(errs.AdapterFailed, "Could not connect this app's egress gateway to the network it leaves through.", err)
	}
	if _, err := a.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not start this app's egress gateway.", err)
	}
	return nil
}

// gatewayMatches reports whether an existing gateway already satisfies the
// plan: same image and rules, and on both of this bundle's networks — a
// network removed and created again by hand is a different network.
func (a *Adapter) gatewayMatches(ctx context.Context, id, bundleID, digest string) (bool, error) {
	inspect, err := a.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return false, errs.Wrap(errs.AdapterUnavailable, "Could not read this app's egress gateway.", err)
	}
	if inspect.Container.Config == nil || inspect.Container.Config.Labels[labelEgressDigest] != digest {
		return false, nil
	}
	if inspect.Container.NetworkSettings != nil && inspect.Container.NetworkSettings.Networks != nil {
		for _, name := range []string{internalNetworkName(bundleID), outboundNetworkName(bundleID)} {
			if _, ok := inspect.Container.NetworkSettings.Networks[name]; !ok {
				return false, nil
			}
		}
	}
	return true, nil
}

func (a *Adapter) findGateway(ctx context.Context, bundleID string) (*containerSummary, error) {
	list, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelBundle+"="+bundleID).Add("label", labelRole+"="+roleEgressGateway),
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &containerSummary{ID: list.Items[0].ID, State: string(list.Items[0].State), Labels: list.Items[0].Labels}, nil
}

// removeGateway removes a bundle's gateway and its outbound network, left
// over from when the bundle was restricted. Nothing else is on that network,
// so it goes with no one detached.
func (a *Adapter) removeGateway(ctx context.Context, bundleID string) error {
	gw, err := a.findGateway(ctx, bundleID)
	if err != nil {
		return err
	}
	if gw != nil {
		if err := a.removeContainer(ctx, gw.ID); err != nil {
			return err
		}
	}
	a.removeIdleNetwork(ctx, outboundNetworkName(bundleID))
	return nil
}

// removeIdleNetwork removes a network nothing is attached to, and leaves one
// something is — Pando's own container included, which is never detached
// (see Destroy). ReclaimNetworks collects that one after the next restart.
func (a *Adapter) removeIdleNetwork(ctx context.Context, name string) {
	full, err := a.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil || len(full.Network.Containers) > 0 {
		return
	}
	_, _ = a.cli.NetworkRemove(ctx, full.Network.ID, client.NetworkRemoveOptions{})
}

// isGateway reports a container that is a bundle's egress gateway rather than
// one of its workloads.
func isGateway(labels map[string]string) bool { return labels[labelRole] == roleEgressGateway }

// networkFailure is a refused network create in an operator's terms.
func networkFailure(err error, what string) error {
	// Docker's default address pool holds about thirty /16 networks, and
	// Pando takes one per app (R-025). An install that grows past that fails
	// here with a message naming subnets, which tells an operator nothing
	// about what to do.
	if strings.Contains(err.Error(), "address pools") {
		return errs.Wrap(errs.CapacityWouldOversubscribe,
			"This machine has run out of private networks, so no more apps can start on it.", err).
			WithRemedy("Docker reserves a fixed pool of network addresses, and Pando uses one per app. Raise it by setting default-address-pools in /etc/docker/daemon.json — for example a /16 base with /24 subnets gives 256 apps instead of about 30 — then restart Docker. Deleting apps you no longer need also frees them.")
	}
	return errs.Wrap(errs.AdapterFailed, what, err)
}
