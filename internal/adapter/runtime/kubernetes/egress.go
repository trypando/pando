package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/egress"
	"github.com/trypando/pando/internal/errs"
)

// Egress enforcement (R-185 – R-187), the Docker adapter's mechanism one
// namespace over.
//
// An app whose rules restrict nothing runs as it would with no egress controls
// (R-186): its namespace's policy admits any address outside the cluster, and
// nothing is put in its path. An app whose rules restrict something loses that
// rule: its workloads reach each other and cluster DNS and nothing else. One
// pod in the namespace, the gateway, is selected by a second policy reaching
// outside the cluster, and the workloads are told to use it through
// HTTP_PROXY and HTTPS_PROXY. Raw TCP and clients that ignore the variables
// have nowhere to go (R-187). Lookups through cluster DNS still leave, which
// is wider than on Docker: the cluster's resolver answers any name.

const (
	gatewayName   = "pando-egress"
	gatewayBinary = "/usr/local/bin/pando"

	// The hardened base image's nonroot user, by number.
	gatewayUID = 65532

	gatewayCPUMillis = 250
	gatewayMemory    = 128 << 20

	helperCPUMillis = 250
	helperMemory    = 128 << 20

	annoEgressDigest = "pando.dev/egress-digest"
)

var gatewayProxyURL = "http://" + gatewayName + ":" + strconv.Itoa(egress.DefaultPort)

// proxyEnv is what a restricted app's workloads are given (R-187). NO_PROXY
// names every workload: they reach each other directly.
func proxyEnv(p api.BundlePlan) map[string]string {
	noProxy := []string{"localhost", "127.0.0.1", "::1"}
	names := make([]string, 0, len(p.Workloads))
	for _, w := range p.Workloads {
		names = append(names, w.Name)
	}
	sort.Strings(names)
	noProxy = append(noProxy, names...)
	np := strings.Join(noProxy, ",")
	return map[string]string{
		"HTTP_PROXY": gatewayProxyURL, "HTTPS_PROXY": gatewayProxyURL,
		"http_proxy": gatewayProxyURL, "https_proxy": gatewayProxyURL,
		"NO_PROXY": np, "no_proxy": np,
	}
}

// workloadEnv is a workload's environment as its pod will have it. Revealed
// here, into the Secret the pod reads: the adapter never learned which values
// were sensitive.
func workloadEnv(w api.WorkloadPlan, proxy map[string]string) map[string]string {
	env := make(map[string]string, len(w.Env)+len(proxy))
	for k, v := range w.Env {
		env[k] = v.Reveal()
	}
	for k, v := range proxy {
		if (k == "NO_PROXY" || k == "no_proxy") && env[k] != "" {
			v = v + "," + env[k]
		}
		env[k] = v
	}
	return env
}

// egressRulesJSON is the rules as the gateway reads them, checked first.
func egressRulesJSON(r api.EgressRules) (string, error) {
	if _, err := r.Compile(); err != nil {
		return "", errs.Wrap(errs.ValidInvalid, "This app's egress rules have an entry Pando cannot enforce.", err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not write down this app's egress rules.", err)
	}
	return string(raw), nil
}

// ensureGateway runs the app's egress gateway and the Service its workloads
// reach it by.
//
// restartPolicy Always, unlike a workload: the gateway is Pando's own and has
// no failed state, and while it is down the app can reach nothing, which the
// reconciler, watching workloads, would not see. The Docker adapter's gateway
// restarts itself for the same reason.
func (a *Adapter) ensureGateway(ctx context.Context, ns, bundleID, rules string) error {
	h := sha256.New()
	h.Write([]byte(a.config.EgressGatewayImage + "\x00" + rules))
	digest := hex.EncodeToString(h.Sum(nil))[:16]

	pods := a.cs.CoreV1().Pods(ns)
	existing, err := pods.Get(ctx, gatewayName, metav1.GetOptions{})
	switch {
	case err == nil && existing.Annotations[annoEgressDigest] == digest && existing.DeletionTimestamp == nil:
		// Already running these rules.
	case err == nil:
		if err := a.deletePod(ctx, ns, gatewayName); err != nil {
			return err
		}
		// The new pod takes the same name, so the old one must be gone.
		if err := a.waitGone(ctx, ns, gatewayName); err != nil {
			return err
		}
		fallthrough
	case apierrors.IsNotFound(err):
		if _, err := pods.Create(ctx, a.gatewayPod(ns, bundleID, rules, digest), metav1.CreateOptions{}); err != nil {
			return errs.Wrap(errs.AdapterFailed, "Could not start this app's egress gateway.", err)
		}
	default:
		return errs.Wrap(errs.AdapterUnavailable, "Could not read this app's egress gateway.", err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayName, Namespace: ns, Labels: map[string]string{labelManagedBy: managedBy, labelRole: roleGateway}},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
			Selector:  map[string]string{labelRole: roleGateway},
			Ports: []corev1.ServicePort{{
				Name: "proxy", Port: egress.DefaultPort, TargetPort: intstr.FromInt32(egress.DefaultPort), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	if _, err := a.cs.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return errs.Wrap(errs.AdapterFailed, "Could not give this app's egress gateway its name in the cluster.", err)
	}
	return nil
}

func (a *Adapter) gatewayPod(ns, bundleID, rules, digest string) *corev1.Pod {
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(gatewayCPUMillis, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(gatewayMemory, resource.BinarySI),
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: gatewayName, Namespace: ns,
			Labels:      map[string]string{labelManagedBy: managedBy, labelBundle: toLabel(bundleID), labelRole: roleGateway},
			Annotations: map[string]string{annoEgressDigest: digest},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: ptr.To(false),
			EnableServiceLinks:           ptr.To(false),
			Containers: []corev1.Container{{
				Name:    "gateway",
				Image:   a.config.EgressGatewayImage,
				Command: []string{gatewayBinary},
				Args:    []string{"egress-gateway"},
				Env: []corev1.EnvVar{
					{Name: "PANDO_EGRESS_RULES", Value: rules},
					{Name: "PANDO_EGRESS_LISTEN", Value: ":" + strconv.Itoa(egress.DefaultPort)},
				},
				Ports:     []corev1.ContainerPort{{ContainerPort: egress.DefaultPort, Protocol: corev1.ProtocolTCP}},
				Resources: corev1.ResourceRequirements{Limits: limits, Requests: limits.DeepCopy()},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                ptr.To(int64(gatewayUID)),
					RunAsGroup:               ptr.To(int64(gatewayUID)),
					RunAsNonRoot:             ptr.To(true),
					ReadOnlyRootFilesystem:   ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

// removeGateway takes the gateway away when an app's rules stop restricting.
func (a *Adapter) removeGateway(ctx context.Context, ns string) error {
	if err := a.deletePod(ctx, ns, gatewayName); err != nil {
		return err
	}
	err := a.cs.CoreV1().Services(ns).Delete(ctx, gatewayName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return errs.Wrap(errs.AdapterFailed, "Could not remove this app's egress gateway.", err)
	}
	return nil
}

// waitGone waits for a pod to be removed.
func (a *Adapter) waitGone(ctx context.Context, ns, name string) error {
	deadline := a.now().Add(a.dependencyWait)
	for a.now().Before(deadline) {
		_, err := a.cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err := sleep(ctx, a.poll); err != nil {
			return err
		}
	}
	return errs.Newf(errs.AdapterFailed, "The cluster did not remove %s in time.", name)
}
