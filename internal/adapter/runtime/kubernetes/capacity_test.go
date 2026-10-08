package kubernetes

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/trypando/pando/internal/adapter/api"
)

func readyNode(name, cpu, mem string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func requesting(name, ns, node, cpu, mem string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
		}}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// podLists counts the cluster-wide pod lists a fake cluster answers.
func podLists(cs interface {
	PrependReactor(string, string, k8stesting.ReactionFunc)
}) func() int {
	var mu sync.Mutex
	n := 0
	cs.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "" {
			mu.Lock()
			n++
			mu.Unlock()
		}
		return false, nil, nil
	})
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// TestR242_OnePlanReadsTheClustersPodsOnce asserts R-242's capacity check
// reads every pod in the cluster once per plan, not once per question, and
// that no plan is answered from an earlier plan's reading: a pod started
// between two plans is counted by the second.
func TestR242_OnePlanReadsTheClustersPodsOnce(t *testing.T) {
	a, cs := testAdapter(t, nil, readyNode("a", "4", "8Gi"), requesting("other", "monitoring", "a", "1", "1Gi", corev1.PodRunning))
	lists := podLists(cs)

	plan := api.WithReadScope(context.Background())
	capacity, err := a.Capacity(plan)
	require.NoError(t, err)
	fit, err := a.LargestFitFor(plan, testBundle)
	require.NoError(t, err)
	require.Equal(t, 1, lists(), "Capacity and LargestFitFor in one plan share one reading")
	require.Equal(t, 3000, capacity.TotalCPUMillis)
	require.Equal(t, 3000, fit.CPUMillis)

	_, err = cs.CoreV1().Pods("monitoring").Create(context.Background(),
		requesting("more", "monitoring", "a", "2", "1Gi", corev1.PodRunning), metav1.CreateOptions{})
	require.NoError(t, err)
	next := api.WithReadScope(context.Background())
	capacity, err = a.Capacity(next)
	require.NoError(t, err)
	require.Equal(t, 2, lists(), "the next plan reads again")
	require.Equal(t, 1000, capacity.TotalCPUMillis, "and counts the pod started since")

	_, err = a.Capacity(context.Background())
	require.NoError(t, err)
	_, err = a.Capacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, 4, lists(), "outside a plan every question reads")
}

// TestR242_TheCapacityReadIsPagedAndSkipsFinishedPods asserts the read asks
// the API only for pods that hold room, a page at a time, sums every page,
// and starts again whole when the API server lets the list expire part way,
// rather than answer from part of the cluster.
func TestR242_TheCapacityReadIsPagedAndSkipsFinishedPods(t *testing.T) {
	a, cs := testAdapter(t, nil, readyNode("a", "64", "64Gi"))
	pods := make([]corev1.Pod, 0, 1200)
	for i := range 1200 {
		pods = append(pods, *requesting(fmt.Sprintf("p%d", i), "monitoring", "a", "10m", "10Mi", corev1.PodRunning))
	}
	pods = append(pods, *requesting("done", "monitoring", "a", "60", "60Gi", corev1.PodSucceeded))

	var (
		selectors []string
		limits    []int64
		expire    = true
	)
	cs.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != "" {
			return false, nil, nil
		}
		l := action.(k8stesting.ListActionImpl)
		selectors = append(selectors, l.GetListRestrictions().Fields.String())
		limits = append(limits, l.ListOptions.Limit)
		start := 0
		if c := l.ListOptions.Continue; c != "" {
			if expire {
				expire = false
				return true, nil, apierrors.NewResourceExpired("the continuation has expired")
			}
			start, _ = strconv.Atoi(c)
		}
		end := len(pods)
		if l.ListOptions.Limit > 0 {
			end = min(start+int(l.ListOptions.Limit), len(pods))
		}
		list := &corev1.PodList{Items: pods[start:end]}
		if end < len(pods) {
			list.Continue = strconv.Itoa(end)
		}
		return true, list, nil
	})

	capacity, err := a.Capacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, 64000-1200*10, capacity.TotalCPUMillis, "every page counted, the finished pod not")
	require.Equal(t, 1200, capacity.RunningWorkloads)
	require.Equal(t, []int64{capacityPage, capacityPage, 0}, limits, "a page, an expired page, then the whole list again")
	for _, s := range selectors {
		require.Contains(t, s, "status.phase!=Succeeded")
		require.Contains(t, s, "status.phase!=Failed")
	}

	expire = false
	limits = nil
	_, err = a.Capacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int64{capacityPage, capacityPage, capacityPage}, limits, "three pages of 500")
}

// TestCapacityReportsAFailedRead: a read that fails is an error, not a
// cluster with room.
func TestCapacityReportsAFailedRead(t *testing.T) {
	a, cs := testAdapter(t, nil, readyNode("a", "4", "8Gi"))
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("busy")
	})
	_, err := a.Capacity(api.WithReadScope(context.Background()))
	require.Error(t, err)
	cs.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("busy")
	})
	_, err = a.LargestFitFor(context.Background(), testBundle)
	require.Error(t, err)
}
