package kubernetes

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// The cluster's room (R-242, R-243).
//
// Reading it lists every pod in the cluster, since pods that are not Pando's
// hold room on the same nodes: 2.5 s at 26,000 pods on kwok, and the planner
// asks twice per plan, Capacity then LargestFitFor
// (notes-kubernetes-scale-issue-72.md). So the reading is made once per plan
// (api.WithReadScope) and made cheaper: in pages, keeping only sums, so
// Pando never holds the whole list; without finished pods, which hold no
// room; and as protobuf (coreProtobuf).
//
// Not from a watch. WatchBundles follows only Pando's workload pods, and only
// while the reconciler runs it; a total kept from it would miss every other
// pod and lag by however far the watch is behind, and a plan answered from it
// could start an app on room that is already taken. A list is the cluster's
// state when it is read, which is what the refusal rests on.

// capacityPage is how many pods one call of the capacity read returns.
const capacityPage = 500

// unfinished selects pods that hold room: a finished pod's requests are free.
const unfinished = "status.phase!=" + string(corev1.PodSucceeded) + ",status.phase!=" + string(corev1.PodFailed)

// usage is what pods request.
type usage struct{ cpu, mem int64 }

// room is one reading of the cluster: its eligible nodes, and what pods on
// them request, apart for Pando's namespaces so a bundle's own can be
// counted as free for it.
type room struct {
	nodes   []corev1.Node
	other   map[string]usage            // node → requested outside Pando's namespaces
	pando   map[string]usage            // node → requested in Pando's namespaces
	byNS    map[string]map[string]usage // Pando namespace → node → requested
	running int
}

type roomKey struct{}

// capacity reads the cluster's room. ownNS, when set, is a bundle's namespace,
// whose pods' requests count as free in LargestFit.
func (a *Adapter) capacity(ctx context.Context, ownNS string) (api.Capacity, error) {
	r, err := api.ScopedRead(ctx, roomKey{}, func() (*room, error) { return a.readRoom(ctx) })
	if err != nil {
		return api.Capacity{}, errs.Wrap(errs.AdapterUnavailable, "Could not read how much room the cluster has.", err)
	}

	capacity := api.Capacity{RunningWorkloads: r.running, Reported: a.now()}
	perNode := make([]map[string]any, 0, len(r.nodes))
	for _, n := range r.nodes {
		allocCPU, allocMem := n.Status.Allocatable.Cpu().MilliValue(), n.Status.Allocatable.Memory().Value()
		other, pando := r.other[n.Name], r.pando[n.Name]
		// The bundle being planned: what it holds is free for it.
		own := r.byNS[ownNS][n.Name]
		pando.cpu -= own.cpu
		pando.mem -= own.mem

		cpu, mem := max(allocCPU-other.cpu, 0), max(allocMem-other.mem, 0)
		capacity.TotalCPUMillis += int(cpu)
		capacity.TotalMemoryBytes += mem
		free := api.Fit{CPUMillis: int(max(cpu-pando.cpu, 0)), MemoryBytes: max(mem-pando.mem, 0)}
		if f := capacity.LargestFit; f == nil || free.MemoryBytes > f.MemoryBytes ||
			(free.MemoryBytes == f.MemoryBytes && free.CPUMillis > f.CPUMillis) {
			capacity.LargestFit = &free
		}
		perNode = append(perNode, map[string]any{
			"node":                       n.Name,
			"allocatable_cpu_millis":     allocCPU,
			"allocatable_memory_bytes":   allocMem,
			"requested_by_others_millis": other.cpu,
			"requested_by_others_bytes":  other.mem,
			"kubelet_version":            n.Status.NodeInfo.KubeletVersion,
			"architecture":               n.Status.NodeInfo.Architecture,
		})
	}
	capacity.Details = map[string]any{
		"nodes":          perNode,
		"eligible_nodes": len(r.nodes),
		"note":           "Totals are the eligible nodes' allocatable CPU and memory, less what pods outside Pando's namespaces request on them. Volume storage comes from the cluster's storage class and is not counted.",
	}
	if a.config.NodeSelector != "" {
		capacity.Details["node_selector"] = a.config.NodeSelector
	}
	return capacity, nil
}

// readRoom reads the eligible nodes and sums what the pods on them request.
func (a *Adapter) readRoom(ctx context.Context) (*room, error) {
	nodes, err := a.eligibleNodes(ctx)
	if err != nil {
		return nil, err
	}
	ours, err := a.pandoNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	eligible := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		eligible[n.Name] = true
	}

	// Pages of one list are one snapshot of the cluster. A snapshot that
	// expires part way (the API server keeps a continuation only for a few
	// minutes) is read again from the start, without pages.
	limit := int64(capacityPage)
	for attempt := 0; ; attempt++ {
		r := &room{nodes: nodes, other: map[string]usage{}, pando: map[string]usage{}, byNS: map[string]map[string]usage{}}
		opts := metav1.ListOptions{FieldSelector: unfinished, Limit: limit}
		for {
			list, err := a.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, opts)
			if apierrors.IsResourceExpired(err) && attempt == 0 {
				break
			}
			if err != nil {
				return nil, err
			}
			for i := range list.Items {
				r.add(&list.Items[i], eligible, ours)
			}
			if list.Continue == "" {
				return r, nil
			}
			opts.Continue = list.Continue
		}
		limit = 0
	}
}

// add counts one pod's requests where it runs.
func (r *room) add(p *corev1.Pod, eligible, ours map[string]bool) {
	// Checked here too: not every server applies the field selector.
	if !eligible[p.Spec.NodeName] || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return
	}
	if p.Status.Phase == corev1.PodRunning {
		r.running++
	}
	cpu, mem := podRequests(*p)
	node := p.Spec.NodeName
	if !ours[p.Namespace] {
		u := r.other[node]
		r.other[node] = usage{u.cpu + cpu, u.mem + mem}
		return
	}
	u := r.pando[node]
	r.pando[node] = usage{u.cpu + cpu, u.mem + mem}
	if r.byNS[p.Namespace] == nil {
		r.byNS[p.Namespace] = map[string]usage{}
	}
	n := r.byNS[p.Namespace][node]
	r.byNS[p.Namespace][node] = usage{n.cpu + cpu, n.mem + mem}
}
