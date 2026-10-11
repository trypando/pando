package incus

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/adapter/runtime/incus/incusapi"
	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/errs"
)

// aclAllow is an Incus ACL rule's action: Incus vocabulary, not an audit
// action (internal/core/audit reads Action: "..." literals as those).
const aclAllow = "allow"

// namePrefix begins the name of everything the adapter makes in Incus.
const namePrefix = "pando-"

// msgReadVM is the message for failing to read an app's VM.
const msgReadVM = "Could not read the app's VM from Incus."

// nicName is the VM's one NIC (R-026).
const nicName = "eth0"

// instanceName is the VM for a bundle. Incus names are hostnames: letters,
// digits and dashes. Matched by keyBundle, never by name alone.
func instanceName(bundleID string) string { return namePrefix + hostname(bundleID) }

// trialName is the throwaway VM for a trial.
func trialName(trialID string) string { return namePrefix + "trial-" + hostname(trialID) }

// volumeName is an Incus custom volume for a Pando volume.
func volumeName(volumeID string) string { return namePrefix + hostname(volumeID) }

func hostname(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}

// aclName is the adapter's ingress ACL. One per project: ACLs on a bridge
// network live with the network, so two installs sharing a cluster in
// different projects must not share one.
func (a *Adapter) aclName() string {
	if a.config.Project == defaultProject {
		return "pando-ingress"
	}
	return "pando-ingress-" + hostname(a.config.Project)
}

// ensureACL makes the ingress ACL admit exactly the proxy's addresses on the
// agent's port, and nothing else (R-025, R-026). Every other inbound
// connection is dropped by the NIC's default action.
func (a *Adapter) ensureACL(ctx context.Context) error {
	sources := make([]string, 0, len(a.sources))
	for _, s := range a.sources {
		sources = append(sources, s.String())
	}
	want := incusapi.NetworkACLPut{
		Description: "Pando: only Pando's proxy reaches an app's VM, on the agent's port (R-026).",
		Ingress: []incusapi.NetworkACLRule{{
			Action:          aclAllow,
			Source:          strings.Join(sources, ","),
			Protocol:        "tcp",
			DestinationPort: strconv.Itoa(a.config.AgentPort),
			State:           "enabled",
			Description:     "Pando's proxy to the forwarding agent",
		}},
		Egress: []incusapi.NetworkACLRule{},
		Config: map[string]string{keyManaged: "true"},
	}
	have, etag, err := a.api.ACL(ctx, a.aclName())
	if forbidden(err) {
		// A bridge network's ACLs are in the project that owns the network,
		// usually default, whatever project the VMs are in.
		return errs.Newf(errs.AdapterUnavailable,
			"Incus refused Pando access to the network ACLs for %s. They are kept in the project that owns the network, and Pando's certificate is not allowed that project.", a.config.Network).
			WithRemedy("Allow Pando's certificate the network's project as well, with `incus config trust edit`, or use an OVN network in Pando's own project.")
	}
	if errors.Is(err, errNotFound) {
		err = a.api.CreateACL(ctx, incusapi.NetworkACLsPost{NetworkACLPut: want, NetworkACLPost: incusapi.NetworkACLPost{Name: a.aclName()}})
		if err != nil {
			return errs.Wrap(errs.AdapterFailed, "Could not create the network ACL that keeps apps' VMs private.", err)
		}
		return a.ensureNetworkACL(ctx)
	}
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the network ACL that keeps apps' VMs private.", err)
	}
	if !reflect.DeepEqual(have.Ingress, want.Ingress) || len(have.Egress) != 0 {
		if err := a.api.UpdateACL(ctx, a.aclName(), want, etag); err != nil {
			return errs.Wrap(errs.AdapterFailed, "Could not update the network ACL that keeps apps' VMs private.", err)
		}
	}
	return a.ensureNetworkACL(ctx)
}

// nic is the VM's one NIC: on the configured network, behind the ACL, with
// every inbound connection the ACL does not admit dropped. When the ACL is
// on the network instead (onNetwork), the NIC carries none of it.
func (a *Adapter) nic() map[string]string {
	d := map[string]string{
		"type":    "nic",
		"network": a.config.Network,
		"name":    nicName,
	}
	if a.bridge {
		// Bridge ACLs on the network apply only between the bridge and the
		// host, never between two instances on it. Port isolation is what
		// keeps one app's VM from reaching another's on the same bridge
		// (R-025); traffic routed through the host crosses the ACL.
		d["security.port_isolation"] = "true"
	}
	if !a.aclOnNetwork {
		d["security.acls"] = a.aclName()
		d["security.acls.default.ingress.action"] = "drop"
		d["security.acls.default.egress.action"] = "allow"
	}
	return d
}

// placeACL decides whether the ACL goes on each VM's NIC or on the network,
// from the network's project.
//
// On each NIC is the default: it is what keeps two apps apart on a network
// that other things share. But Incus 6.21 applies a NIC's ACLs from the
// instance's project, while a bridge network's ACLs live in the network's
// (default, for a project without features.networks), so a VM in Pando's
// own project fails to start with "Network ACL not found". There the ACL goes
// on the network, which Incus applies from the right project, and the network
// must be Pando's alone (checkDedicated).
func (a *Adapter) placeACL(n *incusapi.Network) {
	a.aclOnNetwork = n.Project != "" && n.Project != a.config.Project
	a.bridge = n.Type == "bridge"
}

// checkDedicated refuses, when the ACL goes on the network, a network that
// anything but this adapter's VMs uses: the ACL would close it to them too.
func (a *Adapter) checkDedicated(n *incusapi.Network) error {
	a.placeACL(n)
	if !a.aclOnNetwork {
		return nil
	}
	for _, user := range n.UsedBy {
		path, query, _ := strings.Cut(user, "?")
		name, isInstance := strings.CutPrefix(path, "/1.0/instances/")
		project := "default"
		for _, kv := range strings.Split(query, "&") {
			if v, ok := strings.CutPrefix(kv, "project="); ok {
				project = v
			}
		}
		if isInstance && project == a.config.Project && strings.HasPrefix(name, namePrefix) {
			continue
		}
		return errs.Newf(errs.AdapterUnavailable,
			"The network %s is in the project %s and Pando's VMs are in %s, so Pando has to put its network ACL on the whole network, and %s also uses it.",
			a.config.Network, n.Project, a.config.Project, user).
			WithRemedy("Create a network for Pando's VMs alone, such as `incus network create pando0`, and set network in " + settingIn + " to it. Or run the VMs in the network's project.")
	}
	return nil
}

// ensureNetworkACL puts the ACL on the network, dropping every other inbound
// connection, when that is where it goes.
func (a *Adapter) ensureNetworkACL(ctx context.Context) error {
	n, etag, err := a.api.Network(ctx, a.config.Network)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the network "+a.config.Network+" from Incus.", err)
	}
	if err := a.checkDedicated(n); err != nil {
		return err
	}
	if !a.aclOnNetwork {
		return nil
	}
	want := map[string]string{
		"security.acls":                        a.aclName(),
		"security.acls.default.ingress.action": "drop",
		"security.acls.default.egress.action":  "allow",
	}
	put := n.Writable()
	if put.Config == nil {
		put.Config = map[string]string{}
	}
	changed := false
	for k, v := range want {
		if put.Config[k] != v {
			put.Config[k] = v
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := a.api.UpdateNetwork(ctx, a.config.Network, put, etag); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not put the network ACL that keeps apps' VMs private on "+a.config.Network+".", err)
	}
	return nil
}

// size is the VM's CPUs and memory for a plan: its workloads' limits summed,
// plus the guest's overhead, or the defaults when no workload has a limit.
func (a *Adapter) size(workloads []api.WorkloadPlan) (int, int64) {
	var millis int
	var memory int64
	for _, w := range workloads {
		millis += w.Resources.CPUMillis
		memory += w.Resources.MemoryBytes
	}
	cpus := (millis + 999) / 1000
	if cpus == 0 {
		cpus = a.config.DefaultCPUs
	}
	if memory == 0 {
		memory = a.config.DefaultMemoryBytes
	}
	memory += a.config.GuestOverheadBytes
	// Incus takes whole MiB.
	memory = (memory + (1<<20 - 1)) &^ (1<<20 - 1)
	return cpus, memory
}

// put is what the VM for a plan should be, apart from its image.
func (a *Adapter) put(pool string, bundleID string, workloads []api.WorkloadPlan, volumes []api.VolumePlan) incusapi.InstancePut {
	cpus, memory := a.size(workloads)
	names := make([]string, 0, len(workloads))
	for _, w := range workloads {
		names = append(names, w.Name)
	}
	sort.Strings(names)
	devices := map[string]map[string]string{
		"root":  {"type": "disk", "path": "/", "pool": pool, "size": strconv.FormatInt(a.config.RootSizeBytes, 10) + "B"},
		nicName: a.nic(),
	}
	for _, v := range volumes {
		devices["vol-"+hostname(v.VolumeID)] = map[string]string{
			"type": "disk", "pool": pool, "source": volumeName(v.VolumeID),
			"path": volumeRoot + "/" + v.VolumeID,
		}
	}
	profiles := a.config.profiles()
	if profiles == nil {
		profiles = []string{}
	}
	return incusapi.InstancePut{
		Profiles: profiles,
		Config: map[string]string{
			"limits.cpu":          strconv.Itoa(cpus),
			"limits.memory":       strconv.FormatInt(memory>>20, 10) + "MiB",
			"security.secureboot": "false",
			"boot.autostart":      "true",
			keyBundle:             bundleID,
			keyWorkloads:          strings.Join(names, ","),
			keyManaged:            "true",
		},
		Devices: devices,
	}
}

// differs reports whether an existing VM's settings differ from what the
// plan wants, ignoring the keys Incus adds itself.
func differs(have *incusapi.Instance, want incusapi.InstancePut) bool {
	for k, v := range want.Config {
		if have.Config[k] != v {
			return true
		}
	}
	if !reflect.DeepEqual(have.Devices, want.Devices) {
		return true
	}
	return !reflect.DeepEqual(append([]string{}, have.Profiles...), want.Profiles)
}

// merged is have with want's keys over it, keeping what Incus keeps there
// (volatile.*, image.*).
func merged(have *incusapi.Instance, want incusapi.InstancePut) incusapi.InstancePut {
	out := have.Writable()
	if out.Config == nil {
		out.Config = map[string]string{}
	}
	for k, v := range want.Config {
		out.Config[k] = v
	}
	out.Devices = want.Devices
	out.Profiles = want.Profiles
	return out
}

// Apply converges the app's VM, then what runs in it, toward the plan.
func (a *Adapter) Apply(ctx context.Context, p api.BundlePlan) (api.BundleHandle, error) {
	if !p.Network.Private {
		// R-026, checked rather than assumed, as on Docker.
		return api.BundleHandle{}, errs.New(errs.AdapterFailed, "Pando will not start an app on a shared network.")
	}
	v, err := a.ensureVM(ctx, p.BundleID, p.Workloads, p.Volumes, p.FirstDeploy)
	if err != nil {
		return api.BundleHandle{}, err
	}
	if _, err := v.agent.Ensure(ctx); err != nil {
		return api.BundleHandle{}, err
	}
	if _, err := v.rt.Apply(ctx, p); err != nil {
		return api.BundleHandle{}, err
	}
	return api.BundleHandle{BundleID: p.BundleID, Handle: v.name}, nil
}

// ensureVM makes the bundle's VM exist, sized and attached as planned, and
// running, and returns it ready for the Docker adapter.
func (a *Adapter) ensureVM(ctx context.Context, bundleID string, workloads []api.WorkloadPlan, volumes []api.VolumePlan, _ bool) (*vm, error) {
	if err := a.ensureACL(ctx); err != nil {
		return nil, err
	}
	pool, err := a.storagePool(ctx)
	if err != nil {
		return nil, err
	}
	for _, vol := range volumes {
		if _, err := a.createVolume(ctx, pool, bundleID, vol.VolumeID, 0); err != nil {
			return nil, err
		}
	}
	name := instanceName(bundleID)
	want := a.put(pool, bundleID, workloads, volumes)

	have, etag, err := a.api.Instance(ctx, name)
	switch {
	case errors.Is(err, errNotFound):
		image, err := a.guestImage(ctx, pool)
		if err != nil {
			return nil, err
		}
		post := incusapi.InstancesPost{
			Name:        name,
			Type:        incusapi.InstanceTypeVM,
			Source:      image,
			InstancePut: want,
		}
		if err := a.api.CreateInstance(ctx, a.memberTarget(), post); err != nil {
			return nil, errs.Wrap(errs.AdapterFailed, "Incus could not create the app's VM.", err)
		}
	case err != nil:
		return nil, errs.Wrap(errs.AdapterUnavailable, msgReadVM, err)
	default:
		if have.Config[keyBundle] != bundleID {
			return nil, errs.Newf(errs.AdapterFailed,
				"Incus already has an instance named %s that is not this app's VM.", name).
				WithRemedy("Rename or remove that instance; Pando names its VMs pando-<app ID>.")
		}
		if differs(have, want) {
			if err := a.resize(ctx, name, have, want, etag); err != nil {
				return nil, err
			}
		}
	}
	if err := a.start(ctx, name); err != nil {
		return nil, err
	}
	addr, err := a.waitReady(ctx, name)
	if err != nil {
		return nil, err
	}
	return a.handle(bundleID, name, addr)
}

// resize applies changed settings. Incus applies a CPU change and a memory
// increase to a running VM; anything it refuses live is applied by
// restarting the VM, which only Apply does, and only for a changed plan.
func (a *Adapter) resize(ctx context.Context, name string, have *incusapi.Instance, want incusapi.InstancePut, etag string) error {
	err := a.api.UpdateInstance(ctx, name, merged(have, want), etag)
	if err == nil {
		return nil
	}
	if !strings.EqualFold(have.Status, "Running") {
		return errs.Wrap(errs.AdapterFailed, "Incus could not change the app's VM.", err)
	}
	if err := a.stop(ctx, name); err != nil {
		return err
	}
	have, etag, err = a.api.Instance(ctx, name)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, msgReadVM, err)
	}
	if err := a.api.UpdateInstance(ctx, name, merged(have, want), etag); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Incus could not change the app's VM.", err)
	}
	return nil
}

// memberTarget is the member to create on when target names one; a group,
// or none, is Incus's placement.
func (a *Adapter) memberTarget() string { return a.config.Target }

func (a *Adapter) start(ctx context.Context, name string) error {
	st, err := a.api.InstanceState(ctx, name)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, msgReadVM, err)
	}
	if strings.EqualFold(st.Status, "Running") {
		return nil
	}
	if err := a.api.SetState(ctx, name, "start", false); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Incus could not start the app's VM.", err).
			WithRemedy("Check that the cluster has room for it: `incus info " + name + " --show-log` says why it did not start.")
	}
	return nil
}

// stop stops a VM, cleanly if it stops within a minute, forcibly if not.
func (a *Adapter) stop(ctx context.Context, name string) error {
	clean, cancel := context.WithTimeout(ctx, time.Minute)
	err := a.api.SetState(clean, name, "stop", false)
	cancel()
	if err == nil || errors.Is(err, errNotFound) {
		return nil
	}
	st, serr := a.api.InstanceState(ctx, name)
	if serr == nil && strings.EqualFold(st.Status, "Stopped") {
		return nil
	}
	if err := a.api.SetState(ctx, name, "stop", true); err != nil && !errors.Is(err, errNotFound) {
		return errs.Wrap(errs.AdapterFailed, "Incus could not stop the app's VM.", err)
	}
	return nil
}

// waitReady waits for the VM's Podman to answer and its NIC to have an
// address, and returns the address.
func (a *Adapter) waitReady(ctx context.Context, name string) (netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, a.readyWait)
	defer cancel()
	for {
		addr, ok := a.address(ctx, name)
		if ok {
			code, err := a.api.Exec(ctx, name, []string{"test", "-S", podmanSocket}, nil, nil, nil)
			if err == nil && code == 0 {
				return addr, nil
			}
		}
		select {
		case <-ctx.Done():
			if !ok {
				return netip.Addr{}, errs.Newf(errs.AdapterFailed,
					"The app's VM %s started, and got no IPv4 address on the network %s within %s.", name, a.config.Network, a.readyWait).
					WithRemedy("Check that the network hands out addresses (DHCP) and that the VM booted: `incus console " + name + " --show-log`.")
			}
			return netip.Addr{}, errs.Newf(errs.AdapterFailed,
				"The app's VM %s started at %s, and Podman inside it did not answer within %s.", name, addr, a.readyWait).
				WithRemedy("Look at the VM's console with `incus console " + name + " --show-log`. A guest image of your own must serve Podman's API on " + podmanSocket + ".")
		case <-a.after(2):
		}
	}
}

// address is the VM's IPv4 address on its NIC.
func (a *Adapter) address(ctx context.Context, name string) (netip.Addr, bool) {
	inst, _, err := a.api.Instance(ctx, name)
	if err != nil {
		return netip.Addr{}, false
	}
	st, err := a.api.InstanceState(ctx, name)
	if err != nil || st == nil {
		return netip.Addr{}, false
	}
	return addressOf(st, inst.Config["volatile."+nicName+".hwaddr"])
}

// addressOf finds the NIC's IPv4 address in the guest's own report. A VM's
// guest names its interfaces itself (enp5s0, not eth0), so the NIC is found
// by the MAC address Incus gave it; Podman's bridges have others.
func addressOf(st *incusapi.InstanceState, hwaddr string) (netip.Addr, bool) {
	for _, n := range st.Network {
		if hwaddr == "" || !strings.EqualFold(n.Hwaddr, hwaddr) {
			continue
		}
		for _, ad := range n.Addresses {
			if ad.Family != "inet" || ad.Scope != "global" {
				continue
			}
			if addr, err := netip.ParseAddr(ad.Address); err == nil {
				return addr, true
			}
		}
	}
	return netip.Addr{}, false
}

// handle is the in-memory handle on a VM: its Docker adapter and agent,
// made once per VM and kept.
func (a *Adapter) handle(bundleID, name string, addr netip.Addr) (*vm, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := a.vms[bundleID]; ok {
		if addr.IsValid() {
			v.addr = addr
		}
		return v, nil
	}
	cli, err := a.newDocker(a.api, name)
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not set up the connection to Podman in the app's VM.", err)
	}
	v := &vm{
		name: name,
		rt: a.newInner(cli, docker.Config{
			ProxyContainer:     multidocker.AgentContainer,
			NetworkPool:        a.pool.String(),
			NetworkBlockBits:   a.config.NetworkBlockBits,
			EgressGatewayImage: a.config.AgentImage,
			VolumeRoot:         volumeRoot,
		}),
		agent: a.newAgent(cli, name),
		addr:  addr,
	}
	a.vms[bundleID] = v
	return v, nil
}

func (a *Adapter) forget(bundleID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.vms, bundleID)
}

// running returns the handle on a bundle's VM when it exists and runs.
func (a *Adapter) running(ctx context.Context, bundleID string) (*vm, error) {
	name := instanceName(bundleID)
	inst, _, err := a.api.Instance(ctx, name)
	if errors.Is(err, errNotFound) || (err == nil && inst.Config[keyBundle] != bundleID) {
		return nil, errs.Newf(errs.NotFound, "The app has no VM on the Incus runtime.")
	}
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, msgReadVM, err)
	}
	if !strings.EqualFold(inst.Status, "Running") {
		return nil, errs.Newf(errs.AdapterFailed, "The app's VM %s is %s.", name, strings.ToLower(inst.Status)).
			WithRemedy("Start the app; its VM starts with it.")
	}
	a.mu.Lock()
	v, ok := a.vms[bundleID]
	a.mu.Unlock()
	if ok && v.addr.IsValid() {
		return v, nil
	}
	addr, _ := a.address(ctx, name)
	return a.handle(bundleID, name, addr)
}

// Observe reports what exists, and never starts or changes anything (R-148).
func (a *Adapter) Observe(ctx context.Context, ref api.BundleRef) (api.ObservedBundle, error) {
	name := instanceName(ref.BundleID)
	inst, _, err := a.api.Instance(ctx, name)
	if errors.Is(err, errNotFound) || (err == nil && inst.Config[keyBundle] != ref.BundleID) {
		return api.ObservedBundle{Exists: false}, nil
	}
	if err != nil {
		return api.ObservedBundle{}, errs.Wrap(errs.AdapterUnavailable, msgReadVM, err)
	}
	if !strings.EqualFold(inst.Status, "Running") {
		// The workloads are in the stopped VM, and not running. Named from
		// the VM's own record, since Podman cannot be asked.
		out := api.ObservedBundle{Exists: true}
		for _, w := range strings.Split(inst.Config[keyWorkloads], ",") {
			if w != "" {
				out.Workloads = append(out.Workloads, api.ObservedWorkload{Name: w, Present: true})
			}
		}
		return out, nil
	}
	v, err := a.running(ctx, ref.BundleID)
	if err != nil {
		return api.ObservedBundle{}, err
	}
	obs, err := v.rt.Observe(ctx, ref)
	if err != nil {
		return api.ObservedBundle{}, errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("Could not read what runs in the app's VM %s.", name), err)
	}
	return obs, nil
}

// Stop stops the app's VM: a stopped app holds no CPU or memory (R-242).
func (a *Adapter) Stop(ctx context.Context, ref api.BundleRef) error {
	name := instanceName(ref.BundleID)
	if _, _, err := a.api.Instance(ctx, name); errors.Is(err, errNotFound) {
		return nil
	}
	return a.stop(ctx, name)
}

// Destroy deletes the app's VM. Its volumes stay unless KeepVolumes is
// false (R-204).
func (a *Adapter) Destroy(ctx context.Context, ref api.BundleRef, opts api.DestroyOptions) error {
	name := instanceName(ref.BundleID)
	inst, _, err := a.api.Instance(ctx, name)
	switch {
	case errors.Is(err, errNotFound):
	case err != nil:
		return errs.Wrap(errs.AdapterUnavailable, msgReadVM, err)
	case inst.Config[keyBundle] != ref.BundleID:
		// Not ours; nothing of this app's to remove.
	default:
		if err := a.stop(ctx, name); err != nil {
			return err
		}
		if err := a.api.DeleteInstance(ctx, name); err != nil && !errors.Is(err, errNotFound) {
			return errs.Wrap(errs.AdapterFailed, "Incus could not delete the app's VM.", err)
		}
	}
	a.forget(ref.BundleID)
	if opts.KeepVolumes {
		return nil
	}
	return a.destroyBundleVolumes(ctx, ref.BundleID)
}
