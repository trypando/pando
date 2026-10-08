package kubernetes

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// maxCarriedBytes is what one ConfigMap holds, less room for its metadata.
const maxCarriedBytes = 1000 * 1024

// Apply converges an app's namespace toward the plan.
//
// Idempotent: each object is compared with what exists, and a workload's pod is
// recreated only when its digest differs or it has exited. Calling Apply with a
// satisfied plan writes nothing but the pull Secret, which is rewritten on
// every call because a registry password can expire (O-41).
func (a *Adapter) Apply(ctx context.Context, p api.BundlePlan) (api.BundleHandle, error) {
	if !p.Network.Private {
		// R-026, checked rather than assumed.
		return api.BundleHandle{}, errs.New(errs.AdapterFailed, "Pando will not start an app on a shared network.")
	}
	for _, w := range p.Workloads {
		if err := checkWorkloadName(w.Name); err != nil {
			return api.BundleHandle{}, err
		}
		if err := checkFilesFit(w); err != nil {
			return api.BundleHandle{}, err
		}
	}
	if !a.canaryPassed() {
		// The planner refuses this runtime first (SupportsPrivateNetwork);
		// reaching here is a plan made against stale capabilities.
		return api.BundleHandle{}, errNotEnforced()
	}

	restricted := p.Network.Egress.Restricted()
	var rules string
	if restricted {
		if a.config.EgressGatewayImage == "" {
			return api.BundleHandle{}, errs.New(errs.PlanCapabilityUnsupported,
				"This app's egress rules restrict where it may connect, and the Kubernetes runtime has no image to run Pando's egress gateway from, so it cannot enforce them.").
				WithRemedy("Set the Kubernetes runtime's egress_gateway_image to an image of Pando.")
		}
		var err error
		if rules, err = egressRulesJSON(p.Network.Egress); err != nil {
			return api.BundleHandle{}, err
		}
	}

	ns := namespaceFor(p.BundleID)
	if err := a.ensureNamespace(ctx, p.BundleID); err != nil {
		return api.BundleHandle{}, err
	}
	if err := a.ensurePolicies(ctx, ns, restricted); err != nil {
		return api.BundleHandle{}, err
	}
	if err := a.ensureQuota(ctx, ns, p); err != nil {
		return api.BundleHandle{}, err
	}
	if err := a.ensurePullSecret(ctx, ns, p.Workloads); err != nil {
		return api.BundleHandle{}, err
	}
	if restricted {
		if err := a.ensureGateway(ctx, ns, p.BundleID, rules); err != nil {
			return api.BundleHandle{}, err
		}
	} else if err := a.removeGateway(ctx, ns); err != nil {
		return api.BundleHandle{}, err
	}

	for _, v := range p.Volumes {
		if _, err := a.CreateVolume(ctx, api.VolumeRequest{VolumeID: v.VolumeID, BundleID: p.BundleID, Name: v.Name}); err != nil {
			return api.BundleHandle{}, err
		}
	}

	var proxy map[string]string
	if restricted {
		proxy = proxyEnv(p)
	}
	planned := make(map[string]api.WorkloadPlan, len(p.Workloads))
	for _, w := range p.Workloads {
		planned[w.Name] = w
	}
	for _, w := range ordered(p.Workloads) {
		if err := a.ensureService(ctx, ns, p.BundleID, w); err != nil {
			return api.BundleHandle{}, err
		}
		a.waitForDependencies(ctx, ns, w, planned)
		if err := a.applyWorkload(ctx, ns, p, w, workloadEnv(w, proxy)); err != nil {
			return api.BundleHandle{}, err
		}
	}

	// A volume is bound once its first pod is placed, under a
	// WaitForFirstConsumer class, so the reclaim policy is set after the pods.
	if err := a.retainVolumes(ctx, ns); err != nil {
		return api.BundleHandle{}, err
	}
	return api.BundleHandle{BundleID: p.BundleID, Handle: ns}, nil
}

// ensureNamespace makes the app's namespace and gives Pando its role there.
func (a *Adapter) ensureNamespace(ctx context.Context, bundleID string) error {
	name := namespaceFor(bundleID)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			labelManagedBy:  managedBy,
			labelApp:        toLabel(bundleID),
			labelBundle:     toLabel(bundleID),
			labelPSAEnforce: "baseline",
		},
	}}
	existing, err := a.cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := a.cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return errs.Wrap(errs.AdapterFailed, "Could not create the app's namespace in the cluster.", err)
		}
	case err != nil:
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the app's namespace from the cluster.", err)
	case existing.DeletionTimestamp != nil:
		return errs.Newf(errs.AdapterFailed, "The app's namespace %s is still being removed from the cluster.", name).
			WithRemedy("Wait for the cluster to finish removing it, then deploy again. Pando tries again on its next pass.")
	case existing.Labels[labelManagedBy] != managedBy:
		// Pando does not take over a namespace somebody else made.
		return errs.Newf(errs.AdapterFailed, "The namespace %s exists in the cluster and was not made by Pando.", name).
			WithRemedy("Remove or rename that namespace. Pando will not put an app in a namespace it does not own.")
	}

	return a.bindAppRole(ctx, name)
}

// bindAppRole gives Pando's ServiceAccount its role in a namespace it made:
// the one ClusterRole it may bind, holding rights over pods, Services,
// claims, Secrets and policies, and nothing cluster-wide
// (deploy/kubernetes/rbac.yaml).
func (a *Adapter) bindAppRole(ctx context.Context, name string) error {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: roleBindingName, Namespace: name, Labels: map[string]string{labelManagedBy: managedBy}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: a.config.AppRole},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: a.config.ServiceAccount, Namespace: a.config.PandoNamespace,
		}},
	}
	if _, err := a.cs.RbacV1().RoleBindings(name).Create(ctx, rb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return errs.Wrap(errs.AdapterFailed, "Could not give Pando its role in the app's namespace.", err)
	}
	return nil
}

// ensurePolicies writes the namespace's NetworkPolicies (R-023, R-025, R-187).
//
// One policy selects every pod. Ingress: from the app's own pods, and from
// pods in Pando's namespace carrying Pando's server labels — one peer, so both
// must match. Egress: to the app's own pods, to cluster DNS, and, when egress
// is unrestricted, anywhere outside the cluster's pod and Service ranges. A
// restricted app's workloads have no route out but the gateway, which alone is
// selected by a second policy reaching outside the cluster.
func (a *Adapter) ensurePolicies(ctx context.Context, ns string, restricted bool) error {
	sameNamespace := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}
	pando := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": a.config.PandoNamespace}},
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			labelName: "pando", labelComponent: componentSrv,
		}},
	}
	egress := []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{sameNamespace}},
		dnsRule(),
	}
	if !restricted {
		egress = append(egress, a.outsideRule())
	}
	def := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: defaultPolicy, Namespace: ns, Labels: map[string]string{labelManagedBy: managedBy}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{From: []networkingv1.NetworkPolicyPeer{sameNamespace}},
				{From: []networkingv1.NetworkPolicyPeer{pando}},
			},
			Egress: egress,
		},
	}
	if err := a.putPolicy(ctx, def); err != nil {
		return err
	}

	if !restricted {
		err := a.cs.NetworkingV1().NetworkPolicies(ns).Delete(ctx, gatewayPolicy, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return errs.Wrap(errs.AdapterFailed, "Could not remove the app's egress gateway policy.", err)
		}
		return nil
	}
	gw := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayPolicy, Namespace: ns, Labels: map[string]string{labelManagedBy: managedBy}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelRole: roleGateway}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{a.outsideRule()},
		},
	}
	return a.putPolicy(ctx, gw)
}

// dnsRule admits cluster DNS.
func dnsRule() networkingv1.NetworkPolicyEgressRule {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	port := intstr.FromInt32(53)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &port}, {Protocol: &tcp, Port: &port}},
	}
}

// outsideRule admits every address outside the cluster's pod and Service
// ranges, in both address families.
func (a *Adapter) outsideRule() networkingv1.NetworkPolicyEgressRule {
	var v4, v6 []string
	for _, c := range []string{a.config.PodCIDR, a.config.ServiceCIDR} {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if n.IP.To4() != nil {
			v4 = append(v4, n.String())
		} else {
			v6 = append(v6, n.String())
		}
	}
	return networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{
		{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: v4}},
		{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: v6}},
	}}
}

func (a *Adapter) putPolicy(ctx context.Context, np *networkingv1.NetworkPolicy) error {
	client := a.cs.NetworkingV1().NetworkPolicies(np.Namespace)
	existing, err := client.Get(ctx, np.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = client.Create(ctx, np, metav1.CreateOptions{})
	case err == nil:
		if equalJSON(existing.Spec, np.Spec) {
			return nil
		}
		existing.Spec = np.Spec
		_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not write the app's network policy.", err)
	}
	return nil
}

// ensureQuota sets a ceiling on the namespace. Not the enforcement — R-242
// and R-026 are Pando's — but a bug in the adapter fails at the API:
//
//   - no NodePort and no LoadBalancer Service, ever (R-026);
//   - when every workload has limits, twice the CPU and memory the plan asks
//     for, so a pod being replaced and its replacement fit at once. Only
//     then: a quota on CPU refuses any pod that does not state its own.
func (a *Adapter) ensureQuota(ctx context.Context, ns string, p api.BundlePlan) error {
	client := a.cs.CoreV1().ResourceQuotas(ns)
	hard := corev1.ResourceList{
		corev1.ResourceServicesNodePorts:     resource.MustParse("0"),
		corev1.ResourceServicesLoadBalancers: resource.MustParse("0"),
	}
	limited := len(p.Workloads) > 0
	var cpu, mem int64
	for _, w := range p.Workloads {
		if w.Resources.CPUMillis <= 0 || w.Resources.MemoryBytes <= 0 {
			limited = false
			break
		}
		cpu += int64(w.Resources.CPUMillis)
		mem += w.Resources.MemoryBytes
	}
	if limited {
		cpu += gatewayCPUMillis + helperCPUMillis
		mem += gatewayMemory + helperMemory
		hard[corev1.ResourceRequestsCPU] = *resource.NewMilliQuantity(2*cpu, resource.DecimalSI)
		hard[corev1.ResourceLimitsCPU] = *resource.NewMilliQuantity(2*cpu, resource.DecimalSI)
		hard[corev1.ResourceRequestsMemory] = *resource.NewQuantity(2*mem, resource.BinarySI)
		hard[corev1.ResourceLimitsMemory] = *resource.NewQuantity(2*mem, resource.BinarySI)
	}
	q := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: quotaName, Namespace: ns, Labels: map[string]string{labelManagedBy: managedBy}},
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
	}
	existing, err := client.Get(ctx, quotaName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = client.Create(ctx, q, metav1.CreateOptions{})
	case err == nil:
		if equalJSON(existing.Spec, q.Spec) {
			return nil
		}
		existing.Spec = q.Spec
		_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not write the app's resource quota.", err)
	}
	return nil
}

// ensurePullSecret writes the credentials the namespace's pods pull with
// (O-41): rewritten at every Apply, because an ECR password lasts twelve
// hours, and removed when no workload needs one. App pods have no
// ServiceAccount token, so nothing in the app can read it.
func (a *Adapter) ensurePullSecret(ctx context.Context, ns string, workloads []api.WorkloadPlan) error {
	client := a.cs.CoreV1().Secrets(ns)
	auths := map[string]map[string]string{}
	for _, w := range workloads {
		if w.PullAuth == nil {
			continue
		}
		entry := map[string]string{}
		if w.PullAuth.Username != "" {
			entry["username"] = w.PullAuth.Username
			entry["password"] = w.PullAuth.Password.Reveal()
			entry["auth"] = base64.StdEncoding.EncodeToString([]byte(w.PullAuth.Username + ":" + w.PullAuth.Password.Reveal()))
		}
		if !w.PullAuth.IdentityToken.IsZero() {
			entry["identitytoken"] = w.PullAuth.IdentityToken.Reveal()
		}
		auths[registryOf(w.PullAuth.Registry, w.Image)] = entry
	}
	if len(auths) == 0 {
		err := client.Delete(ctx, pullSecretName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return errs.Wrap(errs.AdapterFailed, "Could not remove the app's image pull credentials.", err)
		}
		return nil
	}
	body, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not write the app's image pull credentials.", err)
	}
	return a.putSecret(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pullSecretName, Namespace: ns, Labels: map[string]string{labelManagedBy: managedBy}},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: body},
	}, "the app's image pull credentials")
}

// registryOf is the registry an auth entry is for: the one named, or the
// image reference's host, Docker Hub when it has none.
func registryOf(named, image string) string {
	if named != "" {
		return named
	}
	first, _, found := strings.Cut(image, "/")
	if found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return "https://index.docker.io/v1/"
}

func (a *Adapter) putSecret(ctx context.Context, s *corev1.Secret, what string) error {
	client := a.cs.CoreV1().Secrets(s.Namespace)
	existing, err := client.Get(ctx, s.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = client.Create(ctx, s, metav1.CreateOptions{})
	case err == nil:
		existing.Type = s.Type
		existing.Data = s.Data
		existing.Labels = s.Labels
		for k, v := range s.Annotations {
			if existing.Annotations == nil {
				existing.Annotations = map[string]string{}
			}
			existing.Annotations[k] = v
		}
		_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not write "+what+".", err)
	}
	return nil
}

// envKey is the key a workload's environment is HMACed under in its plan
// digest: the one already on its environment Secret, or a new one when there
// is none. A new key changes the digest, so a Secret deleted by hand costs
// the workload one restart, never a pod left on an old environment.
func (a *Adapter) envKey(ctx context.Context, ns, name string) ([]byte, error) {
	existing, err := a.cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		if key, err := base64.StdEncoding.DecodeString(existing.Annotations[annoEnvKey]); err == nil && len(key) == envKeySize {
			return key, nil
		}
	case !apierrors.IsNotFound(err):
		return nil, errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not read the environment of %q.", name), err)
	}
	key := make([]byte, envKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not generate a key.", err)
	}
	return key, nil
}

// envKeySize is the length of an envKey in bytes.
const envKeySize = 32

// ensureService gives a workload a headless Service named for it.
//
// Headless, so the name resolves to the pod rather than a virtual address and
// kube-proxy installs no rules for it — which matters at thousands of apps.
// publishNotReadyAddresses, so the name resolves while the readiness probe is
// failing, as a Docker container name does: whether to send traffic to an
// unhealthy app is Pando's decision, not the DNS server's.
//
// ClusterIP None, and never NodePort or LoadBalancer: nothing outside the
// cluster gets an address for an app (R-026).
func (a *Adapter) ensureService(ctx context.Context, ns, bundleID string, w api.WorkloadPlan) error {
	var ports []corev1.ServicePort
	for _, p := range w.Ports {
		proto := corev1.ProtocolTCP
		if p.Protocol == "udp" {
			proto = corev1.ProtocolUDP
		}
		ports = append(ports, corev1.ServicePort{
			Name:       fmt.Sprintf("%s-%d", strings.ToLower(string(proto)), p.Number),
			Port:       int32(p.Number),                   //nolint:gosec // validated by the spec
			TargetPort: intstr.FromInt32(int32(p.Number)), //nolint:gosec
			Protocol:   proto,
		})
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: w.Name, Namespace: ns,
			Labels: map[string]string{labelManagedBy: managedBy, labelBundle: toLabel(bundleID), labelWorkload: w.Name},
		},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 map[string]string{labelWorkload: w.Name},
			Ports:                    ports,
			PublishNotReadyAddresses: true,
		},
	}
	client := a.cs.CoreV1().Services(ns)
	existing, err := client.Get(ctx, w.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = client.Create(ctx, svc, metav1.CreateOptions{})
	case err == nil:
		if existing.Spec.Type != corev1.ServiceTypeClusterIP || existing.Spec.ClusterIP != corev1.ClusterIPNone {
			// A Service changed by hand into something reachable from
			// outside is replaced, not adjusted: ClusterIP cannot be changed
			// in place.
			if err = client.Delete(ctx, w.Name, metav1.DeleteOptions{}); err == nil {
				_, err = client.Create(ctx, svc, metav1.CreateOptions{})
			}
			break
		}
		if equalJSON(existing.Spec.Ports, svc.Spec.Ports) && equalJSON(existing.Spec.Selector, svc.Spec.Selector) &&
			existing.Spec.PublishNotReadyAddresses {
			return nil
		}
		existing.Spec.Ports = svc.Spec.Ports
		existing.Spec.Selector = svc.Spec.Selector
		existing.Spec.PublishNotReadyAddresses = true
		existing.Labels = svc.Labels
		_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not give %q its name in the cluster.", w.Name), err)
	}
	return nil
}

// checkFilesFit refuses carried files a ConfigMap cannot hold.
func checkFilesFit(w api.WorkloadPlan) error {
	total := 0
	for _, f := range w.Files {
		total += len(f.Content)
	}
	if total > maxCarriedBytes {
		return errs.Newf(errs.ValidInvalid,
			"The configuration files carried into %q add up to %d KiB, and the Kubernetes runtime can carry at most %d KiB per part of an app.",
			w.Name, total/1024, maxCarriedBytes/1024).
			WithRemedy("Build the larger files into the image instead of carrying them in the app's configuration.")
	}
	return nil
}

// applyWorkload converges one workload's pod.
func (a *Adapter) applyWorkload(ctx context.Context, ns string, p api.BundlePlan, w api.WorkloadPlan, env map[string]string) error {
	key, err := a.envKey(ctx, ns, envSecretName(w.Name))
	if err != nil {
		return err
	}
	digest := planDigest(w, env, key, a.config.RuntimeClass, a.config.NodeSelector)

	if err := a.putSecret(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: envSecretName(w.Name), Namespace: ns,
			Labels:      map[string]string{labelManagedBy: managedBy, labelWorkload: w.Name},
			Annotations: map[string]string{annoEnvKey: base64.StdEncoding.EncodeToString(key)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: secretData(env),
	}, fmt.Sprintf("the environment of %q", w.Name)); err != nil {
		return err
	}
	if err := a.ensureFiles(ctx, ns, w); err != nil {
		return err
	}

	pods, err := a.workloadPods(ctx, ns, w.Name)
	if err != nil {
		return err
	}
	current := newest(pods)
	if current != nil && current.Annotations[annoDigest] == digest && live(current) {
		// Running or on its way: nothing to do but tidy finished ones away
		// once the current pod runs (below).
		if current.Status.Phase == corev1.PodRunning {
			a.deleteFinished(ctx, ns, pods, current.Name, "")
		}
		return nil
	}

	// Recreate (R-144). A changed plan replaces every pod of the workload. An
	// exited pod of the same plan is kept until its replacement runs, so the
	// log of the crash that sent an app to failed stays readable.
	keep := ""
	if current != nil && current.Annotations[annoDigest] == digest && finished(current) {
		keep = current.Name
	}
	for _, old := range pods {
		if old.Name == keep || old.DeletionTimestamp != nil {
			continue
		}
		if err := a.deletePod(ctx, ns, old.Name); err != nil {
			return err
		}
	}

	pod := a.podFor(ns, p, w, digest)
	created, err := a.cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not create %q.", w.Name), err)
	}
	return a.waitScheduled(ctx, ns, created.Name, w)
}

// live reports a pod that is running or will be.
func live(p *corev1.Pod) bool {
	return p.DeletionTimestamp == nil && (p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodPending || p.Status.Phase == "")
}

func finished(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// deleteFinished removes a workload's exited pods other than keep.
func (a *Adapter) deleteFinished(ctx context.Context, ns string, pods []corev1.Pod, current, keep string) {
	for _, p := range pods {
		if p.Name != current && p.Name != keep && finished(&p) && p.DeletionTimestamp == nil {
			_ = a.deletePod(ctx, ns, p.Name)
		}
	}
}

func (a *Adapter) deletePod(ctx context.Context, ns, name string) error {
	err := a.cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return errs.Wrap(errs.AdapterFailed, "Could not remove a stopped part of the app.", err)
	}
	return nil
}

// waitScheduled turns a pod the scheduler cannot place into an error that
// says why, rather than a pod left pending for room that is not there.
func (a *Adapter) waitScheduled(ctx context.Context, ns, name string, w api.WorkloadPlan) error {
	if a.scheduleWait <= 0 {
		return nil
	}
	deadline := a.now().Add(a.scheduleWait)
	for a.now().Before(deadline) {
		// A pod that cannot be read now is looked at again; one placed on a
		// node has room.
		if p, err := a.cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			if limited := pullLimited(p, w.PullAuth != nil); limited != nil {
				return limited
			}
			if p.Spec.NodeName != "" {
				return nil
			}
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
					return unschedulable(w, c.Message)
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.poll):
		}
	}
	return nil
}

func unschedulable(w api.WorkloadPlan, why string) error {
	need := ""
	if w.Resources.MemoryBytes > 0 || w.Resources.CPUMillis > 0 {
		need = fmt.Sprintf(" It asks for %d thousandths of a CPU and %d MiB of memory.", w.Resources.CPUMillis, w.Resources.MemoryBytes>>20)
	}
	return errs.Newf(errs.CapacityWouldOversubscribe,
		"No machine in the cluster has room for %q.%s The cluster's scheduler said: %s", w.Name, need, why).
		WithRemedy(fmt.Sprintf("Lower what %q asks for, stop another app, or add a machine to the cluster.", w.Name))
}

// podFor is a workload's pod.
//
// A bare pod with restartPolicy Never, owned by no controller: a pod that
// exits stays exited, and only the reconciler, under R-149's backoff, makes
// another. Every controller that keeps a pod running would restart it forever
// instead, and a failed app would not stay failed (R-151).
func (a *Adapter) podFor(ns string, p api.BundlePlan, w api.WorkloadPlan, digest string) *corev1.Pod {
	sel, _ := parseSelector(a.config.NodeSelector)
	labels := map[string]string{
		labelManagedBy: managedBy,
		labelApp:       toLabel(p.Labels["pando.app"]),
		labelBundle:    toLabel(p.BundleID),
		labelWorkload:  w.Name,
	}

	c := corev1.Container{
		Name:       appContainer,
		Image:      w.Image,
		Command:    w.Entrypoint,
		Args:       w.Command,
		WorkingDir: w.WorkingDir,
		EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: envSecretName(w.Name)}},
		}},
		ReadinessProbe:  readinessProbe(w.Health),
		Resources:       resourcesFor(w.Resources),
		ImagePullPolicy: corev1.PullIfNotPresent,
	}
	for _, port := range w.Ports {
		proto := corev1.ProtocolTCP
		if port.Protocol == "udp" {
			proto = corev1.ProtocolUDP
		}
		// ContainerPort documents the port. HostPort is never set: nothing
		// outside the cluster reaches an app (R-026).
		c.Ports = append(c.Ports, corev1.ContainerPort{ContainerPort: int32(port.Number), Protocol: proto}) //nolint:gosec
	}

	var volumes []corev1.Volume
	for _, m := range w.Mounts {
		vname := "vol-" + toLabel(m.VolumeID)
		volumes = append(volumes, corev1.Volume{Name: vname, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName(m.VolumeID), ReadOnly: m.ReadOnly},
		}})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: vname, MountPath: m.Path, ReadOnly: m.ReadOnly})
	}
	if len(w.Files) > 0 {
		items := make([]corev1.KeyToPath, 0, len(w.Files))
		for i, f := range sortedFiles(w.Files) {
			mode := int32(f.Mode) //nolint:gosec // file modes fit
			if mode == 0 {
				mode = 0o644
			}
			key := fmt.Sprintf("f%d", i)
			items = append(items, corev1.KeyToPath{Key: key, Path: key, Mode: ptr.To(mode)})
			// subPath, so the file can sit over one in the image without
			// hiding the rest of its directory.
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "carried-files", MountPath: f.Path, SubPath: key, ReadOnly: true})
		}
		volumes = append(volumes, corev1.Volume{Name: "carried-files", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: filesName(w.Name)}, Items: items},
		}})
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName(w.Name), Namespace: ns, Labels: labels,
			Annotations: map[string]string{annoDigest: digest, annoCreated: a.now().Format(time.RFC3339Nano), annoBundleID: p.BundleID},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Nothing in an app can reach the cluster's API.
			AutomountServiceAccountToken: ptr.To(false),
			// No environment variables naming other Services: the app gets
			// exactly the environment its spec gives it (R-028).
			EnableServiceLinks: ptr.To(false),
			Hostname:           w.Name,
			Containers:         []corev1.Container{c},
			Volumes:            volumes,
			NodeSelector:       sel,
		},
	}
	if len(pod.Spec.NodeSelector) == 0 {
		pod.Spec.NodeSelector = nil
	}
	if a.config.RuntimeClass != "" {
		pod.Spec.RuntimeClassName = ptr.To(a.config.RuntimeClass)
	}
	for _, x := range p.Workloads {
		if x.PullAuth != nil {
			pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: pullSecretName}}
			break
		}
	}
	return pod
}

// readinessProbe is the HealthPlan as a readiness probe, never a liveness
// probe: a liveness probe makes the kubelet kill the container, which is the
// runtime remediating (design 03 §2.2). Readiness only reports.
func readinessProbe(h *api.HealthPlan) *corev1.Probe {
	if h == nil {
		return nil
	}
	probe := &corev1.Probe{}
	switch {
	case len(h.Command) > 0:
		probe.Exec = &corev1.ExecAction{Command: h.Command}
	case h.Path != "" && h.Port > 0:
		probe.HTTPGet = &corev1.HTTPGetAction{Path: h.Path, Port: intOrPort(h.Port)}
	case h.Port > 0:
		probe.TCPSocket = &corev1.TCPSocketAction{Port: intOrPort(h.Port)}
	default:
		return nil
	}
	if h.IntervalSeconds > 0 {
		probe.PeriodSeconds = int32(h.IntervalSeconds) //nolint:gosec
	}
	if h.TimeoutSeconds > 0 {
		probe.TimeoutSeconds = int32(h.TimeoutSeconds) //nolint:gosec
	}
	if h.Retries > 0 {
		probe.FailureThreshold = int32(h.Retries) //nolint:gosec
	}
	return probe
}

func intOrPort(port int) intstr.IntOrString { return intstr.FromInt32(int32(port)) } //nolint:gosec

// resourcesFor sets requests equal to limits, so the scheduler's arithmetic
// and R-242's agree and the pod is in the Guaranteed class.
func resourcesFor(r api.ResourcePlan) corev1.ResourceRequirements {
	list := corev1.ResourceList{}
	if r.CPUMillis > 0 {
		list[corev1.ResourceCPU] = *resource.NewMilliQuantity(int64(r.CPUMillis), resource.DecimalSI)
	}
	if r.MemoryBytes > 0 {
		list[corev1.ResourceMemory] = *resource.NewQuantity(r.MemoryBytes, resource.BinarySI)
	}
	if len(list) == 0 {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{Limits: list, Requests: list.DeepCopy()}
}

// ensureFiles writes a workload's carried files into its ConfigMap.
func (a *Adapter) ensureFiles(ctx context.Context, ns string, w api.WorkloadPlan) error {
	client := a.cs.CoreV1().ConfigMaps(ns)
	if len(w.Files) == 0 {
		err := client.Delete(ctx, filesName(w.Name), metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not remove the files carried into %q.", w.Name), err)
		}
		return nil
	}
	data := map[string]string{}
	for i, f := range sortedFiles(w.Files) {
		data[fmt.Sprintf("f%d", i)] = f.Content
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: filesName(w.Name), Namespace: ns, Labels: map[string]string{labelManagedBy: managedBy, labelWorkload: w.Name}},
		Data:       data,
	}
	existing, err := client.Get(ctx, cm.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = client.Create(ctx, cm, metav1.CreateOptions{})
	case err == nil:
		existing.Data = data
		_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not write the files carried into %q.", w.Name), err)
	}
	return nil
}

func sortedFiles(files []api.FilePlan) []api.FilePlan {
	out := append([]api.FilePlan(nil), files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func secretData(env map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(env))
	for k, v := range env {
		out[k] = []byte(v)
	}
	return out
}

// planDigest summarizes what a workload's pod is made from.
//
// The digest is an annotation anyone who can read the pod can see, and the
// environment holds resolved secrets — a database password among them. So
// the environment goes in only as an HMAC under envKey, a random key kept on
// the environment's own Secret: whoever can check a guess against it can
// already read the values. Plain SHA-256 over the values let anyone who could
// read pods but not Secrets test guesses at a weak password (CodeQL
// go/weak-sensitive-data-hashing, R-194).
func planDigest(w api.WorkloadPlan, env map[string]string, envKey []byte, runtimeClass, nodeSelector string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	mac := hmac.New(sha256.New, envKey)
	for _, k := range keys {
		fmt.Fprintf(mac, "%s\x00%s\x00", k, env[k])
	}

	h := sha256.New()
	write := func(parts ...any) {
		for _, p := range parts {
			fmt.Fprintf(h, "%v\x00", p)
		}
	}
	write("image", w.Image, "cmd", strings.Join(w.Command, "\x01"), "entry", strings.Join(w.Entrypoint, "\x01"), "wd", w.WorkingDir)
	write("env", hex.EncodeToString(mac.Sum(nil)))
	for _, p := range w.Ports {
		write("port", p.Number, p.Protocol)
	}
	for _, m := range w.Mounts {
		write("mount", m.VolumeID, m.Path, m.ReadOnly)
	}
	for _, f := range sortedFiles(w.Files) {
		write("file", f.Path, f.Mode, f.Content)
	}
	if w.Health != nil {
		write("health", strings.Join(w.Health.Command, "\x01"), w.Health.Path, w.Health.Port, w.Health.IntervalSeconds, w.Health.TimeoutSeconds, w.Health.Retries)
	}
	write("res", w.Resources.CPUMillis, w.Resources.MemoryBytes, "rc", runtimeClass, "sel", nodeSelector, "pull", w.PullAuth != nil)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// podName is a workload's name and a random suffix: a finished pod cannot be
// started again, so every start is a new pod with a new name.
func podName(workload string) string {
	return workload + "-" + randomLabel(5)
}

// randomLabel is n random characters that are valid in a DNS label.
func randomLabel(n int) string {
	const alphabet = "bcdfghjklmnpqrstvwxz2456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// workloadPods lists a workload's pods.
func (a *Adapter) workloadPods(ctx context.Context, ns, workload string) ([]corev1.Pod, error) {
	list, err := a.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelWorkload + "=" + workload})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}
	var out []corev1.Pod
	for _, p := range list.Items {
		if p.Labels[labelRole] == "" && p.Labels[labelWorkload] == workload {
			out = append(out, p)
		}
	}
	return out, nil
}

// newest is the most recently created pod, preferring one not being deleted.
func newest(pods []corev1.Pod) *corev1.Pod {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		switch {
		case best == nil:
			best = p
		case (best.DeletionTimestamp != nil) != (p.DeletionTimestamp != nil):
			if p.DeletionTimestamp == nil {
				best = p
			}
		case createdAt(p).After(createdAt(best)):
			best = p
		}
	}
	return best
}

// createdAt is when Pando made a pod, to the nanosecond. The API's own
// creation timestamp has whole seconds, and a replacement is often made in
// the same second as the pod it replaces.
func createdAt(p *corev1.Pod) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, p.Annotations[annoCreated]); err == nil {
		return t
	}
	return p.CreationTimestamp.Time
}

// waitForDependencies holds a workload back until each dependency with a
// health check reports ready (R-096), as the Docker adapter does.
func (a *Adapter) waitForDependencies(ctx context.Context, ns string, w api.WorkloadPlan, planned map[string]api.WorkloadPlan) {
	for _, dep := range w.DependsOn {
		if d, ok := planned[dep]; !ok || d.Health == nil {
			continue
		}
		deadline := a.now().Add(a.dependencyWait)
		for a.now().Before(deadline) {
			if a.settled(ctx, ns, dep) {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(a.poll):
			}
		}
	}
}

// settled reports a dependency that is ready, or not worth waiting for.
func (a *Adapter) settled(ctx context.Context, ns, workload string) bool {
	pods, err := a.workloadPods(ctx, ns, workload)
	if err != nil {
		return true
	}
	p := newest(pods)
	if p == nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return true
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ordered sorts workloads so dependencies start first (R-096).
func ordered(workloads []api.WorkloadPlan) []api.WorkloadPlan {
	byName := make(map[string]api.WorkloadPlan, len(workloads))
	for _, w := range workloads {
		byName[w.Name] = w
	}
	var out []api.WorkloadPlan
	placed := map[string]bool{}
	var place func(api.WorkloadPlan)
	place = func(w api.WorkloadPlan) {
		if placed[w.Name] {
			return
		}
		placed[w.Name] = true
		for _, dep := range w.DependsOn {
			if d, ok := byName[dep]; ok {
				place(d)
			}
		}
		out = append(out, w)
	}
	for _, w := range workloads {
		place(w)
	}
	return out
}

// retainVolumes sets every bound volume of the namespace to Retain, so that a
// deleted claim or namespace does not take its data with it (R-204). The
// state-side half is ON DELETE RESTRICT on volumes.app_id.
func (a *Adapter) retainVolumes(ctx context.Context, ns string) error {
	claims, err := a.cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedBy})
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the app's storage.", err)
	}
	for _, c := range claims.Items {
		if err := a.setReclaim(ctx, c.Spec.VolumeName, corev1.PersistentVolumeReclaimRetain); err != nil {
			return err
		}
	}
	return nil
}

// setReclaim sets a volume's reclaim policy, when it is bound to one.
func (a *Adapter) setReclaim(ctx context.Context, pv string, policy corev1.PersistentVolumeReclaimPolicy) error {
	if pv == "" {
		return nil
	}
	existing, err := a.cs.CoreV1().PersistentVolumes().Get(ctx, pv, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the app's storage.", err)
	}
	if existing.Spec.PersistentVolumeReclaimPolicy == policy {
		return nil
	}
	patch := fmt.Sprintf(`{"spec":{"persistentVolumeReclaimPolicy":%q}}`, policy)
	if _, err := a.cs.CoreV1().PersistentVolumes().Patch(ctx, pv, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not set how the cluster keeps the app's storage.", err)
	}
	return nil
}

func equalJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}
