package kubernetes

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestPodChangesAreTranslatedToBundleEvents pins which changes between two
// readings of a pod are reported (O-52).
func TestPodChangesAreTranslatedToBundleEvents(t *testing.T) {
	up := podFacts{running: true, ready: true}
	cases := []struct {
		name string
		prev podFacts
		had  bool
		now  podFacts
		want []api.BundleEventKind
	}{
		{"a new pod not yet running", podFacts{}, false, podFacts{}, nil},
		{"a new pod already running", podFacts{}, false, up, []api.BundleEventKind{api.BundleEventStarted}},
		{"starts", podFacts{}, true, up, []api.BundleEventKind{api.BundleEventStarted}},
		{"exits", up, true, podFacts{ended: true}, []api.BundleEventKind{api.BundleEventExited}},
		{"killed for memory", up, true, podFacts{ended: true, oom: true}, []api.BundleEventKind{api.BundleEventOOMKilled}},
		{"stays exited", podFacts{ended: true}, true, podFacts{ended: true}, nil},
		{"becomes unready", up, true, podFacts{running: true}, []api.BundleEventKind{api.BundleEventHealth}},
		{"nothing changed", up, true, up, nil},
		{"is being deleted", up, true, podFacts{running: true, ready: true, deleting: true}, []api.BundleEventKind{api.BundleEventRemoved}},
	}
	for _, c := range cases {
		require.Equal(t, c.want, changes(c.prev, c.had, c.now), c.name)
	}

	oom := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		{Name: appContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}},
	}}}
	require.Equal(t, podFacts{ended: true, oom: true}, factsOf(oom))
	require.Equal(t, podFacts{ended: true}, factsOf(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}))
}

func workloadPod(name, bundle string, status corev1.PodStatus) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS, UID: types.UID(name), ResourceVersion: "1",
			Labels: map[string]string{labelManagedBy: managedBy, labelWorkload: "web"},
		},
		Status: status,
	}
	if bundle != "" {
		p.Annotations = map[string]string{annoBundleID: bundle}
	}
	return p
}

// collector gathers what WatchBundles sends.
type collector struct {
	mu  sync.Mutex
	got []api.BundleEvent
}

func (c *collector) sink(e api.BundleEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, e)
}

func (c *collector) kinds() []api.BundleEventKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]api.BundleEventKind, 0, len(c.got))
	for _, e := range c.got {
		out = append(out, e.Kind)
	}
	return out
}

func (c *collector) waitFor(t *testing.T, kinds ...api.BundleEventKind) {
	t.Helper()
	require.Eventually(t, func() bool { return equalKinds(c.kinds(), kinds) }, 5*time.Second, time.Millisecond,
		"want %v", kinds)
}

func equalKinds(a, b []api.BundleEventKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestWatchBundlesFollowsTheClustersPods drives the watch through scripted
// watch events: what an existing pod does after the list, a new pod, a
// helper pod, a pod that predates the bundle annotation, the API server
// ending the watch, and a watch that cannot be resumed.
func TestWatchBundlesFollowsTheClustersPods(t *testing.T) {
	existing := workloadPod("web-1", testBundle, running(true))
	a, cs := testAdapter(t, nil, existing)

	first, second := watch.NewFakeWithChanSize(16, false), watch.NewFakeWithChanSize(16, false)
	var mu sync.Mutex
	var opened []string
	cs.PrependWatchReactor("pods", func(act k8stesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, act.(k8stesting.WatchActionImpl).WatchRestrictions.ResourceVersion)
		if len(opened) == 1 {
			return true, first, nil
		}
		return true, second, nil
	})

	c := &collector{}
	done := make(chan error)
	go func() { done <- a.WatchBundles(context.Background(), c.sink) }()
	c.waitFor(t, api.BundleEventWatching)

	// The pod known from the list exits for want of memory.
	oom := existing.DeepCopy()
	oom.ResourceVersion = "7"
	oom.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: appContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
	}}}
	first.Modify(oom)
	c.waitFor(t, api.BundleEventWatching, api.BundleEventOOMKilled)
	require.Equal(t, testBundle, c.got[1].BundleID, "named as core named it, not lowercased")
	require.Equal(t, "web", c.got[1].Workload)

	// Helpers and pods that are not workloads are not reported.
	helper := workloadPod("gateway", testBundle, running(true))
	helper.Labels[labelRole] = roleGateway
	first.Add(helper)
	first.Action(watch.Bookmark, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "9"}})

	// A new pod is reported once it runs, and when its readiness changes.
	fresh := workloadPod("web-2", testBundle, corev1.PodStatus{Phase: corev1.PodPending})
	first.Add(fresh)
	first.Modify(workloadPod("web-2", testBundle, running(false)))
	first.Modify(workloadPod("web-2", testBundle, running(true)))
	c.waitFor(t, api.BundleEventWatching, api.BundleEventOOMKilled, api.BundleEventStarted, api.BundleEventHealth)

	// A pod from before pods named their bundle asks for everything to be
	// looked at, once.
	old := workloadPod("web-old", "", running(true))
	first.Add(old)
	gone := workloadPod("web-old", "", exited(1))
	gone.ResourceVersion = "11"
	first.Modify(gone)
	c.waitFor(t, api.BundleEventWatching, api.BundleEventOOMKilled, api.BundleEventStarted, api.BundleEventHealth,
		api.BundleEventMissed)

	// The API server ends the watch: it is reopened from the last version
	// seen, and nothing is returned.
	first.Stop()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(opened) == 2
	}, 5*time.Second, time.Millisecond)
	mu.Lock()
	require.Equal(t, "11", opened[1], "resumed where the first watch ended")
	mu.Unlock()

	second.Delete(workloadPod("web-2", testBundle, running(true)))
	c.waitFor(t, api.BundleEventWatching, api.BundleEventOOMKilled, api.BundleEventStarted, api.BundleEventHealth,
		api.BundleEventMissed, api.BundleEventRemoved)

	// A watch that cannot be resumed ends the call.
	second.Error(&apierrors.NewResourceExpired("too old").ErrStatus)
	err := <-done
	require.Error(t, err)
}

// TestWatchBundlesSeesAPodPandoMade asserts the end to end path on the fake
// cluster: a pod Apply made carries its bundle, and its starting is reported
// under the ID core gave it.
func TestWatchBundlesSeesAPodPandoMade(t *testing.T) {
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(context.Background(), webPlan())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	c := &collector{}
	done := make(chan error)
	go func() { done <- a.WatchBundles(ctx, c.sink) }()
	c.waitFor(t, api.BundleEventWatching)

	pods := listPods(t, cs, testNS, "web")
	require.NotEmpty(t, pods)
	require.Equal(t, testBundle, pods[0].Annotations[annoBundleID])
	setStatus(t, cs, pods[0], running(true))
	c.waitFor(t, api.BundleEventWatching, api.BundleEventStarted)
	require.Equal(t, testBundle, c.got[1].BundleID)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// TestWatchBundlesReportsWhatStopsIt covers the ways a watch cannot start.
func TestWatchBundlesReportsWhatStopsIt(t *testing.T) {
	require.Error(t, (&Adapter{}).WatchBundles(context.Background(), func(api.BundleEvent) {}), "not set up")

	refused := apierrors.NewGenericServerResponse(http.StatusForbidden, "list", corev1.Resource("pods"), "", "no", 0, false)
	a, cs := testAdapter(t, nil)
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, refused })
	require.Error(t, a.WatchBundles(context.Background(), func(api.BundleEvent) {}), "the list is refused")

	a, cs = testAdapter(t, nil)
	cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, errors.New("no watch for you")
	})
	called := false
	require.Error(t, a.WatchBundles(context.Background(), func(api.BundleEvent) { called = true }), "the watch is refused")
	require.False(t, called, "nothing is said to be watched")
}

// TestWatchBundlesListsInPages asserts that the list a watch starts from is
// read a page at a time, so starting is not one enormous response.
func TestWatchBundlesListsInPages(t *testing.T) {
	a, cs := testAdapter(t, nil)
	var pages []string
	cs.PrependReactor("list", "pods", func(act k8stesting.Action) (bool, runtime.Object, error) {
		cont := act.(k8stesting.ListActionImpl).ListOptions.Continue
		pages = append(pages, cont)
		list := &corev1.PodList{Items: []corev1.Pod{*workloadPod("web-"+cont, testBundle, running(true))}}
		if cont == "" {
			list.Continue = "next"
		}
		list.ResourceVersion = "42"
		return true, list, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	err := a.WatchBundles(ctx, func(e api.BundleEvent) {
		if e.Kind == api.BundleEventWatching {
			cancel()
		}
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []string{"", "next"}, pages)
}
