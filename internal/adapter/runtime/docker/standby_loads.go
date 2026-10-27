package docker

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// What a standby's containers say about it, for the failover reservation
// (R-422). The multi-host adapter writes these into a standby's plan as
// pando.standby* labels; a standby's containers carry each as io.pando.*, so
// one container list tells what every standby on this host is for and what
// it takes when it starts. A standby is stopped, so Committed never counts
// it: this is the only place its limits are read.
const (
	// planLabelStandby is the plan label naming the bundle a standby is for;
	// its siblings start with it.
	planLabelStandby = "pando.standby"

	labelStandbyPrimary  = labelStandby + ".primary"
	labelStandbyPriority = labelStandby + ".priority"
	labelStandbyDisk     = labelStandby + ".disk"
)

// StandbyLoad is one standby on this host: the bundle it is for, the host
// that bundle ran on when the standby was made, its failover priority, what
// its workloads are limited to, and the disk its copies are counted at.
type StandbyLoad struct {
	Bundle   string
	Primary  string
	Priority int
	Need     api.Fit
	// DiskBytes is what the multi-host adapter counted the copies at.
	DiskBytes int64
}

// standbyLabels is the plan's pando.standby* labels as container labels,
// io.pando.standby*: empty for a plan that is not a standby's.
func standbyLabels(p api.BundlePlan) map[string]string {
	out := map[string]string{}
	for k, v := range p.Labels {
		if k == planLabelStandby || strings.HasPrefix(k, planLabelStandby+".") {
			out["io."+k] = v
		}
	}
	return out
}

// sameStandbyLabels reports whether a container carries exactly the plan's
// standby labels, so a standby made for another primary or priority is made
// again rather than kept with what it said before.
func sameStandbyLabels(have map[string]string, p api.BundlePlan) bool {
	want := standbyLabels(p)
	for k, v := range have {
		if (k == labelStandby || strings.HasPrefix(k, labelStandby+".")) && want[k] != v {
			return false
		}
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// StandbyLoads lists every standby on this host, from its containers'
// labels, sorted by bundle.
func (a *Adapter) StandbyLoads(ctx context.Context) ([]StandbyLoad, error) {
	list, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelStandby),
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not list the standbys on this host.", err)
	}
	byBundle := map[string]*StandbyLoad{}
	for _, c := range list.Items {
		addStandbyContainer(byBundle, c.Labels)
	}
	out := make([]StandbyLoad, 0, len(byBundle))
	for _, b := range slices.Sorted(maps.Keys(byBundle)) {
		out = append(out, *byBundle[b])
	}
	return out, nil
}

// addStandbyContainer adds one standby container's limits to its bundle's.
func addStandbyContainer(byBundle map[string]*StandbyLoad, labels map[string]string) {
	bundle := labels[labelStandby]
	if bundle == "" {
		return
	}
	load, ok := byBundle[bundle]
	if !ok {
		load = &StandbyLoad{Bundle: bundle, Primary: labels[labelStandbyPrimary]}
		load.Priority, _ = strconv.Atoi(labels[labelStandbyPriority])
		load.DiskBytes, _ = strconv.ParseInt(labels[labelStandbyDisk], 10, 64)
		byBundle[bundle] = load
	}
	cpu, _ := strconv.Atoi(labels[labelLimitCPU])
	mem, _ := strconv.ParseInt(labels[labelLimitMemory], 10, 64)
	load.Need.CPUMillis += cpu
	load.Need.MemoryBytes += mem
}
