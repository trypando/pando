package kubernetes

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// TestR245_UsageComesFromTheClustersMetricsAPI asserts R-245 on Kubernetes:
// each workload's CPU and memory now, beside its limits, read from
// metrics.k8s.io, and the sum over every app's pods.
func TestR245_UsageComesFromTheClustersMetricsAPI(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)
	web := listPods(t, cs, testNS, "web")[0]
	setStatus(t, cs, web, running(true))

	podMetrics := func(ns, name, cpu, mem string) *metricsv1beta1.PodMetrics {
		return &metricsv1beta1.PodMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Containers: []metricsv1beta1.ContainerMetrics{{Name: appContainer, Usage: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem),
			}}},
		}
	}
	all := []metricsv1beta1.PodMetrics{
		*podMetrics(testNS, web.Name, "120m", "64Mi"),
		*podMetrics("monitoring", "prometheus-0", "2", "4Gi"),
	}
	mc := metricsfake.NewSimpleClientset()
	// The fake's tracker files PodMetrics under a resource name the client
	// does not list, so the list is answered here, as metrics-server would.
	mc.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ns := action.GetNamespace()
		out := &metricsv1beta1.PodMetricsList{}
		for _, m := range all {
			if ns == "" || m.Namespace == ns {
				out.Items = append(out.Items, m)
			}
		}
		return true, out, nil
	})
	a.mc = mc

	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.True(t, caps.ReportsUsage)

	usage, err := a.Usage(ctx, api.BundleRef{BundleID: testBundle})
	require.NoError(t, err)
	var got api.WorkloadUsage
	for _, w := range usage.Workloads {
		if w.Workload == "web" {
			got = w
		}
	}
	require.True(t, got.Running)
	require.Equal(t, 120, got.CPUMillis)
	require.Equal(t, int64(64<<20), got.MemoryBytes)
	require.Equal(t, 500, got.CPULimitMillis)
	require.Equal(t, int64(512<<20), got.MemoryLimitBytes)
	require.Equal(t, int64(-1), got.DiskBytes, "not reported, never zero")
	require.Equal(t, []api.VolumeUsage{{VolumeID: "vol_01DATA", Bytes: -1}}, usage.Volumes)

	in, err := a.InUse(ctx)
	require.NoError(t, err)
	require.Equal(t, 120, in.CPUMillis, "only apps' pods, not the cluster's other work")
}

// TestR245_WithoutMetricsServerUseIsNotReported asserts the degradation: a
// cluster that serves no metrics API says so, rather than reporting zeros.
func TestR245_WithoutMetricsServerUseIsNotReported(t *testing.T) {
	ctx := context.Background()
	a, _ := testAdapter(t, nil)
	absent := metricsfake.NewSimpleClientset()
	absent.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server could not find the requested resource")
	})
	a.mc = absent

	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.False(t, caps.ReportsUsage)
	_, err = a.Usage(ctx, api.BundleRef{BundleID: testBundle})
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "metrics-server")
}
