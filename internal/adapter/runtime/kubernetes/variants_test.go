package kubernetes

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR026_ApplyRefusesWhatItCannotRunPrivately asserts the refusals Apply
// makes before touching the cluster: a plan without a private network (R-026
// is checked, not assumed) and carried files too large for a ConfigMap.
func TestR026_ApplyRefusesWhatItCannotRunPrivately(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)

	shared := webPlan()
	shared.Network.Private = false
	_, err := a.Apply(ctx, shared)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.ErrorContains(t, err, "shared network")

	big := webPlan()
	big.Workloads[0].Files = []api.FilePlan{{Path: "/a", Content: strings.Repeat("x", maxCarriedBytes+1)}}
	_, err = a.Apply(ctx, big)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.ErrorContains(t, err, `carried into "web"`)
	require.Contains(t, errs.As(err).Remedy, "Build the larger files into the image")

	_, err = cs.CoreV1().Namespaces().Get(ctx, testNS, metav1.GetOptions{})
	require.Error(t, err, "nothing was created")
}

// TestR023_PandoNeverTakesOverANamespaceItDidNotMake asserts that an app is
// never put in a namespace somebody else made, nor in one of Pando's that is
// still being removed, and that each refusal says what to do.
func TestR023_PandoNeverTakesOverANamespaceItDidNotMake(t *testing.T) {
	ctx := context.Background()
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS, Labels: map[string]string{"team": "data"}}}
	a, _ := testAdapter(t, nil, foreign)
	_, err := a.Apply(ctx, webPlan())
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.ErrorContains(t, err, "was not made by Pando")
	require.Contains(t, errs.As(err).Remedy, "Remove or rename that namespace")

	now := metav1.Now()
	leaving := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: testNS, Labels: map[string]string{labelManagedBy: managedBy},
		DeletionTimestamp: &now, Finalizers: []string{"kubernetes"},
	}}
	a, _ = testAdapter(t, nil, leaving)
	_, err = a.Apply(ctx, webPlan())
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.ErrorContains(t, err, "still being removed")
}

// TestR023_ThePodIsThePlanAndNothingMore asserts how plan details become the
// pod and its Service: UDP ports, read-only mounts, a file with no mode, each
// kind of health check as a readiness probe (never a liveness probe), no
// limits when the plan sets none, a pull credential keyed by the image's
// registry, and a quota that does not cap CPU when a workload is unlimited.
func TestR023_ThePodIsThePlanAndNothingMore(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, func(c *Config) { c.ServiceCIDR = "fd00:10:96::/112" })
	plan := api.BundlePlan{
		BundleID: testBundle, Network: api.NetworkPlan{Private: true},
		Volumes: []api.VolumePlan{{VolumeID: "vol_01DATA", Name: "data"}},
		Workloads: []api.WorkloadPlan{
			{
				Name: "dns", Image: "registry.example.com:5000/team/dns:1",
				Ports:    []api.PortPlan{{Number: 53, Protocol: "udp"}},
				Mounts:   []api.MountPlan{{VolumeID: "vol_01DATA", Path: "/zones", ReadOnly: true}},
				Files:    []api.FilePlan{{Path: "/etc/dns.conf", Content: "a"}},
				Health:   &api.HealthPlan{Command: []string{"dig", "@127.0.0.1"}, TimeoutSeconds: 2},
				PullAuth: &api.RegistryAuth{IdentityToken: secret.New("idtok")},
			},
			{Name: "tcp", Image: "library/redis", Health: &api.HealthPlan{Port: 6379}, PullAuth: &api.RegistryAuth{Username: "u", Password: secret.New("p")}},
			{Name: "local", Image: "localhost/app", Health: &api.HealthPlan{}, PullAuth: &api.RegistryAuth{Username: "v", Password: secret.New("q")}},
		},
	}
	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	dns := listPods(t, cs, testNS, "dns")[0]
	c := dns.Spec.Containers[0]
	require.Equal(t, corev1.ProtocolUDP, c.Ports[0].Protocol)
	require.Equal(t, []string{"dig", "@127.0.0.1"}, c.ReadinessProbe.Exec.Command)
	require.EqualValues(t, 2, c.ReadinessProbe.TimeoutSeconds)
	require.Nil(t, c.LivenessProbe, "the runtime never remediates")
	require.Empty(t, c.Resources.Limits, "no limits when the plan sets none")
	require.True(t, dns.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly)
	require.EqualValues(t, 0o644, *dns.Spec.Volumes[1].ConfigMap.Items[0].Mode)
	require.Equal(t, []corev1.LocalObjectReference{{Name: pullSecretName}}, dns.Spec.ImagePullSecrets)

	tcp := listPods(t, cs, testNS, "tcp")[0]
	require.EqualValues(t, 6379, tcp.Spec.Containers[0].ReadinessProbe.TCPSocket.Port.IntValue())
	require.Nil(t, listPods(t, cs, testNS, "local")[0].Spec.Containers[0].ReadinessProbe, "a health check with nothing to check is no probe")

	svc, err := cs.CoreV1().Services(testNS).Get(ctx, "dns", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, corev1.ProtocolUDP, svc.Spec.Ports[0].Protocol)
	require.Equal(t, "udp-53", svc.Spec.Ports[0].Name)

	pull, err := cs.CoreV1().Secrets(testNS).Get(ctx, pullSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	var cfg struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	require.NoError(t, json.Unmarshal(pull.Data[corev1.DockerConfigJsonKey], &cfg))
	require.Equal(t, map[string]string{"identitytoken": "idtok"}, cfg.Auths["registry.example.com:5000"])
	require.Equal(t, "u", cfg.Auths["https://index.docker.io/v1/"]["username"], "an image with no registry host is Docker Hub's")
	require.Equal(t, "v", cfg.Auths["localhost"]["username"])

	quota, err := cs.CoreV1().ResourceQuotas(testNS).Get(ctx, quotaName, metav1.GetOptions{})
	require.NoError(t, err)
	_, capped := quota.Spec.Hard[corev1.ResourceLimitsCPU]
	require.False(t, capped, "a quota on CPU would refuse a pod that states none")

	def, err := cs.NetworkingV1().NetworkPolicies(testNS).Get(ctx, defaultPolicy, metav1.GetOptions{})
	require.NoError(t, err)
	var v6Except []string
	for _, rule := range def.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "::/0" {
				v6Except = peer.IPBlock.Except
			}
		}
	}
	require.Equal(t, []string{"fd00:10:96::/112"}, v6Except, "an IPv6 Service range is excluded from the IPv6 route out")
}

// TestR026_AServiceChangedByHandIsPutBack asserts that a workload's Service
// made reachable from outside the cluster by hand is replaced with the
// headless one, and that one with other ports is corrected in place.
func TestR026_AServiceChangedByHandIsPutBack(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)

	web, err := cs.CoreV1().Services(testNS).Get(ctx, "web", metav1.GetOptions{})
	require.NoError(t, err)
	web.Spec.Type, web.Spec.ClusterIP = corev1.ServiceTypeNodePort, "10.96.0.9"
	_, err = cs.CoreV1().Services(testNS).Update(ctx, web, metav1.UpdateOptions{})
	require.NoError(t, err)
	db, err := cs.CoreV1().Services(testNS).Get(ctx, "db", metav1.GetOptions{})
	require.NoError(t, err)
	db.Spec.Ports[0].Port = 9999
	db.Spec.PublishNotReadyAddresses = false
	_, err = cs.CoreV1().Services(testNS).Update(ctx, db, metav1.UpdateOptions{})
	require.NoError(t, err)

	_, err = a.Apply(ctx, webPlan())
	require.NoError(t, err)
	web, err = cs.CoreV1().Services(testNS).Get(ctx, "web", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, corev1.ServiceTypeClusterIP, web.Spec.Type)
	require.Equal(t, corev1.ClusterIPNone, web.Spec.ClusterIP)
	db, err = cs.CoreV1().Services(testNS).Get(ctx, "db", metav1.GetOptions{})
	require.NoError(t, err)
	require.EqualValues(t, 5432, db.Spec.Ports[0].Port)
	require.True(t, db.Spec.PublishNotReadyAddresses)
}

// scheduler gives every new app pod in testNS the outcome decide returns.
func scheduler(t *testing.T, a *Adapter, decide func(p *corev1.Pod)) {
	t.Helper()
	cs := a.cs
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		ctx := context.Background()
		seen := map[string]bool{}
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			list, err := cs.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			for _, p := range list.Items {
				if seen[p.Name] {
					continue
				}
				seen[p.Name] = true
				decide(&p)
				_, _ = cs.CoreV1().Pods(testNS).Update(ctx, &p, metav1.UpdateOptions{})
				_, _ = cs.CoreV1().Pods(testNS).UpdateStatus(ctx, &p, metav1.UpdateOptions{})
			}
		}
	}()
}

// TestR242_APodNoMachineHasRoomForIsAnErrorThatSaysSo asserts that a pod the
// scheduler cannot place fails the deploy with what it asked for and what
// the scheduler said, rather than leaving it pending.
func TestR242_APodNoMachineHasRoomForIsAnErrorThatSaysSo(t *testing.T) {
	ctx := context.Background()
	a, _ := testAdapter(t, nil)
	a.scheduleWait = 5 * time.Second
	scheduler(t, a, func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/3 nodes are available: 3 Insufficient memory.",
		}}
	})
	_, err := a.Apply(ctx, webPlan())
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	require.ErrorContains(t, err, `No machine in the cluster has room for "db". It asks for 500 thousandths of a CPU and 512 MiB of memory.`)
	require.ErrorContains(t, err, "3 Insufficient memory")
	require.Contains(t, errs.As(err).Remedy, `Lower what "db" asks for`)

	require.NotContains(t, unschedulable(api.WorkloadPlan{Name: "x"}, "full").Error(), "asks for", "nothing asked, nothing named")
}

// TestR242_APlacedPodIsNotWaitedOn asserts that a pod the scheduler places
// ends the wait at once.
func TestR242_APlacedPodIsNotWaitedOn(t *testing.T) {
	ctx := context.Background()
	a, _ := testAdapter(t, nil)
	a.scheduleWait = time.Minute
	scheduler(t, a, func(p *corev1.Pod) { p.Spec.NodeName = "node-a" })
	start := time.Now()
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)
	require.Less(t, time.Since(start), 10*time.Second)
}

// TestR096_AWorkloadWaitsForItsDependencyToBeReady asserts R-096: a workload
// is started only once a dependency with a health check reports ready, and a
// dependency that never does holds it back no longer than the wait allows.
func TestR096_AWorkloadWaitsForItsDependencyToBeReady(t *testing.T) {
	ctx := context.Background()
	plan := webPlan()
	plan.Workloads[1].Health = &api.HealthPlan{Port: 5432}

	a, cs := testAdapter(t, nil)
	a.dependencyWait = time.Minute
	var readyAt atomic.Int64
	scheduler(t, a, func(p *corev1.Pod) {
		if p.Labels[labelWorkload] != "db" {
			return
		}
		time.Sleep(30 * time.Millisecond)
		readyAt.Store(time.Now().UnixNano())
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)
	web := listPods(t, cs, testNS, "web")[0]
	created, err := time.Parse(time.RFC3339Nano, web.Annotations[annoCreated])
	require.NoError(t, err)
	require.NotZero(t, readyAt.Load())
	require.False(t, created.Before(time.Unix(0, readyAt.Load())), "web started after db was ready")

	// A dependency that stays unready: web still starts, after the wait.
	a, cs = testAdapter(t, nil)
	a.dependencyWait = 50 * time.Millisecond
	scheduler(t, a, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	})
	_, err = a.Apply(ctx, plan)
	require.NoError(t, err)
	require.Len(t, listPods(t, cs, testNS, "web"), 1)

	// A canceled deploy stops waiting.
	a, _ = testAdapter(t, nil)
	a.dependencyWait = time.Hour
	scheduler(t, a, func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning })
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, _ = a.Apply(cctx, plan)
	require.Error(t, cctx.Err())
}

// TestR148_TheNewestPodIsTheOneNotBeingDeleted asserts how a workload's
// current pod is chosen among several: one being deleted loses to one that
// is not, and a pod without Pando's creation annotation falls back to the
// API's timestamp.
func TestR148_TheNewestPodIsTheOneNotBeingDeleted(t *testing.T) {
	now := time.Now()
	deleting := metav1.NewTime(now)
	pod := func(name string, created time.Time, annotated bool, del *metav1.Time) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created), DeletionTimestamp: del}}
		if annotated {
			p.Annotations = map[string]string{annoCreated: created.Format(time.RFC3339Nano)}
		}
		return p
	}
	require.Nil(t, newest(nil))
	require.Equal(t, "old", newest([]corev1.Pod{
		pod("new-deleting", now, true, &deleting), pod("old", now.Add(-time.Hour), true, nil),
	}).Name)
	require.Equal(t, "old", newest([]corev1.Pod{
		pod("old", now.Add(-time.Hour), true, nil), pod("new-deleting", now, true, &deleting),
	}).Name)
	require.Equal(t, "b", newest([]corev1.Pod{
		pod("a", now.Add(-time.Hour), false, nil), pod("b", now, false, nil),
	}).Name, "the API's timestamp when Pando's is missing")
}

func node(name, os, arch string, ready bool, taints ...corev1.Taint) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"pool": "apps"}},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			NodeInfo:    corev1.NodeSystemInfo{OperatingSystem: os, Architecture: arch},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
		},
	}
}

// TestR254_ThePlatformIsReportedOnlyWhenEveryEligibleNodeAgrees asserts the
// platform in the capabilities: the eligible nodes' os/arch when they agree,
// and nothing when they differ, report none, or there are none — unready and
// tainted nodes do not count.
func TestR254_ThePlatformIsReportedOnlyWhenEveryEligibleNodeAgrees(t *testing.T) {
	tests := []struct {
		name  string
		nodes []runtime.Object
		want  string
	}{
		{"agree", []runtime.Object{node("a", "linux", "arm64", true), node("b", "linux", "arm64", true), node("c", "linux", "amd64", false)}, "linux/arm64"},
		{"tainted nodes do not count", []runtime.Object{node("a", "linux", "arm64", true), node("b", "linux", "amd64", true, corev1.Taint{Key: "gpu", Effect: corev1.TaintEffectNoSchedule})}, "linux/arm64"},
		{"differ", []runtime.Object{node("a", "linux", "arm64", true), node("b", "linux", "amd64", true)}, ""},
		{"unreported", []runtime.Object{node("a", "", "", true)}, ""},
		{"none", nil, ""},
		{"no ready condition", []runtime.Object{&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "x"}}}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := testAdapter(t, nil, tc.nodes...)
			caps, err := a.Capabilities(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.want, caps.Platform)
		})
	}

	ctx := context.Background()

	a, cs := testAdapter(t, func(c *Config) { c.NodeSelector = "pool=apps" }, node("a", "linux", "arm64", true))
	failOn(cs, "list", "nodes")
	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.Empty(t, caps.Platform, "unreadable nodes report no platform")
	failOn(cs, "list", "namespaces")
	_, err = a.Capacity(ctx)
	require.ErrorIs(t, err, errAPI)

	require.Empty(t, New().platform(ctx), "an unconfigured adapter has no platform")
}

// TestR243_CapacityNamesTheNodeSelector asserts that capacity limited by a
// node selector says so.
func TestR243_CapacityNamesTheNodeSelector(t *testing.T) {
	a, _ := testAdapter(t, func(c *Config) { c.NodeSelector = "pool=apps" }, node("a", "linux", "arm64", true))
	got, err := a.Capacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, "pool=apps", got.Details["node_selector"])
	require.Equal(t, 2000, got.TotalCPUMillis)
}

// TestR114_IsolationComesFromTheRuntimeClassHandler asserts the handlers that
// raise the isolation class and that an unreadable RuntimeClass is an
// unavailable adapter.
func TestR114_IsolationComesFromTheRuntimeClassHandler(t *testing.T) {
	require.Equal(t, spec.IsolationSandboxed, isolationOf("runsc"))
	require.Equal(t, spec.IsolationSandboxed, isolationOf("kata-qemu"))
	require.Equal(t, spec.IsolationContainer, isolationOf("runc"))

	kata := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata"}, Handler: "kata-fc"}
	a, cs := testAdapter(t, func(c *Config) { c.RuntimeClass = "kata" }, kata)
	failOn(cs, "get", "runtimeclasses")
	err := a.HealthCheck(context.Background())
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.ErrorIs(t, err, errAPI)
}

// TestR254_HealthCheckReportsAnUnusableCluster asserts each way the cluster
// is reported unusable: never configured, an API server that does not
// answer, and a canary that could not be run.
func TestR254_HealthCheckReportsAnUnusableCluster(t *testing.T) {
	ctx := context.Background()
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(New().HealthCheck(ctx)))

	a, cs := testAdapter(t, nil)
	cs.PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errAPI
	})
	err := a.HealthCheck(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.ErrorContains(t, err, "not responding")
}

// TestR254_ConfigurationThatCannotWorkIsRefused asserts the settings refused
// beyond those TestConfigRefusesWhatCannotWork covers.
func TestR254_ConfigurationThatCannotWorkIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"api server range": func(c *Config) { c.APIServerCIDR = "not-a-range" },
		"proxy port":       func(c *Config) { c.ProxyPort = 70000 },
	} {
		cfg := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12"}
		mutate(&cfg)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(cfg.validate()), name)
	}
	sel, err := parseSelector(" pool = apps , ,zone=b")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"pool": "apps", "zone": "b"}, sel)

	_, err = New().ImportImage(context.Background(), strings.NewReader(""))
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "image registry adapter")

	err = checkWorkloadName("___")
	require.Contains(t, errs.As(err).Remedy, `Rename it to "web"`, "a name with nothing usable gets a usable suggestion")
}
