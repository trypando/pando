package kubernetes

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/secret"
)

const testBundle = "app_01HQ8ABC"

var testNS = namespaceFor(testBundle)

// newFake is a fake cluster that plays the controller manager's part every
// pod depends on: each namespace, given or created, has its default
// ServiceAccount (awaitServiceAccount).
func newFake(objs ...runtime.Object) *fake.Clientset {
	cs := fake.NewClientset(objs...)
	account := func(ns string) {
		_ = cs.Tracker().Add(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns}})
	}
	for _, o := range objs {
		if ns, ok := o.(*corev1.Namespace); ok {
			account(ns.Name)
		}
	}
	cs.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if ns, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace); ok {
			account(ns.Name)
		}
		return false, nil, nil
	})
	return cs
}

// testAdapter is an adapter on a fake cluster whose canary has passed.
func testAdapter(t *testing.T, mutate func(*Config), objs ...runtime.Object) (*Adapter, *fake.Clientset) {
	t.Helper()
	cfg := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12", EgressGatewayImage: "ghcr.io/trypando/pando:test"}
	if mutate != nil {
		mutate(&cfg)
	}
	require.NoError(t, cfg.validate())
	cs := newFake(objs...)
	a := &Adapter{
		probe:        func(context.Context) (bool, error) { return true, nil },
		stream:       func(context.Context, string, string, string, []string, remotecommand.StreamOptions) error { return nil },
		poll:         5 * time.Millisecond,
		scheduleWait: -1,
	}
	a.use(cs, cfg)
	require.NoError(t, a.HealthCheck(context.Background()))
	return a, cs
}

func webPlan() api.BundlePlan {
	return api.BundlePlan{
		BundleID: testBundle,
		Network:  api.NetworkPlan{Private: true},
		Labels:   map[string]string{"pando.app": testBundle},
		Volumes:  []api.VolumePlan{{VolumeID: "vol_01DATA", Name: "data"}},
		Workloads: []api.WorkloadPlan{
			{
				Name: "web", Image: "ghcr.io/example/web@sha256:abc",
				Command:   []string{"serve"},
				Env:       map[string]secret.Value{"DATABASE_URL": secret.New("postgres://u:hunter2@db/app")},
				Ports:     []api.PortPlan{{Number: 3000, Protocol: "tcp"}},
				Health:    &api.HealthPlan{Path: "/healthz", Port: 3000, IntervalSeconds: 10, Retries: 3},
				Resources: api.ResourcePlan{CPUMillis: 500, MemoryBytes: 512 << 20},
				Files:     []api.FilePlan{{Path: "/etc/web/config.toml", Content: "x = 1\n", Mode: 0o600}},
				Exposed:   true, DependsOn: []string{"db"},
			},
			{
				Name: "db", Image: "postgres:17",
				Ports:     []api.PortPlan{{Number: 5432}},
				Mounts:    []api.MountPlan{{VolumeID: "vol_01DATA", Path: "/var/lib/postgresql/data"}},
				Resources: api.ResourcePlan{CPUMillis: 500, MemoryBytes: 512 << 20},
			},
		},
	}
}

func listPods(t *testing.T, cs *fake.Clientset, ns, workload string) []corev1.Pod {
	t.Helper()
	list, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: labelWorkload + "=" + workload})
	require.NoError(t, err)
	return list.Items
}

// setStatus gives a pod the status the kubelet would.
func setStatus(t *testing.T, cs *fake.Clientset, p corev1.Pod, status corev1.PodStatus) {
	t.Helper()
	p.Status = status
	_, err := cs.CoreV1().Pods(p.Namespace).UpdateStatus(context.Background(), &p, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func running(ready bool) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: appContainer, Ready: ready, ImageID: "ghcr.io/example/web@sha256:abc",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		}},
	}
}

func exited(code int32) corev1.PodStatus {
	phase := corev1.PodFailed
	if code == 0 {
		phase = corev1.PodSucceeded
	}
	return corev1.PodStatus{
		Phase: phase,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: appContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: code, StartedAt: metav1.Now(), FinishedAt: metav1.Now(),
			}},
		}},
	}
}
