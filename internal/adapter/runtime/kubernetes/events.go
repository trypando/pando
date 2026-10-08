package kubernetes

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// workloadPods selects the pods that are an app's workloads: Pando's, with a
// workload, and not one of its helpers (the egress gateway, a volume helper).
// Edge and trial pods carry no workload label.
var workloadPods = labelManagedBy + "=" + managedBy + "," + labelWorkload + ",!" + labelRole

// listPage is how many pods one list call reads when a watch starts.
const listPage = 500

// WatchBundles watches every pod that is an app's workload, across the
// cluster, and reports what changed (O-52). It reports and never acts
// (R-148): with restartPolicy Never an exited pod stays exited until the
// reconciler, under backoff, makes another (R-151).
//
// One watch for the cluster rather than one per app namespace: Pando's role
// already reads pods cluster-wide for capacity (deploy/kubernetes/rbac.yaml),
// and thousands of watches would be thousands of open streams. The pods are
// listed first and the watch starts from that list's version, so only changes
// after the start are reported; the caller looks at every app when Watching
// arrives. A watch the API server closes on its own timeout is reopened from
// the last version seen without returning; a watch that cannot be resumed —
// the version has expired — returns, and the caller lists and sweeps again.
func (a *Adapter) WatchBundles(ctx context.Context, sink func(api.BundleEvent)) error {
	if a.cs == nil {
		return errs.New(errs.AdapterUnavailable, "The Kubernetes runtime has not been set up.")
	}
	pods := a.cs.CoreV1().Pods(metav1.NamespaceAll)

	seen := map[types.UID]podFacts{}
	version := ""
	opts := metav1.ListOptions{LabelSelector: workloadPods, Limit: listPage}
	for {
		list, err := pods.List(ctx, opts)
		if err != nil {
			return errs.Wrap(errs.AdapterUnavailable, "Could not list the cluster's pods to follow them.", err)
		}
		for i := range list.Items {
			if isWorkloadPod(&list.Items[i]) {
				seen[list.Items[i].UID] = factsOf(&list.Items[i])
			}
		}
		version = list.ResourceVersion
		if list.Continue == "" {
			break
		}
		opts.Continue = list.Continue
	}

	open := func() (watch.Interface, error) {
		w, err := pods.Watch(ctx, metav1.ListOptions{
			LabelSelector: workloadPods, ResourceVersion: version, AllowWatchBookmarks: true,
		})
		if err != nil {
			return nil, errs.Wrap(errs.AdapterUnavailable, "Could not watch the cluster's pods.", err)
		}
		return w, nil
	}
	w, err := open()
	if err != nil {
		return err
	}
	defer func() { w.Stop() }()
	sink(api.BundleEvent{Kind: api.BundleEventWatching})

	unattributed := map[types.UID]bool{}
	for {
		var ev watch.Event
		var ok bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok = <-w.ResultChan():
		}
		if !ok {
			// The API server ends every watch after a while. Carry on from
			// the last version seen, so nothing in between is lost.
			w.Stop()
			if w, err = open(); err != nil {
				return err
			}
			continue
		}

		switch ev.Type {
		case watch.Error:
			status := apierrors.FromObject(ev.Object)
			return errs.Wrap(errs.AdapterUnavailable, "Lost the watch on the cluster's pods.", status)
		case watch.Bookmark:
			if p, ok := ev.Object.(*corev1.Pod); ok {
				version = p.ResourceVersion
			}
			continue
		}

		p, ok := ev.Object.(*corev1.Pod)
		if !ok {
			continue
		}
		if p.ResourceVersion != "" {
			version = p.ResourceVersion
		}
		if !isWorkloadPod(p) {
			continue
		}

		prev, had := seen[p.UID]
		var kinds []api.BundleEventKind
		if ev.Type == watch.Deleted {
			delete(seen, p.UID)
			kinds = []api.BundleEventKind{api.BundleEventRemoved}
		} else {
			now := factsOf(p)
			seen[p.UID] = now
			kinds = changes(prev, had, now)
		}
		if len(kinds) == 0 {
			continue
		}

		bundle := p.Annotations[annoBundleID]
		if bundle == "" {
			// A pod made before pods named their bundle. Its namespace names
			// it only lowercased, so ask for everything to be looked at,
			// once per pod.
			if !unattributed[p.UID] {
				unattributed[p.UID] = true
				sink(api.BundleEvent{Kind: api.BundleEventMissed})
			}
			continue
		}
		for _, kind := range kinds {
			sink(api.BundleEvent{Kind: kind, BundleID: bundle, Workload: p.Labels[labelWorkload], At: a.now().UTC()})
		}
	}
}

// isWorkloadPod is workloadPods in code, since not every client applies a
// watch's selector.
func isWorkloadPod(p *corev1.Pod) bool {
	return p.Labels[labelManagedBy] == managedBy && p.Labels[labelWorkload] != "" && p.Labels[labelRole] == ""
}

// podFacts is what a pod's events are worked out from: the parts of it that
// Observe reports.
type podFacts struct {
	running  bool
	ended    bool
	oom      bool
	ready    bool
	deleting bool
}

func factsOf(p *corev1.Pod) podFacts {
	f := podFacts{deleting: p.DeletionTimestamp != nil}
	for _, s := range p.Status.ContainerStatuses {
		if s.Name != appContainer {
			continue
		}
		switch {
		case s.State.Running != nil:
			f.running = true
		case s.State.Terminated != nil:
			f.ended = true
			f.oom = s.State.Terminated.Reason == "OOMKilled"
		}
		f.ready = s.Ready
	}
	if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
		f.ended = true
	}
	return f
}

// changes is what happened between two readings of a pod. A pod not seen
// before is new: it is reported once it runs, not while it is scheduled.
func changes(prev podFacts, had bool, now podFacts) []api.BundleEventKind {
	if !had {
		prev = podFacts{}
	}
	var out []api.BundleEventKind
	switch {
	case now.ended && !prev.ended && now.oom:
		out = append(out, api.BundleEventOOMKilled)
	case now.ended && !prev.ended:
		out = append(out, api.BundleEventExited)
	case now.running && !prev.running:
		out = append(out, api.BundleEventStarted)
	case now.running && prev.running && now.ready != prev.ready:
		out = append(out, api.BundleEventHealth)
	}
	if now.deleting && !prev.deleting {
		out = append(out, api.BundleEventRemoved)
	}
	return out
}
