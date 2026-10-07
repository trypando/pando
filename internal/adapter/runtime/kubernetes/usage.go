package kubernetes

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// What apps are using now (R-245), from metrics.k8s.io: metrics-server, or
// anything serving its API. A cluster without it reports nothing, and the
// console says the runtime does not report use rather than showing zeros.
// Volume sizes are not reported: the kubelet's stats need nodes/proxy, which
// is close to node-level access, and the adapter does not ask for it.

// metricsRecheck is how long the answer to "does this cluster serve
// metrics.k8s.io" is kept.
const metricsRecheck = 5 * time.Minute

// metricsAvailable reports whether the cluster serves metrics.k8s.io.
func (a *Adapter) metricsAvailable(ctx context.Context) bool {
	if a.mc == nil {
		return false
	}
	a.metricsMu.Lock()
	defer a.metricsMu.Unlock()
	if !a.metricsChecked.IsZero() && a.now().Sub(a.metricsChecked) < metricsRecheck {
		return a.metricsOK
	}
	_, err := a.mc.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{Limit: 1})
	a.metricsOK, a.metricsChecked = err == nil, a.now()
	return a.metricsOK
}

func errMetrics(err error) error {
	return errs.Wrap(errs.AdapterUnavailable,
		"The cluster's metrics API did not answer, so Pando cannot say what apps are using.", err).
		WithRemedy("Install metrics-server in the cluster. Pando reports use again within five minutes of it answering.")
}

// Usage reports each workload's CPU and memory from its newest pod.
func (a *Adapter) Usage(ctx context.Context, ref api.BundleRef) (api.BundleUsage, error) {
	if !a.metricsAvailable(ctx) {
		return api.BundleUsage{}, errNoUsage()
	}
	ns := namespaceFor(ref.BundleID)
	pods, err := a.cs.CoreV1().Pods(ns).List(ctx, managedOnly)
	if err != nil {
		return api.BundleUsage{}, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}
	metrics, err := a.mc.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return api.BundleUsage{}, errMetrics(err)
	}
	byPod := map[string]metricsv1beta1.PodMetrics{}
	for _, m := range metrics.Items {
		byPod[m.Name] = m
	}

	byWorkload := map[string][]corev1.Pod{}
	for _, p := range pods.Items {
		if w := p.Labels[labelWorkload]; w != "" && p.Labels[labelRole] == "" {
			byWorkload[w] = append(byWorkload[w], p)
		}
	}
	usage := api.BundleUsage{Reported: a.now()}
	for name, list := range byWorkload {
		p := newest(list)
		w := api.WorkloadUsage{Workload: name, Running: p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil, DiskBytes: -1}
		for _, c := range p.Spec.Containers {
			if c.Name == appContainer {
				w.CPULimitMillis = int(c.Resources.Limits.Cpu().MilliValue())
				w.MemoryLimitBytes = c.Resources.Limits.Memory().Value()
			}
		}
		if m, ok := byPod[p.Name]; ok {
			for _, c := range m.Containers {
				if c.Name == appContainer {
					w.CPUMillis = int(c.Usage.Cpu().MilliValue())
					w.MemoryBytes = c.Usage.Memory().Value()
				}
			}
		}
		usage.Workloads = append(usage.Workloads, w)
	}
	claims, err := a.cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, managedOnly)
	if err == nil {
		for _, c := range claims.Items {
			usage.Volumes = append(usage.Volumes, api.VolumeUsage{VolumeID: c.Labels[labelVolume], Bytes: -1})
		}
	}
	return usage, nil
}

// InUse sums what every app's pods are using now.
func (a *Adapter) InUse(ctx context.Context) (api.InUse, error) {
	if !a.metricsAvailable(ctx) {
		return api.InUse{}, errNoUsage()
	}
	apps, err := a.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedBy + "," + labelBundle})
	if err != nil {
		return api.InUse{}, errs.Wrap(errs.AdapterUnavailable, "Could not read the apps in the cluster.", err)
	}
	ours := map[string]bool{}
	for _, n := range apps.Items {
		ours[n.Name] = true
	}
	metrics, err := a.mc.MetricsV1beta1().PodMetricses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return api.InUse{}, errMetrics(err)
	}
	in := api.InUse{Reported: a.now()}
	for _, m := range metrics.Items {
		if !ours[m.Namespace] {
			continue
		}
		for _, c := range m.Containers {
			in.CPUMillis += int(c.Usage.Cpu().MilliValue())
			in.MemoryBytes += c.Usage.Memory().Value()
		}
	}
	return in, nil
}
