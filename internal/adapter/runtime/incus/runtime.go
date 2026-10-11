package incus

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

// after is a wait of n seconds, through the sleep seam tests shorten.
func (a *Adapter) after(n int) <-chan time.Time {
	if a.sleep != nil {
		return a.sleep(time.Duration(n) * time.Second)
	}
	return time.After(time.Duration(n) * time.Second)
}

// ImportImage is not offered (ImageDelivery is registry only).
func (a *Adapter) ImportImage(context.Context, io.Reader) (string, error) {
	return "", errs.New(errs.PlanCapabilityUnsupported,
		"The Incus runtime takes images from a registry, not from Pando's host.").
		WithRemedy("Add an image registry adapter under System → Adapters, or with pando adapter add image_registry/oci, so builds are pushed there and each app's VM pulls what it runs.")
}

func (a *Adapter) Logs(ctx context.Context, ref api.WorkloadRef, opts api.LogOptions) (io.ReadCloser, error) {
	v, err := a.running(ctx, ref.BundleID)
	if err != nil {
		return nil, err
	}
	return v.rt.Logs(ctx, ref, opts)
}

func (a *Adapter) Exec(ctx context.Context, ref api.WorkloadRef, req api.ExecRequest) (api.ExecSession, error) {
	v, err := a.running(ctx, ref.BundleID)
	if err != nil {
		return nil, err
	}
	return v.rt.Exec(ctx, ref, req)
}

// Usage is the Docker adapter's reading inside the VM (R-245); the VM's own
// overhead is not an app's usage. A stopped VM's workloads use nothing.
func (a *Adapter) Usage(ctx context.Context, ref api.BundleRef) (api.BundleUsage, error) {
	obs, err := a.Observe(ctx, ref)
	if err != nil {
		return api.BundleUsage{}, err
	}
	if !obs.Exists {
		return api.BundleUsage{Reported: a.now().UTC()}, nil
	}
	v, err := a.running(ctx, ref.BundleID)
	if err != nil {
		out := api.BundleUsage{Reported: a.now().UTC()}
		for _, w := range obs.Workloads {
			out.Workloads = append(out.Workloads, api.WorkloadUsage{Workload: w.Name, DiskBytes: -1})
		}
		return out, nil
	}
	return v.rt.Usage(ctx, ref)
}

// Upstream is the workload's address as the Docker adapter in its VM
// answers, dialed through that VM's agent (O-45, R-023). The proxy decides
// the request before it calls Dial; the agent only carries it.
//
// Answered from memory when the VM's address is known: this runs on every
// proxied request.
func (a *Adapter) Upstream(ctx context.Context, ref api.WorkloadRef, port int) (api.Upstream, error) {
	a.mu.Lock()
	v, ok := a.vms[ref.BundleID]
	a.mu.Unlock()
	if !ok || !v.addr.IsValid() {
		var err error
		if v, err = a.running(ctx, ref.BundleID); err != nil {
			return api.Upstream{}, err
		}
		if !v.addr.IsValid() {
			return api.Upstream{}, errs.Newf(errs.AdapterUnavailable, "The app's VM %s has no address on %s yet.", v.name, a.config.Network)
		}
	}
	direct, err := v.rt.Upstream(ctx, ref, port)
	if err != nil {
		return api.Upstream{}, err
	}
	u, err := url.Parse(direct.URL)
	if err != nil {
		return api.Upstream{}, errs.Wrap(errs.AdapterFailed, "The app's address could not be read.", err)
	}
	name := u.Hostname()
	target, _ := strconv.Atoi(u.Port())
	addr := net.JoinHostPort(v.addr.String(), strconv.Itoa(a.config.AgentPort))
	tlsConfig := hostagent.ClientTLS(a.authorities, a.clientCert, v.name)
	return api.Upstream{
		URL: direct.URL,
		Dial: func(ctx context.Context) (net.Conn, error) {
			return hostagent.Dial(ctx, addr, tlsConfig, name, target)
		},
		PoolKey: v.name + "/" + name + ":" + strconv.Itoa(target),
	}, nil
}

// Trial runs in a throwaway VM, deleted afterwards, with the Docker adapter's
// Trial inside it (R-097): the trial is in the app's isolation class, and
// port and write observation work as on Docker.
func (a *Adapter) Trial(ctx context.Context, req api.TrialRequest) (api.TrialResult, error) {
	if req.TrialID == "" {
		return api.TrialResult{}, errs.New(errs.Internal, "A trial run needs an identifier.")
	}
	pool, err := a.storagePool(ctx)
	if err != nil {
		return api.TrialResult{}, err
	}
	if err := a.ensureACL(ctx); err != nil {
		return api.TrialResult{}, err
	}
	image, err := a.guestImage(ctx, pool)
	if err != nil {
		return api.TrialResult{}, err
	}
	name := trialName(req.TrialID)
	put := a.put(pool, "", nil, nil)
	delete(put.Config, keyBundle)
	delete(put.Config, keyWorkloads)
	put.Config[keyTrial] = req.TrialID
	put.Config["boot.autostart"] = "false"

	// Removed on every path, including a cancelled one: a trial that leaks
	// leaks a running copy of a stranger's app.
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		_ = a.stop(cleanup, name)
		_ = a.api.DeleteInstance(cleanup, name)
		a.forget("trial:" + req.TrialID)
	}()
	if err := a.api.CreateInstance(ctx, a.memberTarget(), incusapi.InstancesPost{
		Name: name, Type: incusapi.InstanceTypeVM, Source: image, InstancePut: put,
	}); err != nil {
		return api.TrialResult{}, errs.Wrap(errs.AdapterFailed, "Incus could not create a VM for the trial run.", err)
	}
	if err := a.start(ctx, name); err != nil {
		return api.TrialResult{}, err
	}
	addr, err := a.waitReady(ctx, name)
	if err != nil {
		return api.TrialResult{}, err
	}
	v, err := a.handle("trial:"+req.TrialID, name, addr)
	if err != nil {
		return api.TrialResult{}, err
	}
	return v.rt.Trial(ctx, req)
}

// Capacity is reported from Incus's own resources, never from the host
// (R-243): the members VMs may be placed on, their CPU threads and memory,
// and what the running instances the adapter can see are limited to.
func (a *Adapter) Capacity(ctx context.Context) (api.Capacity, error) {
	places, err := a.places(ctx)
	if err != nil {
		return api.Capacity{}, err
	}
	out := api.Capacity{RunningWorkloads: -1, Reported: a.now().UTC(), Details: map[string]any{}}
	var best *api.Fit
	running := 0
	for _, p := range places {
		out.TotalCPUMillis += p.cpu
		out.TotalMemoryBytes += p.memory
		out.TotalDiskBytes += p.disk
		running += p.running
		free := api.Fit{CPUMillis: max(p.cpu-p.committed.CPUMillis, 0), MemoryBytes: max(p.memory-p.committed.MemoryBytes, 0)}
		if p.free >= 0 && p.free < free.MemoryBytes {
			free.MemoryBytes = p.free
		}
		if best == nil || free.MemoryBytes > best.MemoryBytes {
			f := free
			best = &f
		}
	}
	if a.config.TotalCPUMillis > 0 && a.config.TotalCPUMillis < out.TotalCPUMillis {
		out.TotalCPUMillis = a.config.TotalCPUMillis
	}
	if a.config.TotalMemoryBytes > 0 && a.config.TotalMemoryBytes < out.TotalMemoryBytes {
		out.TotalMemoryBytes = a.config.TotalMemoryBytes
	}
	if len(places) > 1 {
		out.LargestFit = best
	}
	names := make([]string, 0, len(places))
	for _, p := range places {
		names = append(names, p.name)
	}
	out.Details["members"] = strings.Join(names, ", ")
	out.Details["project"] = a.config.Project
	out.Details["running_vms"] = running
	return out, nil
}

// LargestFitFor is LargestFit, counting the bundle's own VM as free where it
// runs, so a redeploy that fits where it is is never refused (R-242).
func (a *Adapter) LargestFitFor(ctx context.Context, bundleID string) (*api.Fit, error) {
	places, err := a.places(ctx)
	if err != nil {
		return nil, err
	}
	if len(places) <= 1 {
		return nil, nil
	}
	inst, _, err := a.api.Instance(ctx, instanceName(bundleID))
	own := api.Fit{}
	location := ""
	if err == nil && inst.Config[keyBundle] == bundleID && strings.EqualFold(inst.Status, "Running") {
		own = limitsOf(inst.Config)
		location = inst.Location
	}
	var best *api.Fit
	for _, p := range places {
		free := api.Fit{CPUMillis: max(p.cpu-p.committed.CPUMillis, 0), MemoryBytes: max(p.memory-p.committed.MemoryBytes, 0)}
		if p.free >= 0 && p.free < free.MemoryBytes {
			free.MemoryBytes = p.free
		}
		if p.name == location {
			free.CPUMillis += own.CPUMillis
			free.MemoryBytes += own.MemoryBytes
		}
		if best == nil || free.MemoryBytes > best.MemoryBytes {
			f := free
			best = &f
		}
	}
	// The bundle's VM is sized with the guest's overhead the plan does not
	// count; the planner compares the plan's limits against this.
	best.MemoryBytes = max(best.MemoryBytes-a.config.GuestOverheadBytes, 0)
	return best, nil
}

// InUse is what the running Pando VMs are using now, summed: memory as Incus
// reports it, CPU over one second.
func (a *Adapter) InUse(ctx context.Context) (api.InUse, error) {
	first, err := a.cpuTimes(ctx)
	if err != nil {
		return api.InUse{}, err
	}
	start := a.now()
	select {
	case <-ctx.Done():
		return api.InUse{}, ctx.Err()
	case <-a.after(1):
	}
	second, err := a.cpuTimes(ctx)
	if err != nil {
		return api.InUse{}, err
	}
	elapsed := a.now().Sub(start)
	out := api.InUse{Reported: a.now().UTC()}
	for name, s := range second {
		out.MemoryBytes += s.memory
		if f, ok := first[name]; ok && elapsed > 0 && s.cpuNanos >= f.cpuNanos {
			out.CPUMillis += int((s.cpuNanos - f.cpuNanos) * 1000 / elapsed.Nanoseconds())
		}
	}
	return out, nil
}

type vmUsage struct {
	cpuNanos int64
	memory   int64
}

func (a *Adapter) cpuTimes(ctx context.Context) (map[string]vmUsage, error) {
	all, err := a.api.Instances(ctx)
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not read the apps' VMs from Incus.", err)
	}
	out := map[string]vmUsage{}
	for _, inst := range all {
		if inst.Config[keyBundle] == "" || inst.State == nil || !strings.EqualFold(inst.Status, "Running") {
			continue
		}
		out[inst.Name] = vmUsage{cpuNanos: inst.State.CPU.Usage, memory: inst.State.Memory.Usage}
	}
	return out, nil
}

// place is one member VMs may be placed on, or the server.
type place struct {
	name         string
	cpu          int
	memory, disk int64
	free         int64 // memory free now, -1 when not known
	committed    api.Fit
	running      int
}

// places reads every member VMs may be placed on.
func (a *Adapter) places(ctx context.Context) ([]place, error) {
	members, err := a.members(ctx)
	if err != nil {
		return nil, err
	}
	all, err := a.api.Instances(ctx)
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not read the instances from Incus.", err)
	}
	read := func(member string) (place, error) {
		res, err := a.api.Resources(ctx, member)
		if err != nil {
			return place{}, errs.Wrap(errs.AdapterUnavailable, "Could not read the resources of Incus "+member+".", err)
		}
		p := place{name: member, cpu: int(clamp(res.CPU.Total)) * 1000, memory: clamp(res.Memory.Total), free: -1}
		if res.Memory.Used > 0 && res.Memory.Total >= res.Memory.Used {
			p.free = clamp(res.Memory.Total - res.Memory.Used)
		}
		for _, inst := range all {
			if member != "" && inst.Location != member {
				continue
			}
			if !strings.EqualFold(inst.Status, "Running") {
				continue
			}
			p.running++
			l := limitsOf(inst.Config)
			p.committed.CPUMillis += l.CPUMillis
			p.committed.MemoryBytes += l.MemoryBytes
		}
		return p, nil
	}
	if members == nil {
		p, err := read("")
		if err != nil {
			return nil, err
		}
		return []place{p}, nil
	}
	out := make([]place, 0, len(members))
	for _, m := range members {
		if !strings.EqualFold(m.Status, "Online") {
			continue
		}
		p, err := read(m.ServerName)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errs.New(errs.AdapterUnavailable, "No member of the Incus cluster that VMs may be placed on is online.")
	}
	return out, nil
}

// limitsOf reads an instance's CPU and memory limits. An instance with none
// commits nothing, as a container with none does on Docker.
func limitsOf(config map[string]string) api.Fit {
	var f api.Fit
	if n, err := strconv.Atoi(config["limits.cpu"]); err == nil {
		f.CPUMillis = n * 1000
	}
	if m, err := parseBytes(config["limits.memory"]); err == nil {
		f.MemoryBytes = m
	}
	return f
}

// parseBytes reads an Incus size: a number with an optional B, kB–EB or
// KiB–EiB suffix.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty")
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"kB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
		{"B", 1},
	}
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
			return v * u.mult, err
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// clamp is a count Incus reports, as an int64, never wrapping negative.
func clamp(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
