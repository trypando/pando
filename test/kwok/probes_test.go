//go:build kwokscale

package kwok_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// TestKwokPodCreateCost times creating a pod like an app's in a namespace
// with a ResourceQuota like the adapter's (ensureQuota) and in one without,
// at the same concurrency, so the report can say what the quota costs each
// pod create. Run after TestKwokScale on the same cluster (KWOK_KEEP=1) to
// measure it at that size: `go test -tags kwokscale -run
// TestKwokPodCreateCost ./test/kwok/` with PANDO_KWOK_KUBECONFIG set.
func TestKwokPodCreateCost(t *testing.T) {
	s := load(t)
	ctx := context.Background()
	c := connect(t, s.kubeconfig, 5000, 10000)
	n := envInt("PANDO_KWOK_PODCOST_N", 500)
	stamp := time.Now().Unix()

	var b strings.Builder
	b.WriteString("| namespaces | create pods p50 | p95 | p99 |\n|---|---:|---:|---:|\n")
	for _, quota := range []bool{false, true} {
		prefix := fmt.Sprintf("podcost-%d-%t-", stamp, quota)
		_, errs := each(ctx, 0, n, s.concurrency, func(i int) error {
			ns := fmt.Sprintf("%s%d", prefix, i)
			if _, err := c.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
				return err
			}
			if !quota {
				return nil
			}
			q := resource.MustParse("2")
			_, err := c.cs.CoreV1().ResourceQuotas(ns).Create(ctx, &corev1.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{Name: "pando-quota"},
				Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
					corev1.ResourceServicesNodePorts: resource.MustParse("0"), corev1.ResourceServicesLoadBalancers: resource.MustParse("0"),
					corev1.ResourceRequestsCPU: q, corev1.ResourceLimitsCPU: q,
					corev1.ResourceRequestsMemory: resource.MustParse("2Gi"), corev1.ResourceLimitsMemory: resource.MustParse("2Gi"),
				}},
			}, metav1.CreateOptions{})
			return err
		})
		if len(errs) > 0 {
			t.Fatal(errs[0])
		}
		// Every namespace's ServiceAccount, and every quota's first status,
		// in place before any pod is made, so only the create is timed.
		time.Sleep(30 * time.Second)
		c.rec.take()
		_, errs = each(ctx, 0, n, s.concurrency, func(i int) error {
			res := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")}
			_, err := c.cs.CoreV1().Pods(fmt.Sprintf("%s%d", prefix, i)).Create(ctx, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "web"},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{Name: "app", Image: "busybox", Resources: corev1.ResourceRequirements{Limits: res, Requests: res}}},
				},
			}, metav1.CreateOptions{})
			return err
		})
		q := quantiles(c.rec.take().calls["create pods"])
		label := "without a ResourceQuota"
		if quota {
			label = "with the adapter's ResourceQuota"
		}
		fmt.Fprintf(&b, "| %d %s | %s | %s | %s |\n", n, label, ms(q.p50), ms(q.p95), ms(q.p99))
		if len(errs) > 0 {
			t.Errorf("%d pod creates failed, first: %v", len(errs), errs[0])
		}
	}
	t.Log("\n" + b.String())
}

// TestKwokObservePass repeats the observe pass over the PANDO_KWOK_APPS apps
// a filled cluster already holds (KWOK_KEEP=1 after TestKwokScale), to show
// how much one pass's time varies from one to the next.
func TestKwokObservePass(t *testing.T) {
	s := load(t)
	ctx := context.Background()
	c := connect(t, s.kubeconfig, 5000, 10000)
	rt, rtg := adapters(t, ctx, s, c)
	n := s.steps[len(s.steps)-1]
	var b strings.Builder
	b.WriteString("| pass | apps | wall | calls | p50 | p95 | p99 | errors |\n|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for pass := 1; pass <= envInt("PANDO_KWOK_PASSES", 3); pass++ {
		c.rec.take()
		start := time.Now()
		_, errs := each(ctx, 0, n, s.observers, func(i int) error { return observe(ctx, rt, rtg, i) })
		wall := time.Since(start)
		snap := c.rec.take()
		q := quantiles(snap.all())
		fmt.Fprintf(&b, "| %d | %d | %s | %d | %s | %s | %s | %d |\n", pass, n, wall.Round(100*time.Millisecond), snap.total(), ms(q.p50), ms(q.p95), ms(q.p99), len(errs))
	}
	t.Log("\n" + b.String())
}
