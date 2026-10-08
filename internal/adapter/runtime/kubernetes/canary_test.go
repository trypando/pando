package kubernetes

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/trypando/pando/internal/errs"
)

func fakeClient() *fake.Clientset { return newFake() }

// kubelet plays the cluster's part in the canary: the server comes up, the
// admitted client connects, and the other client connects or not as the
// network plugin decides.
func kubelet(t *testing.T, cs *fake.Clientset, admittedExit, refusedExit int32, stop <-chan struct{}) {
	t.Helper()
	ctx := context.Background()
	done := map[string]bool{}
	for {
		select {
		case <-stop:
			return
		case <-time.After(2 * time.Millisecond):
		}
		pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, p := range pods.Items {
			if done[p.Namespace+"/"+p.Name] {
				continue
			}
			var status corev1.PodStatus
			switch p.Name {
			case "server":
				status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.1.7"}
			case "admitted":
				status = exited(admittedExit)
			case "refused":
				status = exited(refusedExit)
			}
			p.Status = status
			if _, err := cs.CoreV1().Pods(p.Namespace).UpdateStatus(ctx, &p, metav1.UpdateOptions{}); err == nil {
				done[p.Namespace+"/"+p.Name] = true
			}
		}
	}
}

func runCanaryAgainst(t *testing.T, admittedExit, refusedExit int32) (bool, *fake.Clientset, error) {
	t.Helper()
	cfg := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12"}
	require.NoError(t, cfg.validate())
	cs := fakeClient()
	a := &Adapter{poll: 2 * time.Millisecond}
	a.use(cs, cfg)

	stop := make(chan struct{})
	go kubelet(t, cs, admittedExit, refusedExit, stop)
	defer close(stop)
	enforced, err := a.runCanary(context.Background())
	return enforced, cs, err
}

// TestR025_TheCanaryShowsWhetherPolicyIsEnforced asserts O-43's check: a
// client the policy refuses must fail to connect while one it admits
// connects; a refused client that connects means the cluster ignores policy;
// and an admitted client that cannot connect proves nothing either way. The
// canary's namespace is removed afterwards.
func TestR025_TheCanaryShowsWhetherPolicyIsEnforced(t *testing.T) {
	enforced, cs, err := runCanaryAgainst(t, 0, 1)
	require.NoError(t, err)
	require.True(t, enforced)
	left, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{LabelSelector: labelRole + "=" + roleCanary})
	require.NoError(t, err)
	require.Empty(t, left.Items, "the canary cleans up after itself")

	enforced, _, err = runCanaryAgainst(t, 0, 0)
	require.NoError(t, err)
	require.False(t, enforced, "the refused client connected: the policy was ignored")

	_, _, err = runCanaryAgainst(t, 1, 1)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err), "inconclusive is not a pass")
}

// TestR025_ACanaryResultIsReusedForAnHour asserts the canary runs once per
// hour when it passes, so health checks stay cheap, and is asked again when
// stale.
func TestR025_ACanaryResultIsReusedForAnHour(t *testing.T) {
	cfg := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12"}
	require.NoError(t, cfg.validate())
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	runs := 0
	a := &Adapter{
		probe: func(context.Context) (bool, error) { runs++; return true, nil },
		now:   func() time.Time { return now },
	}
	a.use(fakeClient(), cfg)

	require.False(t, a.canaryPassed(), "nothing is claimed before the check has run")
	require.NoError(t, a.HealthCheck(context.Background()))
	require.NoError(t, a.HealthCheck(context.Background()))
	require.Equal(t, 1, runs)
	require.True(t, a.canaryPassed())

	now = now.Add(canaryEvery + time.Minute)
	require.NoError(t, a.HealthCheck(context.Background()))
	require.Equal(t, 2, runs)
}

// TestO43_ReplicasRunTheirCanariesInNamespacesOfTheirOwn: two replicas
// checking at once each get a namespace of their own. When they shared
// pando-canary, one's cleanup removed the policy under the other's run, its
// refused client connected, and that replica took the cluster for one that
// does not enforce NetworkPolicy (seen on kind).
func TestO43_ReplicasRunTheirCanariesInNamespacesOfTheirOwn(t *testing.T) {
	cfg := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12"}
	require.NoError(t, cfg.validate())
	cs := fakeClient()
	var mu sync.Mutex
	created := map[string]bool{}
	cs.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ns := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace)
		mu.Lock()
		created[ns.Name] = true
		mu.Unlock()
		return false, nil, nil
	})
	stop := make(chan struct{})
	defer close(stop)
	go kubelet(t, cs, 0, 1, stop)

	results := make(chan error, 2)
	for range 2 {
		a := &Adapter{poll: 2 * time.Millisecond}
		a.use(cs, cfg)
		go func() {
			enforced, err := a.runCanary(context.Background())
			if err == nil && !enforced {
				err = errNotEnforced()
			}
			results <- err
		}()
	}
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.Len(t, created, 2, "each run made its own namespace")
	for name := range created {
		require.True(t, strings.HasPrefix(name, canaryNamespacePrefix), name)
	}
}

// TestO43_AStaleCanaryNamespaceIsSwept: a replica that stopped mid-check
// leaves its namespace; the next check removes it.
func TestO43_AStaleCanaryNamespaceIsSwept(t *testing.T) {
	stale := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: canaryNamespacePrefix + "old", Labels: map[string]string{labelRole: roleCanary},
		CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
	}}
	cfg := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12"}
	require.NoError(t, cfg.validate())
	cs := newFake(stale)
	a := &Adapter{poll: 2 * time.Millisecond}
	a.use(cs, cfg)
	stop := make(chan struct{})
	defer close(stop)
	go kubelet(t, cs, 0, 1, stop)

	_, err := a.runCanary(context.Background())
	require.NoError(t, err)
	_, err = cs.CoreV1().Namespaces().Get(context.Background(), stale.Name, metav1.GetOptions{})
	require.Error(t, err, "the stale canary namespace is removed")
}

// TestO43_AFailedCanaryIsCheckedAgainAndRecovers: a failed check makes the
// runtime unavailable — the planner refuses it rather than run apps
// unisolated — and is not latched: five minutes on it runs again, and a pass
// makes the runtime available.
func TestO43_AFailedCanaryIsCheckedAgainAndRecovers(t *testing.T) {
	cfg := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12"}
	require.NoError(t, cfg.validate())
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	enforced := false
	a := &Adapter{
		probe: func(context.Context) (bool, error) { return enforced, nil },
		now:   func() time.Time { return now },
	}
	a.use(fakeClient(), cfg)

	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(a.HealthCheck(context.Background())))
	enforced = true
	require.Error(t, a.HealthCheck(context.Background()), "within five minutes the failed answer stands")
	now = now.Add(canaryRetryFailed + time.Second)
	require.NoError(t, a.HealthCheck(context.Background()), "checked again, and passing")
	require.True(t, a.canaryPassed())
}
