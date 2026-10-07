package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// The edge on Kubernetes (R-174, O-42).
//
// Pando runs the edge itself, Traefik included, in its own namespace. An
// EdgePlan becomes:
//
//   - a Deployment "pando-edge-<name>" of at least two replicas, softly
//     spread across nodes, rolling with maxUnavailable 0, and a
//     PodDisruptionBudget of minAvailable 1, so neither a node lost nor a
//     drain takes every replica at once;
//   - a Secret holding the plan's environment (R-194: secret.Value until the
//     call that writes it);
//   - one Service of type LoadBalancer or NodePort for its ports: the only
//     Service of either type Pando ever creates (R-026 for every app);
//   - an ExternalName Service named for ProxyAlias, resolving to Pando's own
//     Service, which is how the edge reaches Pando's proxy and nothing else
//     (R-023);
//   - a NetworkPolicy letting the edge reach Pando's server pods on the proxy
//     port, DNS, and addresses outside the cluster (ACME, DNS-01 providers,
//     the API server when it lies outside the excluded ranges). Every app
//     namespace refuses it anyway: edge pods do not carry Pando's server
//     labels.
//
// An edge restarts itself (restartPolicy Always, as the Docker edge is
// unless-stopped): R-149 – R-151 are about apps.

const (
	edgeServiceAccount = "pando-edge-traefik"
	annoEdgeDigest     = "pando.dev/edge-digest"
	annoEdgeName       = "pando.dev/edge-name"
	roleCertificate    = "certificate"
	edgeContainer      = "edge"
)

func edgeObjectName(name string) string { return "pando-edge-" + toLabel(name) }

// ApplyEdge converges one edge toward its plan.
func (a *Adapter) ApplyEdge(ctx context.Context, p api.EdgePlan) error {
	for _, m := range p.Mounts {
		switch {
		case m.SharedWithPando != "":
			return errs.Newf(errs.PlanCapabilityUnsupported,
				"The edge %q asks for a directory shared with Pando, and on Kubernetes an edge reads its routes from the cluster's API instead.", p.Name).
				WithRemedy("Set the routing adapter's delivery setting to kubernetes_api.")
		case m.Volume != "":
			return errs.Newf(errs.PlanCapabilityUnsupported,
				"The edge %q asks for storage of its own, and on Kubernetes an edge's replicas share nothing.", p.Name).
				WithRemedy("On Kubernetes the edge's certificates are issued by Pando and kept as Secrets, so it needs no storage of its own. Set the routing adapter's delivery setting to kubernetes_api.")
		}
	}
	if p.ProxyAlias != "" && !dnsLabel.MatchString(p.ProxyAlias) {
		return errs.Newf(errs.ValidInvalid,
			"The edge reaches Pando's proxy as %q, which is not a name a Service can have on Kubernetes.", p.ProxyAlias).
			WithRemedy("Set Pando's proxy upstream to a short name such as http://pando-proxy:8080.")
	}

	ns := a.config.EdgeNamespace
	name := edgeObjectName(p.Name)
	labels := map[string]string{
		labelManagedBy: managedBy, labelComponent: componentEdge, labelEdge: toLabel(p.Name),
	}
	selector := map[string]string{labelComponent: componentEdge, labelEdge: toLabel(p.Name)}

	env := map[string]string{}
	for k, v := range p.Env {
		env[k] = v.Reveal()
	}
	if err := a.putSecret(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-env", Namespace: ns, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       secretData(env),
	}, "the edge's settings"); err != nil {
		return err
	}

	if err := a.ensureAlias(ctx, p.ProxyAlias); err != nil {
		return err
	}
	if err := a.ensureCertificates(ctx, p); err != nil {
		return err
	}
	if err := a.ensureEdgeDeployment(ctx, p, name, labels, selector, env); err != nil {
		return err
	}
	if err := a.ensureEdgePDB(ctx, name, labels, selector); err != nil {
		return err
	}
	if err := a.ensureEdgeService(ctx, p, name, labels, selector); err != nil {
		return err
	}
	return a.putPolicy(ctx, a.edgePolicy(name, labels, selector))
}

// ensureCertificates writes the certificates Pando issued for this edge as
// kubernetes.io/tls Secrets in the edge's namespace, under the names its
// routes refer to, where Traefik's CRD provider loads them on every replica.
// One deleted by hand is written again on the next pass; one no longer in the
// plan is removed. Written only when changed, so Traefik does not reload for
// nothing.
func (a *Adapter) ensureCertificates(ctx context.Context, p api.EdgePlan) error {
	ns := a.config.EdgeNamespace
	client := a.cs.CoreV1().Secrets(ns)
	edge := toLabel(p.Name)
	labels := map[string]string{labelManagedBy: managedBy, labelEdge: edge, labelRole: roleCertificate}
	keep := map[string]bool{}
	for _, c := range p.Certificates {
		keep[c.Name] = true
		data := map[string][]byte{
			corev1.TLSCertKey:       c.CertPEM,
			corev1.TLSPrivateKeyKey: []byte(c.KeyPEM.Reveal()),
		}
		existing, err := client.Get(ctx, c.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			_, err = client.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: ns, Labels: labels},
				Type:       corev1.SecretTypeTLS,
				Data:       data,
			}, metav1.CreateOptions{})
		case err == nil:
			if existing.Type != corev1.SecretTypeTLS {
				// The type of a Secret cannot change; replace it.
				if err = client.Delete(ctx, c.Name, metav1.DeleteOptions{}); err == nil {
					_, err = client.Create(ctx, &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: ns, Labels: labels},
						Type:       corev1.SecretTypeTLS, Data: data,
					}, metav1.CreateOptions{})
				}
				break
			}
			if equalJSON(existing.Data, data) && equalJSON(existing.Labels, labels) {
				continue
			}
			existing.Data, existing.Labels = data, labels
			_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
		}
		if err != nil {
			return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not give the edge its certificate %s.", c.Name), err)
		}
	}
	return a.pruneCertificates(ctx, edge, keep)
}

// pruneCertificates removes an edge's certificate Secrets not in keep.
func (a *Adapter) pruneCertificates(ctx context.Context, edge string, keep map[string]bool) error {
	client := a.cs.CoreV1().Secrets(a.config.EdgeNamespace)
	list, err := client.List(ctx, metav1.ListOptions{LabelSelector: labelEdge + "=" + edge + "," + labelRole + "=" + roleCertificate})
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the edge's certificates.", err)
	}
	for _, s := range list.Items {
		if keep[s.Name] {
			continue
		}
		if err := client.Delete(ctx, s.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return errs.Wrap(errs.AdapterFailed, "Could not remove a certificate the edge no longer uses.", err)
		}
	}
	return nil
}

func (a *Adapter) ensureEdgeDeployment(ctx context.Context, p api.EdgePlan, name string, labels, selector map[string]string, env map[string]string) error {
	c := corev1.Container{
		Name:  edgeContainer,
		Image: p.Image,
		Args:  p.Args,
		EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name + "-env"},
		}}},
	}
	for _, port := range p.Ports {
		proto := corev1.ProtocolTCP
		if port.Protocol == "udp" {
			proto = corev1.ProtocolUDP
		}
		c.Ports = append(c.Ports, corev1.ContainerPort{ContainerPort: int32(port.Container), Protocol: proto}) //nolint:gosec
	}

	spec := corev1.PodSpec{
		RestartPolicy:      corev1.RestartPolicyAlways,
		EnableServiceLinks: ptr.To(false),
		Containers:         []corev1.Container{c},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
			MaxSkew:     1,
			TopologyKey: "kubernetes.io/hostname",
			// Soft: on a cluster with fewer nodes than replicas, replicas
			// share a node rather than wait (ObserveEdge warns).
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: selector},
		}},
	}
	if p.ReadsRoutesFrom == api.EdgeConfigKubernetesAPI {
		// An identity that reads routes, Services and certificates in the
		// edge's namespace, and nothing in Pando's or any app's.
		spec.ServiceAccountName = edgeServiceAccount
	} else {
		spec.AutomountServiceAccountToken = ptr.To(false)
	}

	digest := edgeDigest(p, env, a.config.EdgeReplicas)
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	maxUnavailable, maxSurge := intstr.FromInt32(0), intstr.FromInt32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.config.EdgeNamespace, Labels: labels,
			Annotations: map[string]string{annoEdgeDigest: digest, annoEdgeName: p.Name}},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(a.config.EdgeReplicas)), //nolint:gosec
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Strategy: appsv1.DeploymentStrategy{
				Type:          appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &maxUnavailable, MaxSurge: &maxSurge},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: map[string]string{annoEdgeDigest: digest}},
				Spec:       spec,
			},
		},
	}
	client := a.cs.AppsV1().Deployments(a.config.EdgeNamespace)
	existing, err := client.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = client.Create(ctx, d, metav1.CreateOptions{})
	case err == nil:
		if existing.Annotations[annoEdgeDigest] == digest {
			return nil
		}
		existing.Labels = d.Labels
		existing.Annotations = d.Annotations
		existing.Spec.Replicas = d.Spec.Replicas
		existing.Spec.Strategy = d.Spec.Strategy
		existing.Spec.Template = d.Spec.Template
		_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not start the edge %q.", p.Name), err)
	}
	return nil
}

// edgeDigest summarizes an edge's plan; environment values are hashed in,
// never stored. A changed digest rolls the Deployment.
func edgeDigest(p api.EdgePlan, env map[string]string, replicas int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s\x00", p.Image, strings.Join(p.Args, "\x01"), replicas, p.ReadsRoutesFrom)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "env\x00%s\x00%s\x00", k, env[k])
	}
	for _, port := range p.Ports {
		fmt.Fprintf(h, "port\x00%d\x00%d\x00%s\x00", port.Host, port.Container, port.Protocol)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func (a *Adapter) ensureEdgePDB(ctx context.Context, name string, labels, selector map[string]string) error {
	minAvailable := intstr.FromInt32(1)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.config.EdgeNamespace, Labels: labels},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: selector},
		},
	}
	_, err := a.cs.PolicyV1().PodDisruptionBudgets(a.config.EdgeNamespace).Create(ctx, pdb, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errs.Wrap(errs.AdapterFailed, "Could not protect the edge from being drained all at once.", err)
	}
	return nil
}

// ensureEdgeService publishes the edge's ports. externalTrafficPolicy Local,
// so the address Pando's proxy records is the visitor's and not a node's.
func (a *Adapter) ensureEdgeService(ctx context.Context, p api.EdgePlan, name string, labels, selector map[string]string) error {
	client := a.cs.CoreV1().Services(a.config.EdgeNamespace)
	if len(p.Ports) == 0 {
		// cloudflared dials out; nothing reaches it.
		err := client.Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return errs.Wrap(errs.AdapterFailed, "Could not remove the edge's Service.", err)
		}
		return nil
	}
	nodePort := a.config.EdgeServiceType == string(corev1.ServiceTypeNodePort)
	var ports []corev1.ServicePort
	for _, port := range p.Ports {
		proto := corev1.ProtocolTCP
		if port.Protocol == "udp" {
			proto = corev1.ProtocolUDP
		}
		sp := corev1.ServicePort{
			Name:       fmt.Sprintf("%s-%d", strings.ToLower(string(proto)), port.Container),
			Port:       int32(port.Host),                        //nolint:gosec
			TargetPort: intstr.FromInt32(int32(port.Container)), //nolint:gosec
			Protocol:   proto,
		}
		if nodePort {
			// The node port range does not include 80 and 443; the
			// balancer outside the cluster points at these.
			switch port.Container {
			case 80:
				sp.NodePort = int32(a.config.EdgeHTTPNodePort) //nolint:gosec
			case 443:
				sp.NodePort = int32(a.config.EdgeHTTPSNodePort) //nolint:gosec
			}
		}
		ports = append(ports, sp)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.config.EdgeNamespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceType(a.config.EdgeServiceType),
			Selector:              selector,
			Ports:                 ports,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
		},
	}
	existing, err := client.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = client.Create(ctx, svc, metav1.CreateOptions{})
	case err == nil:
		if existing.Spec.Type == svc.Spec.Type && equalJSON(existing.Spec.Ports, svc.Spec.Ports) {
			return nil
		}
		existing.Spec.Type = svc.Spec.Type
		existing.Spec.Ports = svc.Spec.Ports
		existing.Spec.Selector = selector
		existing.Spec.ExternalTrafficPolicy = svc.Spec.ExternalTrafficPolicy
		_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not publish the edge's ports.", err)
	}
	return nil
}

// ensureAlias makes the name the edge dials resolve to Pando's Service.
func (a *Adapter) ensureAlias(ctx context.Context, alias string) error {
	if alias == "" {
		return nil
	}
	target := fmt.Sprintf("%s.%s.svc.%s", a.config.PandoService, a.config.PandoNamespace, a.config.ClusterDomain)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: alias, Namespace: a.config.EdgeNamespace,
			Labels: map[string]string{labelManagedBy: managedBy, labelRole: "proxy-alias"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: target},
	}
	client := a.cs.CoreV1().Services(a.config.EdgeNamespace)
	existing, err := client.Get(ctx, alias, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = client.Create(ctx, svc, metav1.CreateOptions{})
	case err == nil:
		if existing.Spec.Type == corev1.ServiceTypeExternalName && existing.Spec.ExternalName == target {
			return nil
		}
		existing.Spec = svc.Spec
		_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not give the edge the name it reaches Pando by.", err)
	}
	return nil
}

func (a *Adapter) edgePolicy(name string, labels, selector map[string]string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	proxyPort := intstr.FromInt32(int32(a.config.ProxyPort)) //nolint:gosec
	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": a.config.PandoNamespace}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{labelName: "pando", labelComponent: componentSrv}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &proxyPort}},
		},
		dnsRule(),
		a.outsideRule(),
	}
	if a.config.APIServerCIDR != "" {
		_, n, _ := net.ParseCIDR(a.config.APIServerCIDR)
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: n.String()}}},
		})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.config.EdgeNamespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selector},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// ObserveEdge reports whether an edge is serving, in words an operator can act
// on when it is not, and warns when its replicas share a node.
func (a *Adapter) ObserveEdge(ctx context.Context, name string) (api.EdgeState, error) {
	ns := a.config.EdgeNamespace
	obj := edgeObjectName(name)
	d, err := a.cs.AppsV1().Deployments(ns).Get(ctx, obj, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return api.EdgeState{}, nil
	}
	if err != nil {
		return api.EdgeState{}, errs.Wrap(errs.AdapterUnavailable, "Could not read the edge from the cluster.", err)
	}
	state := api.EdgeState{Present: true, Running: d.Status.ReadyReplicas > 0}
	var details []string
	if !state.Running {
		details = append(details, fmt.Sprintf("None of the edge's %d replicas is ready yet.", a.config.EdgeReplicas))
	}

	if svc, err := a.cs.CoreV1().Services(ns).Get(ctx, obj, metav1.GetOptions{}); err == nil &&
		svc.Spec.Type == corev1.ServiceTypeLoadBalancer && len(svc.Status.LoadBalancer.Ingress) == 0 {
		details = append(details, "The cluster has not given the edge a load balancer address yet. If it has no load balancer, set the Kubernetes runtime's edge_service_type to NodePort and point a load balancer outside the cluster at the node ports.")
	}

	pods, err := a.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelEdge + "=" + toLabel(name)})
	if err == nil {
		nodes := map[string]bool{}
		running := 0
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" && p.DeletionTimestamp == nil {
				nodes[p.Spec.NodeName] = true
				running++
			}
		}
		// A warning, never a blocker (CLAUDE.md §4): the edge serves.
		if running > 1 && len(nodes) == 1 {
			details = append(details, "The edge's replicas share a node, so losing that node takes the edge offline. Add a node to the cluster to spread them.")
		}
	}
	state.Detail = strings.Join(details, " ")
	return state, nil
}

// RemoveEdge removes an edge and everything made for it. The proxy alias goes
// with the last edge.
func (a *Adapter) RemoveEdge(ctx context.Context, name string) error {
	ns := a.config.EdgeNamespace
	obj := edgeObjectName(name)
	steps := []func() error{
		func() error { return a.cs.AppsV1().Deployments(ns).Delete(ctx, obj, metav1.DeleteOptions{}) },
		func() error { return a.cs.CoreV1().Services(ns).Delete(ctx, obj, metav1.DeleteOptions{}) },
		func() error { return a.cs.PolicyV1().PodDisruptionBudgets(ns).Delete(ctx, obj, metav1.DeleteOptions{}) },
		func() error { return a.cs.CoreV1().Secrets(ns).Delete(ctx, obj+"-env", metav1.DeleteOptions{}) },
		func() error { return a.cs.NetworkingV1().NetworkPolicies(ns).Delete(ctx, obj, metav1.DeleteOptions{}) },
		func() error { return a.pruneCertificates(ctx, toLabel(name), nil) },
	}
	for _, step := range steps {
		if err := step(); err != nil && !apierrors.IsNotFound(err) {
			return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not remove the edge %q.", name), err)
		}
	}
	remaining, err := a.Edges(ctx)
	if err == nil && len(remaining) == 0 {
		aliases, err := a.cs.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: labelRole + "=proxy-alias"})
		if err == nil {
			for _, s := range aliases.Items {
				_ = a.cs.CoreV1().Services(ns).Delete(ctx, s.Name, metav1.DeleteOptions{})
			}
		}
	}
	return nil
}

// Edges names every edge that exists.
func (a *Adapter) Edges(ctx context.Context) ([]string, error) {
	list, err := a.cs.AppsV1().Deployments(a.config.EdgeNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelComponent + "=" + componentEdge + "," + labelManagedBy + "=" + managedBy,
	})
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not list the edges in the cluster.", err)
	}
	var out []string
	for _, d := range list.Items {
		if n := d.Annotations[annoEdgeName]; n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// EdgeVolumes: none. Edge replicas share nothing; certificates on this
// runtime are kept by Pando, not by the edge.
func (a *Adapter) EdgeVolumes(context.Context) ([]api.VolumeHandle, error) { return nil, nil }
