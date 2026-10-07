package kubernetes

import (
	"context"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/egress"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

var errAPI = errors.New("the API server is unavailable")

// failOn makes verb on resource fail, as an API server that is down or
// refuses Pando would. The newest reactor is first in the chain.
func failOn(cs *fake.Clientset, verb, resource string) {
	cs.PrependReactor(verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errAPI
	})
}

// faultCase is one adapter operation, run against a cluster in which one verb
// on one kind of object fails.
type faultCase struct {
	name string
	// prepare brings the cluster to where the operation starts, with no fault.
	prepare func(t *testing.T, a *Adapter, cs *fake.Clientset)
	op      func(a *Adapter) error
	// mustSurface are faults ("verb resource") the operation cannot succeed
	// past; the matrix asserts these at least are reported.
	mustSurface []string
}

func restrictedPlan() api.BundlePlan {
	p := webPlan()
	p.Network.Egress = egress.Rules{BlockPrivate: true}
	return p
}

func changedPlan() api.BundlePlan {
	p := restrictedPlan()
	p.Workloads[0].Files[0].Content = "x = 2\n"
	p.Workloads[0].Resources.CPUMillis = 900
	p.Workloads[0].Env = map[string]secret.Value{"DATABASE_URL": secret.New("postgres://u:rotated@db/app")}
	return p
}

func edgePlan() api.EdgePlan {
	return api.EdgePlan{
		Name: "rte_traefik", Image: "traefik:v3.2", Args: []string{"--providers.kubernetescrd"},
		Ports:           []api.EdgePort{{Host: 80, Container: 80}, {Host: 443, Container: 443}},
		ProxyAlias:      "pando-proxy",
		ReadsRoutesFrom: api.EdgeConfigKubernetesAPI,
		Certificates:    []api.EdgeCertificate{{Name: "pando-tls-a.example.com", CertPEM: []byte("CERT"), KeyPEM: secret.New("KEY")}},
	}
}

func applied(plan api.BundlePlan) func(*testing.T, *Adapter, *fake.Clientset) {
	return func(t *testing.T, a *Adapter, _ *fake.Clientset) {
		t.Helper()
		_, err := a.Apply(context.Background(), plan)
		require.NoError(t, err)
	}
}

// boundAndApplied applies plan, then binds its claim to a volume whose
// reclaim policy is policy, as a provisioner would.
func boundAndApplied(plan api.BundlePlan, policy corev1.PersistentVolumeReclaimPolicy) func(*testing.T, *Adapter, *fake.Clientset) {
	return func(t *testing.T, a *Adapter, cs *fake.Clientset) {
		t.Helper()
		ctx := context.Background()
		applied(plan)(t, a, cs)
		_, err := cs.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-data"},
			Spec:       corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: policy},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		pvc, err := cs.CoreV1().PersistentVolumeClaims(testNS).Get(ctx, pvcName("vol_01DATA"), metav1.GetOptions{})
		require.NoError(t, err)
		pvc.Spec.VolumeName = "pv-data"
		_, err = cs.CoreV1().PersistentVolumeClaims(testNS).Update(ctx, pvc, metav1.UpdateOptions{})
		require.NoError(t, err)
	}
}

func edgeApplied(t *testing.T, a *Adapter, _ *fake.Clientset) {
	t.Helper()
	require.NoError(t, a.ApplyEdge(context.Background(), edgePlan()))
}

// TestR105_EveryClusterAPIFailureIsReportedWithItsCause asserts, for each of
// the adapter's operations and each verb on each kind of object it touches,
// that a failing API call is either not one the operation depends on, or is
// reported as an adapter error carrying the API's own error — never a panic,
// never an error with the cause dropped — and that once the API recovers the
// same call succeeds, so the reconciler's next pass converges (R-144).
func TestR105_EveryClusterAPIFailureIsReportedWithItsCause(t *testing.T) {
	ctx := context.Background()
	ref := api.BundleRef{BundleID: testBundle}
	vol := api.VolumeHandle{VolumeID: "vol_01DATA", Handle: testNS + volumeHandleSep + pvcName("vol_01DATA")}
	cases := []faultCase{
		{
			name: "first apply", op: func(a *Adapter) error { _, err := a.Apply(ctx, webPlan()); return err },
			mustSurface: []string{"get namespaces", "create namespaces", "create rolebindings", "create networkpolicies", "create resourcequotas", "create secrets", "create services", "create pods", "list pods", "create persistentvolumeclaims"},
		},
		{
			name: "first restricted apply", op: func(a *Adapter) error { _, err := a.Apply(ctx, restrictedPlan()); return err },
			mustSurface: []string{"create pods", "create networkpolicies"},
		},
		{
			name: "changed apply", prepare: applied(webPlan()),
			op:          func(a *Adapter) error { _, err := a.Apply(ctx, changedPlan()); return err },
			mustSurface: []string{"update secrets", "update configmaps", "update resourcequotas", "delete pods", "create pods"},
		},
		{
			name: "apply binding a new volume", prepare: boundAndApplied(webPlan(), corev1.PersistentVolumeReclaimDelete),
			op:          func(a *Adapter) error { _, err := a.Apply(ctx, webPlan()); return err },
			mustSurface: []string{"get persistentvolumes", "patch persistentvolumes"},
		},
		{
			name: "unrestricting apply", prepare: applied(restrictedPlan()),
			op:          func(a *Adapter) error { _, err := a.Apply(ctx, webPlan()); return err },
			mustSurface: []string{"delete pods", "delete networkpolicies"},
		},
		{
			name: "observe", prepare: applied(webPlan()),
			op:          func(a *Adapter) error { _, err := a.Observe(ctx, ref); return err },
			mustSurface: []string{"list pods"},
		},
		{
			name: "stop", prepare: applied(restrictedPlan()),
			op:          func(a *Adapter) error { return a.Stop(ctx, ref) },
			mustSurface: []string{"list pods", "delete pods"},
		},
		{
			name: "destroy keeping volumes", prepare: applied(restrictedPlan()),
			op:          func(a *Adapter) error { return a.Destroy(ctx, ref, api.DestroyOptions{KeepVolumes: true}) },
			mustSurface: []string{"list pods", "delete pods", "list services", "delete services", "list secrets", "delete secrets", "list configmaps", "delete configmaps"},
		},
		{
			name: "destroy with volumes", prepare: applied(webPlan()),
			op:          func(a *Adapter) error { return a.Destroy(ctx, ref, api.DestroyOptions{KeepVolumes: false}) },
			mustSurface: []string{"delete namespaces"},
		},
		{
			name: "create volume",
			op: func(a *Adapter) error {
				_, err := a.CreateVolume(ctx, api.VolumeRequest{VolumeID: "vol_01DATA", BundleID: testBundle, Name: "data"})
				return err
			},
			mustSurface: []string{"create persistentvolumeclaims"},
		},
		{
			name: "destroy volume", prepare: boundAndApplied(webPlan(), corev1.PersistentVolumeReclaimRetain),
			op:          func(a *Adapter) error { return a.DestroyVolume(ctx, vol) },
			mustSurface: []string{"get persistentvolumeclaims", "patch persistentvolumes", "delete persistentvolumeclaims"},
		},
		{
			name: "logs", prepare: applied(webPlan()),
			op: func(a *Adapter) error {
				rc, err := a.Logs(ctx, api.WorkloadRef{BundleID: testBundle, Workload: "web"}, api.LogOptions{})
				if err == nil {
					_, _ = io.Copy(io.Discard, rc)
					_ = rc.Close()
				}
				return err
			},
			mustSurface: []string{"list pods"},
		},
		{
			name: "first edge", op: func(a *Adapter) error { return a.ApplyEdge(ctx, edgePlan()) },
			mustSurface: []string{"create deployments", "create services", "create secrets", "create poddisruptionbudgets", "create networkpolicies"},
		},
		{
			name: "changed edge", prepare: edgeApplied,
			op: func(a *Adapter) error {
				p := edgePlan()
				p.Image = "traefik:v3.3"
				p.Ports = p.Ports[:1]
				p.Certificates = nil
				return a.ApplyEdge(ctx, p)
			},
			mustSurface: []string{"update deployments", "update services", "list secrets", "delete secrets"},
		},
		{
			name: "observe edge", prepare: edgeApplied,
			op:          func(a *Adapter) error { _, err := a.ObserveEdge(ctx, "rte_traefik"); return err },
			mustSurface: []string{"get deployments"},
		},
		{
			name: "remove edge", prepare: edgeApplied,
			op:          func(a *Adapter) error { return a.RemoveEdge(ctx, "rte_traefik") },
			mustSurface: []string{"delete deployments"},
		},
		{
			name: "edges", prepare: edgeApplied,
			op:          func(a *Adapter) error { _, err := a.Edges(ctx); return err },
			mustSurface: []string{"list deployments"},
		},
		{
			name: "trial",
			op: func(a *Adapter) error {
				_, err := a.Trial(ctx, api.TrialRequest{
					TrialID: "trl_1", Image: "ghcr.io/example/app", Timeout: 20 * time.Millisecond,
					PullAuth: &api.RegistryAuth{Registry: "ghcr.io", Username: "bot", Password: secret.New("pat")},
				})
				return err
			},
			mustSurface: []string{"create namespaces", "create rolebindings", "create networkpolicies", "create secrets", "create pods"},
		},
		{
			name: "capacity", prepare: applied(webPlan()),
			op:          func(a *Adapter) error { _, err := a.Capacity(ctx); return err },
			mustSurface: []string{"list nodes"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fresh := func() (*Adapter, *fake.Clientset) {
				a, cs := testAdapter(t, nil)
				if c.prepare != nil {
					c.prepare(t, a, cs)
				}
				return a, cs
			}

			// The calls the operation makes when nothing fails: each is a
			// fault to inject.
			a, cs := fresh()
			var calls []string
			cs.PrependReactor("*", "*", func(act k8stesting.Action) (bool, runtime.Object, error) {
				call := act.GetVerb() + " " + act.GetResource().Resource
				if !slices.Contains(calls, call) {
					calls = append(calls, call)
				}
				return false, nil, nil
			})
			require.NoError(t, c.op(a))
			sort.Strings(calls)

			var surfaced []string
			for _, call := range calls {
				verb, resource, _ := strings.Cut(call, " ")
				a, cs := fresh()
				failOn(cs, verb, resource)
				err := c.op(a)
				if err == nil {
					continue // best effort: the operation does not depend on it
				}
				surfaced = append(surfaced, call)
				require.ErrorIsf(t, err, errAPI, "%s: the API's error is kept as the cause", call)
				code := errs.CodeOf(err)
				require.Truef(t, code == errs.AdapterFailed || code == errs.AdapterUnavailable,
					"%s: reported as %s, not an adapter error", call, code)
				require.NotEmpty(t, errs.As(err).Message)

				cs.ReactionChain = cs.ReactionChain[1:]
				require.NoErrorf(t, c.op(a), "%s: once the API recovers the same call succeeds", call)
			}
			for _, want := range c.mustSurface {
				require.Containsf(t, surfaced, want, "a failure to %s is reported", want)
			}
			t.Logf("calls: %s; surfaced: %s", strings.Join(calls, ", "), strings.Join(surfaced, ", "))
		})
	}
}
