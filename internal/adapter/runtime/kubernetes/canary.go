package kubernetes

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/trypando/pando/internal/errs"
)

// The NetworkPolicy canary (O-43).
//
// Every isolation guarantee on this runtime — apps unreachable from each other
// (R-025) and from everything but Pando's proxy (R-023) — is a NetworkPolicy,
// and a NetworkPolicy is only true where the cluster's network plugin enforces
// it. Flannel and kubenet accept the objects and enforce nothing, and the API
// cannot say which plugin a cluster has. So the adapter tries it: a server
// behind a policy that admits one labeled client and nobody else, and two
// clients. The admitted one must connect, which shows the network works at
// all; the other must not, which shows the policy is enforced. Anything else
// makes the adapter unusable rather than quietly unisolated.

const (
	canaryNamespacePrefix = "pando-canary-"
	roleCanary            = "canary"
	canaryPort            = 8080

	// A passed check is good for an hour. A failed one is looked at again in
	// five minutes, so installing a network plugin does not wait an hour.
	canaryEvery       = time.Hour
	canaryRetryFailed = 5 * time.Minute
	canaryTimeout     = 3 * time.Minute
)

type canaryResult struct {
	checked  time.Time
	enforced bool
}

func errNotEnforced() error {
	return errs.New(errs.AdapterUnavailable,
		"This cluster does not enforce NetworkPolicy, so Pando cannot keep apps from reaching each other.").
		WithRemedy("Install a network plugin that enforces NetworkPolicy, such as Calico or Cilium. Pando checks again within five minutes.")
}

// canaryPassed is the last answer, without asking again. False until the
// canary has run and passed.
func (a *Adapter) canaryPassed() bool {
	a.canaryMu.Lock()
	defer a.canaryMu.Unlock()
	return !a.canary.checked.IsZero() && a.canary.enforced
}

// networkPolicyEnforced runs the canary when its last answer is stale.
func (a *Adapter) networkPolicyEnforced(ctx context.Context) (bool, error) {
	a.canaryMu.Lock()
	defer a.canaryMu.Unlock()

	if !a.canary.checked.IsZero() {
		age := a.now().Sub(a.canary.checked)
		if (a.canary.enforced && age < canaryEvery) || (!a.canary.enforced && age < canaryRetryFailed) {
			return a.canary.enforced, nil
		}
	}
	enforced, err := a.probe(ctx)
	if err != nil {
		// Not remembered: a canary that could not run says nothing about the
		// cluster, and the next health check tries again.
		return false, err
	}
	a.canary = canaryResult{checked: a.now(), enforced: enforced}
	return enforced, nil
}

// runCanary is the check itself, in a namespace of its own that it removes.
//
// The namespace is this run's alone, named with a random suffix. Every
// replica runs the canary, and when two shared one namespace, one replica's
// cleanup deleted it under the other's run: Kubernetes removes a terminating
// namespace's NetworkPolicy and its pods together, so for a moment the other
// run's refused client reached its server with no policy in the way, and
// that replica recorded the cluster as not enforcing NetworkPolicy — the
// whole runtime unavailable for five minutes, seen on kind whenever both
// replicas started at once.
func (a *Adapter) runCanary(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, canaryTimeout)
	defer cancel()

	cannot := func(what string, err error) error {
		return errs.Wrap(errs.AdapterUnavailable,
			"Pando could not check that this cluster enforces NetworkPolicy: "+what+".", err).
			WithRemedy(fmt.Sprintf("Check that Pando's ServiceAccount may create namespaces and pods, and that nodes can pull %s. Pando does not run apps on a cluster it has not checked.", a.config.HelperImage))
	}

	a.sweepCanaries(ctx)

	name := canaryNamespacePrefix + randomLabel(8)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{labelManagedBy: managedBy, labelPSAEnforce: "baseline", labelRole: roleCanary},
	}}
	if _, err := a.cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		return false, cannot("its namespace could not be created", err)
	}
	if err := a.bindAppRole(ctx, name); err != nil {
		return false, cannot("Pando could not be given its role in the canary's namespace", err)
	}
	if err := a.awaitServiceAccount(ctx, name); err != nil {
		return false, cannot("the canary's namespace never became ready for pods", err)
	}
	defer func() {
		// Its own context: the check's may have ended, and leaving the
		// namespace would leave pods behind.
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer done()
		_ = a.cs.CoreV1().Namespaces().Delete(cleanup, name, metav1.DeleteOptions{})
	}()

	pods := a.cs.CoreV1().Pods(name)
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "admit-one", Namespace: name},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "server"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "admitted"}},
				}},
			}},
		},
	}
	if _, err := a.cs.NetworkingV1().NetworkPolicies(name).Create(ctx, policy, metav1.CreateOptions{}); err != nil {
		return false, cannot("its NetworkPolicy could not be created", err)
	}

	server := a.canaryPod(name, "server", []string{"sh", "-c",
		fmt.Sprintf("mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p %d -h /www", canaryPort)})
	server.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		TCPSocket: &corev1.TCPSocketAction{Port: intOrPort(canaryPort)},
	}}
	if _, err := pods.Create(ctx, server, metav1.CreateOptions{}); err != nil {
		return false, cannot("its server pod could not be created", err)
	}
	ip, err := a.waitForPodIP(ctx, name, "server")
	if err != nil {
		return false, cannot("its server pod did not start", err)
	}

	connect := []string{"sh", "-c", fmt.Sprintf("wget -q -T 5 -O /dev/null http://%s:%d/", ip, canaryPort)}
	for _, role := range []string{"admitted", "refused"} {
		if _, err := pods.Create(ctx, a.canaryPod(name, role, connect), metav1.CreateOptions{}); err != nil {
			return false, cannot("its client pod could not be created", err)
		}
	}
	admitted, err := a.waitForExit(ctx, name, "admitted")
	if err != nil {
		return false, cannot("its client pod did not finish", err)
	}
	if admitted != 0 {
		return false, cannot("a pod the policy admits could not connect either, so the result says nothing about the policy", nil)
	}
	refused, err := a.waitForExit(ctx, name, "refused")
	if err != nil {
		return false, cannot("its client pod did not finish", err)
	}
	// Connected means the policy was ignored.
	return refused != 0, nil
}

// sweepCanaries removes canary namespaces older than any run can last: a
// replica that stopped mid-check leaves its namespace behind.
func (a *Adapter) sweepCanaries(ctx context.Context) {
	list, err := a.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: labelRole + "=" + roleCanary})
	if err != nil {
		return
	}
	for _, ns := range list.Items {
		if ns.DeletionTimestamp == nil && a.now().Sub(ns.CreationTimestamp.Time) > 2*canaryTimeout {
			_ = a.cs.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{})
		}
	}
}

func (a *Adapter) canaryPod(namespace, role string, cmd []string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: role, Namespace: namespace,
			Labels: map[string]string{"role": role, labelManagedBy: managedBy},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			TerminationGracePeriodSeconds: ptr.To(int64(1)),
			Containers: []corev1.Container{{
				Name: "canary", Image: a.config.HelperImage, Command: cmd,
			}},
		},
	}
}

// waitForPodIP waits for a pod to be running and returns its address.
func (a *Adapter) waitForPodIP(ctx context.Context, namespace, name string) (string, error) {
	for {
		p, err := a.cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
			return p.Status.PodIP, nil
		}
		if err == nil && (p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded) {
			return "", fmt.Errorf("pod %s exited", name)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(a.poll):
		}
	}
}

// waitForExit waits for a pod's first container to finish and returns its
// exit code.
func (a *Adapter) waitForExit(ctx context.Context, namespace, name string) (int, error) {
	for {
		p, err := a.cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			for _, cs := range p.Status.ContainerStatuses {
				if t := cs.State.Terminated; t != nil {
					return int(t.ExitCode), nil
				}
			}
			switch p.Status.Phase {
			case corev1.PodSucceeded:
				return 0, nil
			case corev1.PodFailed:
				return 1, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(a.poll):
		}
	}
}
