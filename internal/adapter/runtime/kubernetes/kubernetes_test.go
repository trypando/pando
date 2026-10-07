package kubernetes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/egress"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR026_AnAppGetsNoRouteInFromOutsideTheCluster asserts R-026: an app's
// namespace holds no NodePort or LoadBalancer Service, no pod with a host port
// or the host's network, no ServiceAccount token, and a quota that refuses
// either kind of Service if something tried.
func TestR026_AnAppGetsNoRouteInFromOutsideTheCluster(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)

	services, err := cs.CoreV1().Services(testNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, services.Items, 2)
	for _, s := range services.Items {
		require.Equal(t, corev1.ServiceTypeClusterIP, s.Spec.Type, s.Name)
		require.Equal(t, corev1.ClusterIPNone, s.Spec.ClusterIP, s.Name)
		require.True(t, s.Spec.PublishNotReadyAddresses, "whether to send traffic to an unhealthy app is Pando's decision")
	}

	pods, err := cs.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 2)
	for _, p := range pods.Items {
		require.False(t, p.Spec.HostNetwork, p.Name)
		require.False(t, *p.Spec.AutomountServiceAccountToken, p.Name)
		require.False(t, *p.Spec.EnableServiceLinks, p.Name)
		for _, c := range p.Spec.Containers {
			for _, port := range c.Ports {
				require.Zero(t, port.HostPort, p.Name)
			}
		}
	}

	ns, err := cs.CoreV1().Namespaces().Get(ctx, testNS, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "baseline", ns.Labels[labelPSAEnforce])
	require.Equal(t, managedBy, ns.Labels[labelManagedBy])

	q, err := cs.CoreV1().ResourceQuotas(testNS).Get(ctx, quotaName, metav1.GetOptions{})
	require.NoError(t, err)
	require.True(t, q.Spec.Hard.Name(corev1.ResourceServicesNodePorts, resource.DecimalSI).IsZero())
	require.True(t, q.Spec.Hard.Name(corev1.ResourceServicesLoadBalancers, resource.DecimalSI).IsZero())
}

// TestR023_AnAppNamespaceAdmitsOnlyPandosServerPods asserts R-023 and R-025:
// connections into an app come from its own pods, or from a pod that is both
// in Pando's namespace and carries Pando's server labels — one peer, so both
// must match — and from nowhere else.
func TestR023_AnAppNamespaceAdmitsOnlyPandosServerPods(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)

	np, err := cs.NetworkingV1().NetworkPolicies(testNS).Get(ctx, defaultPolicy, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, np.Spec.PodSelector.MatchLabels, "every pod in the namespace")
	require.ElementsMatch(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
	require.Len(t, np.Spec.Ingress, 2)

	own := np.Spec.Ingress[0].From
	require.Len(t, own, 1)
	require.NotNil(t, own[0].PodSelector)
	require.Nil(t, own[0].NamespaceSelector, "a bare pod selector is this namespace only")

	pando := np.Spec.Ingress[1].From
	require.Len(t, pando, 1, "namespace and pod selector in one peer, so both must match")
	require.Equal(t, map[string]string{"kubernetes.io/metadata.name": "pando"}, pando[0].NamespaceSelector.MatchLabels)
	require.Equal(t, map[string]string{labelName: "pando", labelComponent: componentSrv}, pando[0].PodSelector.MatchLabels)
	require.Nil(t, pando[0].IPBlock)

	// Out of the cluster, but never into it: another app is a pod address.
	outside := np.Spec.Egress[len(np.Spec.Egress)-1].To
	require.Equal(t, "0.0.0.0/0", outside[0].IPBlock.CIDR)
	require.ElementsMatch(t, []string{"10.244.0.0/16", "10.96.0.0/12"}, outside[0].IPBlock.Except)
}

// TestR023_UpstreamIsTheWorkloadsNameInsideTheCluster asserts the proxy is
// sent to the workload's Service by its cluster DNS name, computed without
// asking the API, and that a name no Service can have is refused.
func TestR023_UpstreamIsTheWorkloadsNameInsideTheCluster(t *testing.T) {
	a, cs := testAdapter(t, nil)
	before := len(cs.Actions())
	up, err := a.Upstream(context.Background(), api.WorkloadRef{BundleID: testBundle, Workload: "web"}, 3000)
	require.NoError(t, err)
	require.Equal(t, "http://web.pando-app-01hq8abc.svc.cluster.local:3000", up.URL)
	require.Len(t, cs.Actions(), before, "computed, not looked up: it runs on every request")

	_, err = a.Upstream(context.Background(), api.WorkloadRef{BundleID: testBundle, Workload: "Web_1"}, 3000)
	require.Error(t, err)
}

// TestR151_AWorkloadIsABarePodKubernetesNeverRestarts asserts R-151's
// mechanism on this runtime: no controller owns the pod and its restart
// policy is Never, so a pod that exits stays exited until Pando decides
// otherwise; health is a readiness probe only, never one that kills.
func TestR151_AWorkloadIsABarePodKubernetesNeverRestarts(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)

	pods := listPods(t, cs, testNS, "web")
	require.Len(t, pods, 1)
	p := pods[0]
	require.Equal(t, corev1.RestartPolicyNever, p.Spec.RestartPolicy)
	require.Empty(t, p.OwnerReferences, "owned by no Deployment, ReplicaSet or StatefulSet")
	c := p.Spec.Containers[0]
	require.Nil(t, c.LivenessProbe, "a liveness probe is the runtime remediating")
	require.Equal(t, "/healthz", c.ReadinessProbe.HTTPGet.Path)
	require.Equal(t, c.Resources.Limits, c.Resources.Requests, "requests equal limits")

	setStatus(t, cs, p, exited(137))
	observed, err := a.Observe(ctx, api.BundleRef{BundleID: testBundle})
	require.NoError(t, err)
	web := find(t, observed, "web")
	require.True(t, web.Present)
	require.False(t, web.Running)
	require.Equal(t, 137, *web.ExitCode)
	require.Zero(t, web.RestartCount)
	require.False(t, web.Restarting)
	require.Len(t, listPods(t, cs, testNS, "web"), 1, "Observe never remediates")
}

// TestR148_ApplyReplacesAnExitedPodAndKeepsItsLog asserts R-148 on this
// runtime: the reconciler's Apply recreates an exited workload under a new
// name, and the exited pod — whose log says why — is kept until its
// replacement runs.
func TestR148_ApplyReplacesAnExitedPodAndKeepsItsLog(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)
	first := listPods(t, cs, testNS, "web")[0]
	setStatus(t, cs, first, exited(1))

	_, err = a.Apply(ctx, webPlan())
	require.NoError(t, err)
	pods := listPods(t, cs, testNS, "web")
	require.Len(t, pods, 2, "the exited pod is kept beside its replacement")

	rc, err := a.Logs(ctx, api.WorkloadRef{BundleID: testBundle, Workload: "web"}, api.LogOptions{Tail: 10})
	require.NoError(t, err)
	_ = rc.Close()

	replacement := newest(pods)
	require.NotEqual(t, first.Name, replacement.Name)
	setStatus(t, cs, *replacement, running(true))
	_, err = a.Apply(ctx, webPlan())
	require.NoError(t, err)
	pods = listPods(t, cs, testNS, "web")
	require.Len(t, pods, 1, "once the replacement runs, the exited pod goes")
	require.Equal(t, replacement.Name, pods[0].Name)
}

// TestR144_ApplyIsIdempotentAndAChangedPlanReplacesThePod asserts Apply
// converges: the same plan twice leaves the running pod alone, and a changed
// plan replaces it.
func TestR144_ApplyIsIdempotentAndAChangedPlanReplacesThePod(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)
	first := listPods(t, cs, testNS, "web")[0]
	setStatus(t, cs, first, running(true))

	_, err = a.Apply(ctx, webPlan())
	require.NoError(t, err)
	pods := listPods(t, cs, testNS, "web")
	require.Len(t, pods, 1)
	require.Equal(t, first.Name, pods[0].Name)

	changed := webPlan()
	changed.Workloads[0].Image = "ghcr.io/example/web@sha256:def"
	_, err = a.Apply(ctx, changed)
	require.NoError(t, err)
	pods = listPods(t, cs, testNS, "web")
	require.Len(t, pods, 1)
	require.NotEqual(t, first.Name, pods[0].Name)
	require.Equal(t, "ghcr.io/example/web@sha256:def", pods[0].Spec.Containers[0].Image)
}

// TestR221_HealthIsReportedOnlyWhereThereIsAProbe asserts R-221's
// distinction: no probe is no signal, not unhealthy.
func TestR221_HealthIsReportedOnlyWhereThereIsAProbe(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)
	setStatus(t, cs, listPods(t, cs, testNS, "web")[0], running(false))
	setStatus(t, cs, listPods(t, cs, testNS, "db")[0], running(false))

	observed, err := a.Observe(ctx, api.BundleRef{BundleID: testBundle})
	require.NoError(t, err)
	web, db := find(t, observed, "web"), find(t, observed, "db")
	require.True(t, web.Running)
	require.NotNil(t, web.Healthy)
	require.False(t, *web.Healthy)
	require.True(t, db.Running)
	require.Nil(t, db.Healthy, "no probe, no signal")
	require.Equal(t, "ghcr.io/example/web@sha256:abc", web.ImageDigest)
}

// TestR194_AWorkloadsEnvironmentIsInASecretNotThePodSpec asserts the
// environment reaches the workload through a Secret: the pod spec, which more
// can read, carries no value, and the digest that marks a changed plan is a
// hash.
func TestR194_AWorkloadsEnvironmentIsInASecretNotThePodSpec(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)

	p := listPods(t, cs, testNS, "web")[0]
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "hunter2")
	require.Equal(t, envSecretName("web"), p.Spec.Containers[0].EnvFrom[0].SecretRef.Name)

	s, err := cs.CoreV1().Secrets(testNS).Get(ctx, envSecretName("web"), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "postgres://u:hunter2@db/app", string(s.Data["DATABASE_URL"]))
}

// TestR194_ThePullCredentialIsANamespaceSecretRewrittenEachDeploy asserts
// O-41: a private image's credential is the namespace's pull Secret, written
// at every Apply so a short-lived password is renewed, and gone when nothing
// needs it. It never reaches the workload's environment.
func TestR194_ThePullCredentialIsANamespaceSecretRewrittenEachDeploy(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	plan := webPlan()
	plan.Workloads[0].PullAuth = &api.RegistryAuth{Registry: "ghcr.io", Username: "bot", Password: secret.New("first")}
	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	read := func() string {
		s, err := cs.CoreV1().Secrets(testNS).Get(ctx, pullSecretName, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, corev1.SecretTypeDockerConfigJson, s.Type)
		return string(s.Data[corev1.DockerConfigJsonKey])
	}
	require.Contains(t, read(), `"password":"first"`)
	p := listPods(t, cs, testNS, "web")[0]
	require.Equal(t, pullSecretName, p.Spec.ImagePullSecrets[0].Name)

	plan.Workloads[0].PullAuth.Password = secret.New("second")
	_, err = a.Apply(ctx, plan)
	require.NoError(t, err)
	require.Contains(t, read(), `"password":"second"`)

	env, err := cs.CoreV1().Secrets(testNS).Get(ctx, envSecretName("web"), metav1.GetOptions{})
	require.NoError(t, err)
	for _, v := range env.Data {
		require.NotContains(t, string(v), "second")
	}

	plan.Workloads[0].PullAuth = nil
	_, err = a.Apply(ctx, plan)
	require.NoError(t, err)
	_, err = cs.CoreV1().Secrets(testNS).Get(ctx, pullSecretName, metav1.GetOptions{})
	require.Error(t, err, "removed when no workload needs it")
}

// TestR204_AVolumeIsRetainedAndOutlivesItsApp asserts R-204 on this runtime:
// the bound volume is set to Retain, an app destroyed with its volumes kept
// keeps its namespace and claim, and only destroying the volume itself lets
// the cluster delete the data.
func TestR204_AVolumeIsRetainedAndOutlivesItsApp(t *testing.T) {
	ctx := context.Background()
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-data"},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete},
	}
	a, cs := testAdapter(t, nil, pv)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)

	// The cluster binds the claim once its pod is placed.
	claim, err := cs.CoreV1().PersistentVolumeClaims(testNS).Get(ctx, pvcName("vol_01DATA"), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, claim.Spec.AccessModes)
	claim.Spec.VolumeName = "pv-data"
	_, err = cs.CoreV1().PersistentVolumeClaims(testNS).Update(ctx, claim, metav1.UpdateOptions{})
	require.NoError(t, err)

	_, err = a.Apply(ctx, webPlan())
	require.NoError(t, err)
	reclaim := func() corev1.PersistentVolumeReclaimPolicy {
		got, err := cs.CoreV1().PersistentVolumes().Get(ctx, "pv-data", metav1.GetOptions{})
		require.NoError(t, err)
		return got.Spec.PersistentVolumeReclaimPolicy
	}
	require.Equal(t, corev1.PersistentVolumeReclaimRetain, reclaim())

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: testBundle}, api.DestroyOptions{KeepVolumes: true}))
	require.Empty(t, listPods(t, cs, testNS, "web"))
	_, err = cs.CoreV1().Namespaces().Get(ctx, testNS, metav1.GetOptions{})
	require.NoError(t, err, "a claim lives in its namespace, so the namespace stays")
	observed, err := a.Observe(ctx, api.BundleRef{BundleID: testBundle})
	require.NoError(t, err)
	require.Len(t, observed.Volumes, 1)
	require.Equal(t, "vol_01DATA", observed.Volumes[0].VolumeID)

	require.NoError(t, a.DestroyVolume(ctx, api.VolumeHandle{Handle: observed.Volumes[0].Handle}))
	require.Equal(t, corev1.PersistentVolumeReclaimDelete, reclaim(), "destroying a volume destroys its data")
	_, err = cs.CoreV1().PersistentVolumeClaims(testNS).Get(ctx, pvcName("vol_01DATA"), metav1.GetOptions{})
	require.Error(t, err)
}

// TestR025_AClusterThatDoesNotEnforceNetworkPolicyIsUnusable asserts O-43:
// when the canary connects through a policy that should refuse it, the
// adapter is unhealthy with a message saying why, cannot provide a private
// network, and refuses to start an app.
func TestR025_AClusterThatDoesNotEnforceNetworkPolicyIsUnusable(t *testing.T) {
	ctx := context.Background()
	cfg := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12"}
	require.NoError(t, cfg.validate())
	a := &Adapter{probe: func(context.Context) (bool, error) { return false, nil }}
	a.use(fakeClient(), cfg)

	err := a.HealthCheck(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "does not enforce NetworkPolicy, so Pando cannot keep apps from reaching each other")
	require.Contains(t, errs.As(err).Remedy, "Calico or Cilium")

	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.False(t, caps.SupportsPrivateNetwork)
	require.False(t, caps.SupportsEgressRestriction)

	_, err = a.Apply(ctx, webPlan())
	require.Error(t, err)
}

// TestR186_ARestrictedAppReachesOutOnlyThroughItsGateway asserts R-186 and
// R-187: the workloads lose the rule reaching outside the cluster, a gateway
// pod alone gets it, and the workloads are told to use the gateway.
func TestR186_ARestrictedAppReachesOutOnlyThroughItsGateway(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	plan := webPlan()
	plan.Network.Egress = egress.Rules{BlockPrivate: true}
	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	def, err := cs.NetworkingV1().NetworkPolicies(testNS).Get(ctx, defaultPolicy, metav1.GetOptions{})
	require.NoError(t, err)
	for _, rule := range def.Spec.Egress {
		for _, peer := range rule.To {
			require.Nil(t, peer.IPBlock, "a restricted app's workloads have no route out")
		}
	}
	gw, err := cs.NetworkingV1().NetworkPolicies(testNS).Get(ctx, gatewayPolicy, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{labelRole: roleGateway}, gw.Spec.PodSelector.MatchLabels)
	require.Equal(t, "0.0.0.0/0", gw.Spec.Egress[0].To[0].IPBlock.CIDR)

	pod, err := cs.CoreV1().Pods(testNS).Get(ctx, gatewayName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/trypando/pando:test", pod.Spec.Containers[0].Image)
	require.Equal(t, []string{"egress-gateway"}, pod.Spec.Containers[0].Args)

	env, err := cs.CoreV1().Secrets(testNS).Get(ctx, envSecretName("web"), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "http://pando-egress:3128", string(env.Data["HTTPS_PROXY"]))
	require.Contains(t, string(env.Data["NO_PROXY"]), "db")

	observed, err := a.Observe(ctx, api.BundleRef{BundleID: testBundle})
	require.NoError(t, err)
	for _, w := range observed.Workloads {
		require.NotEqual(t, gatewayName, w.Name, "the gateway is Pando's, not a workload")
	}

	// Unrestricted again: the gateway goes and the route out comes back.
	_, err = a.Apply(ctx, webPlan())
	require.NoError(t, err)
	_, err = cs.CoreV1().Pods(testNS).Get(ctx, gatewayName, metav1.GetOptions{})
	require.Error(t, err)
}

// TestR186_WithoutAGatewayImageEgressCannotBeRestricted asserts the adapter
// says it cannot, and refuses a restricted plan rather than ignoring it.
func TestR186_WithoutAGatewayImageEgressCannotBeRestricted(t *testing.T) {
	ctx := context.Background()
	a, _ := testAdapter(t, func(c *Config) { c.EgressGatewayImage = "" })
	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.False(t, caps.SupportsEgressRestriction)

	plan := webPlan()
	plan.Network.Egress = egress.Rules{BlockPrivate: true}
	_, err = a.Apply(ctx, plan)
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
}

// TestR254_KubernetesCapabilitiesAreData asserts R-254: what this runtime can
// do is in the struct — registry delivery, routes through the API, no
// self-upgrade — and its isolation class comes from the RuntimeClass's handler.
func TestR254_KubernetesCapabilitiesAreData(t *testing.T) {
	ctx := context.Background()
	gvisor := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "runsc"}
	a, _ := testAdapter(t, nil, gvisor)
	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.Equal(t, spec.IsolationContainer, caps.IsolationClass)
	require.True(t, caps.SupportsPrivateNetwork)
	require.False(t, caps.Delivers(api.ImageDeliveryImport), "every node pulls; nothing is streamed in")
	require.Equal(t, []api.ImageDelivery{api.ImageDeliveryRegistry}, caps.ImageDelivery)
	require.Equal(t, []api.EdgeConfig{api.EdgeConfigKubernetesAPI}, caps.EdgeConfig)
	require.True(t, caps.SupportsEdge)
	require.False(t, caps.SupportsSelfUpgrade)
	require.False(t, caps.LogRetention.SupportsSizeCap)
	require.True(t, caps.SupportsPortObservation)
	require.False(t, caps.SupportsWriteObservation)

	sandboxed, _ := testAdapter(t, func(c *Config) { c.RuntimeClass = "gvisor" }, gvisor)
	caps, err = sandboxed.Capabilities(ctx)
	require.NoError(t, err)
	require.Equal(t, spec.IsolationSandboxed, caps.IsolationClass)
	require.False(t, caps.SupportsPortObservation, "a sandbox has its own network stack")
}

// TestR114_ARuntimeClassTheClusterLacksMakesTheAdapterUnhealthy asserts a
// configured RuntimeClass must exist: a sandboxed class satisfied by a runtime
// that was never installed would admit what a policy floor meant to exclude.
func TestR114_ARuntimeClassTheClusterLacksMakesTheAdapterUnhealthy(t *testing.T) {
	cfg := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12", RuntimeClass: "kata"}
	require.NoError(t, cfg.validate())
	a := &Adapter{probe: func(context.Context) (bool, error) { return true, nil }}
	a.use(fakeClient(), cfg)
	err := a.HealthCheck(context.Background())
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, `"kata"`)
}

// TestR243_CapacityIsTheEligibleNodesLessOthersRequests asserts R-243 on a
// cluster: the adapter reports schedulable nodes' allocatable room, less what
// pods outside Pando's namespaces ask for, and the largest single node's room
// as LargestFit (R-242).
func TestR243_CapacityIsTheEligibleNodesLessOthersRequests(t *testing.T) {
	node := func(name, cpu, mem string, mutate func(*corev1.Node)) *corev1.Node {
		n := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				NodeInfo:    corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "amd64"},
			},
		}
		if mutate != nil {
			mutate(n)
		}
		return n
	}
	pod := func(ns, nodeName, cpu, mem string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p-" + ns + nodeName, Namespace: ns},
			Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
			}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	appNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS, Labels: map[string]string{labelManagedBy: managedBy}}}
	a, _ := testAdapter(t, nil,
		node("a", "4", "8Gi", nil),
		node("b", "2", "4Gi", nil),
		node("cordoned", "8", "16Gi", func(n *corev1.Node) { n.Spec.Unschedulable = true }),
		node("control", "8", "16Gi", func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}
		}),
		appNS,
		pod("monitoring", "a", "1", "2Gi"),
		pod(testNS, "a", "1", "1Gi"),
	)
	capacity, err := a.Capacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, 5000, capacity.TotalCPUMillis, "4 + 2 cores, less 1 asked for by somebody else's pod")
	require.Equal(t, int64(10<<30), capacity.TotalMemoryBytes)
	require.Equal(t, api.Fit{CPUMillis: 3000, MemoryBytes: 6 << 30}, capacity.LargestFit)
	require.Zero(t, capacity.TotalDiskBytes, "storage is the storage class's, not counted")
	require.Equal(t, 2, capacity.Details["eligible_nodes"])

	caps, err := a.Capabilities(context.Background())
	require.NoError(t, err)
	require.Equal(t, "linux/amd64", caps.Platform)
}

// TestR174_TheEdgeRunsAsSpreadReplicasWithADisruptionBudget asserts R-174 on
// Kubernetes: Pando runs the edge itself, as at least two replicas spread
// softly across nodes, never all drained at once, behind the one LoadBalancer
// Service Pando creates, reaching Pando's proxy by an alias for Pando's own
// Service.
func TestR174_TheEdgeRunsAsSpreadReplicasWithADisruptionBudget(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	plan := api.EdgePlan{
		Name: "rte_traefik", Image: "traefik:v3.2", Args: []string{"--providers.kubernetescrd"},
		Ports:           []api.EdgePort{{Host: 80, Container: 80}, {Host: 443, Container: 443}},
		ProxyAlias:      "pando-proxy",
		ReadsRoutesFrom: api.EdgeConfigKubernetesAPI,
	}
	require.NoError(t, a.ApplyEdge(ctx, plan))

	d, err := cs.AppsV1().Deployments("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.NoError(t, err)
	require.EqualValues(t, 2, *d.Spec.Replicas)
	require.EqualValues(t, 0, d.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue())
	spread := d.Spec.Template.Spec.TopologySpreadConstraints[0]
	require.Equal(t, "kubernetes.io/hostname", spread.TopologyKey)
	require.Equal(t, corev1.ScheduleAnyway, spread.WhenUnsatisfiable, "soft: one node still runs the edge")
	require.Equal(t, edgeServiceAccount, d.Spec.Template.Spec.ServiceAccountName)
	require.Equal(t, componentEdge, d.Spec.Template.Labels[labelComponent])
	require.NotEqual(t, componentSrv, d.Spec.Template.Labels[labelComponent], "edge pods never carry Pando's server labels")

	pdb, err := cs.PolicyV1().PodDisruptionBudgets("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, pdb.Spec.MinAvailable.IntValue())

	svc, err := cs.CoreV1().Services("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, corev1.ServiceTypeLoadBalancer, svc.Spec.Type)
	require.Equal(t, corev1.ServiceExternalTrafficPolicyLocal, svc.Spec.ExternalTrafficPolicy)

	alias, err := cs.CoreV1().Services("pando-edge").Get(ctx, "pando-proxy", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, corev1.ServiceTypeExternalName, alias.Spec.Type)
	require.Equal(t, "pando.pando.svc.cluster.local", alias.Spec.ExternalName)

	names, err := a.Edges(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"rte_traefik"}, names, "the name core asked for, so the edge is not removed as unwanted")

	require.NoError(t, a.RemoveEdge(ctx, "rte_traefik"))
	names, err = a.Edges(ctx)
	require.NoError(t, err)
	require.Empty(t, names)
	_, err = cs.CoreV1().Services("pando-edge").Get(ctx, "pando-proxy", metav1.GetOptions{})
	require.Error(t, err, "the alias goes with the last edge")
}

// TestR174_EdgeNodePortsAreTheSettingsNot80And443 asserts the NodePort
// setting maps the edge's ports to the configured node ports.
func TestR174_EdgeNodePortsAreTheSettingsNot80And443(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, func(c *Config) { c.EdgeServiceType = "NodePort" })
	require.NoError(t, a.ApplyEdge(ctx, api.EdgePlan{
		Name: "rte_traefik", Image: "traefik:v3.2",
		Ports: []api.EdgePort{{Host: 80, Container: 80}, {Host: 443, Container: 443}},
	}))
	svc, err := cs.CoreV1().Services("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, corev1.ServiceTypeNodePort, svc.Spec.Type)
	require.EqualValues(t, 30080, svc.Spec.Ports[0].NodePort)
	require.EqualValues(t, 30443, svc.Spec.Ports[1].NodePort)
}

// TestR174_EdgeReplicasSharingANodeIsAWarning asserts a one-node cluster runs
// the edge and reports it serving, with a warning that is not a failure.
func TestR174_EdgeReplicasSharingANodeIsAWarning(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	require.NoError(t, a.ApplyEdge(ctx, api.EdgePlan{Name: "rte_cloudflare", Image: "cloudflare/cloudflared"}))

	d, err := cs.AppsV1().Deployments("pando-edge").Get(ctx, "pando-edge-rte-cloudflare", metav1.GetOptions{})
	require.NoError(t, err)
	require.False(t, *d.Spec.Template.Spec.AutomountServiceAccountToken, "an edge that reads no routes gets no API identity")
	d.Status.ReadyReplicas = 2
	_, err = cs.AppsV1().Deployments("pando-edge").UpdateStatus(ctx, d, metav1.UpdateOptions{})
	require.NoError(t, err)
	for _, name := range []string{"one", "two"} {
		_, err := cs.CoreV1().Pods("pando-edge").Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pando-edge", Labels: map[string]string{labelEdge: "rte-cloudflare"}},
			Spec:       corev1.PodSpec{NodeName: "only-node"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	state, err := a.ObserveEdge(ctx, "rte_cloudflare")
	require.NoError(t, err)
	require.True(t, state.Running, "a warning, never a blocker")
	require.Contains(t, state.Detail, "share a node")
	_, err = cs.CoreV1().Services("pando-edge").Get(ctx, "pando-edge-rte-cloudflare", metav1.GetOptions{})
	require.Error(t, err, "cloudflared dials out; nothing is published for it")
}

// TestR174_AnEdgeAskingForSharedStorageIsRefused asserts a plan made for
// Docker's file delivery is refused with the setting that fixes it.
func TestR174_AnEdgeAskingForSharedStorageIsRefused(t *testing.T) {
	a, _ := testAdapter(t, nil)
	err := a.ApplyEdge(context.Background(), api.EdgePlan{
		Name: "rte_traefik", Image: "traefik:v3.2",
		Mounts: []api.EdgeMount{{Path: "/etc/traefik/dynamic", SharedWithPando: "/var/lib/pando/traefik"}},
	})
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "kubernetes_api")
}

// TestR026_AWorkloadNameThatIsNotADNSLabelIsRefusedWithOneThatIs asserts a
// readable refusal naming a name that would work.
func TestR026_AWorkloadNameThatIsNotADNSLabelIsRefusedWithOneThatIs(t *testing.T) {
	a, _ := testAdapter(t, nil)
	plan := webPlan()
	plan.Workloads[1].Name = "My_DB"
	_, err := a.Apply(context.Background(), plan)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, `"my-db"`)
}

func TestConfigRefusesWhatCannotWork(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  Config
		want string
	}{
		"no pod range":  {Config{ServiceCIDR: "10.96.0.0/12"}, "pod_cidr"},
		"bad range":     {Config{PodCIDR: "10.244.0.0", ServiceCIDR: "10.96.0.0/12"}, "not an address range"},
		"one replica":   {Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12", EdgeReplicas: 1}, "at least 2"},
		"service type":  {Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12", EdgeServiceType: "ClusterIP"}, "LoadBalancer"},
		"node selector": {Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12", NodeSelector: "pool"}, "key=value"},
	} {
		err := tc.cfg.validate()
		require.Error(t, err, name)
		require.True(t, strings.Contains(errs.As(err).Message, tc.want), "%s: %s", name, errs.As(err).Message)
	}
	require.NoError(t, Info().Validate())
}

func find(t *testing.T, b api.ObservedBundle, name string) api.ObservedWorkload {
	t.Helper()
	for _, w := range b.Workloads {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("no workload %q in %+v", name, b.Workloads)
	return api.ObservedWorkload{}
}
