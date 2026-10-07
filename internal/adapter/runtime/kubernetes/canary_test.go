package kubernetes

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/trypando/pando/internal/errs"
)

func fakeClient() *fake.Clientset { return fake.NewClientset() }

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
		pods, err := cs.CoreV1().Pods(canaryNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, p := range pods.Items {
			if done[p.Name] {
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
			if _, err := cs.CoreV1().Pods(canaryNamespace).UpdateStatus(ctx, &p, metav1.UpdateOptions{}); err == nil {
				done[p.Name] = true
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
	_, getErr := cs.CoreV1().Namespaces().Get(context.Background(), canaryNamespace, metav1.GetOptions{})
	require.Error(t, getErr, "the canary cleans up after itself")

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
