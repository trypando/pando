package kubernetes

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/ptr"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Trial starts a workload once in a namespace of its own and reports what it
// did (R-097). An observer container in the same pod shares its network
// namespace — what the Docker adapter's sidecar joins — and reads the
// listening sockets from /proc/net/tcp, so any image is observed, one with no
// shell included. Writes are not observed: nothing like a container diff
// exists in the API (SupportsWriteObservation is false).
//
// The namespace denies every connection in, Pando's included: nothing needs to
// reach an app that is being watched. It is removed at the end, on every path.
func (a *Adapter) Trial(ctx context.Context, req api.TrialRequest) (api.TrialResult, error) {
	if req.TrialID == "" {
		return api.TrialResult{}, errs.New(errs.Internal, "A trial run needs an identifier.")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ns := "pando-trial-" + toLabel(req.TrialID)
	if len(ns) > 63 {
		ns = strings.TrimRight(ns[:63], "-")
	}

	if _, err := a.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   ns,
		Labels: map[string]string{labelManagedBy: managedBy, labelPSAEnforce: "baseline"},
	}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return api.TrialResult{}, errs.Wrap(errs.AdapterFailed, "Could not make a place in the cluster to try the app.", err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer done()
		_ = a.cs.CoreV1().Namespaces().Delete(cleanup, ns, metav1.DeleteOptions{})
	}()
	if err := a.bindAppRole(ctx, ns); err != nil {
		return api.TrialResult{}, err
	}
	if err := a.awaitServiceAccount(ctx, ns); err != nil {
		return api.TrialResult{}, err
	}

	deny := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-in", Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	if _, err := a.cs.NetworkingV1().NetworkPolicies(ns).Create(ctx, deny, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return api.TrialResult{}, errs.Wrap(errs.AdapterFailed, "Could not isolate the app's trial run.", err)
	}

	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v.Reveal()
	}
	if err := a.putSecret(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "trial-env", Namespace: ns},
		Data:       secretData(env),
	}, "the trial's environment"); err != nil {
		return api.TrialResult{}, err
	}
	var pullSecrets []corev1.LocalObjectReference
	if req.PullAuth != nil {
		if err := a.ensurePullSecret(ctx, ns, []api.WorkloadPlan{{Image: req.Image, PullAuth: req.PullAuth}}); err != nil {
			return api.TrialResult{}, err
		}
		pullSecrets = []corev1.LocalObjectReference{{Name: pullSecretName}}
	}

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	for i, p := range req.DeclaredPaths {
		name := "declared-" + strconv.Itoa(i)
		volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: p})
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "trial", Namespace: ns, Labels: map[string]string{labelManagedBy: managedBy}},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			TerminationGracePeriodSeconds: ptr.To(int64(1)),
			ImagePullSecrets:              pullSecrets,
			Volumes:                       volumes,
			Containers: []corev1.Container{
				{
					Name: appContainer, Image: req.Image,
					Command: req.Entrypoint, Args: req.Command, WorkingDir: req.WorkingDir,
					EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "trial-env"},
					}}},
					VolumeMounts: mounts,
				},
				{Name: "observer", Image: a.config.HelperImage, Command: []string{"sleep", "3600"}},
			},
		},
	}
	if a.config.RuntimeClass != "" {
		pod.Spec.RuntimeClassName = ptr.To(a.config.RuntimeClass)
	}
	if _, err := a.cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return api.TrialResult{}, errs.Wrap(errs.AdapterFailed, "Could not start the app's trial run.", err)
	}

	result, err := a.watchTrial(ctx, ns, timeout, req.PullAuth != nil)
	if err != nil {
		return api.TrialResult{}, err
	}
	result.Log = a.trialLog(ctx, ns)
	if req.LogSink != nil && result.Log != "" {
		_, _ = io.WriteString(req.LogSink, result.Log)
	}
	return result, nil
}

// watchTrial polls the trial until it exits, binds a port, or time runs out.
// A node refusing the image for the registry's download limit is an error:
// the trial never ran, and waiting out the clock would report it as working.
func (a *Adapter) watchTrial(ctx context.Context, ns string, timeout time.Duration, signed bool) (api.TrialResult, error) {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var result api.TrialResult
	for {
		p, err := a.cs.CoreV1().Pods(ns).Get(deadline, "trial", metav1.GetOptions{})
		if err == nil {
			if limited := pullLimited(p, signed); limited != nil {
				return result, limited
			}
			for _, s := range p.Status.ContainerStatuses {
				if s.Name != appContainer {
					continue
				}
				if s.State.Running != nil {
					result.Started = true
				}
				if t := s.State.Terminated; t != nil {
					code := int(t.ExitCode)
					result.ExitCode = &code
					result.Started = result.Started || !t.StartedAt.IsZero()
					return result, nil
				}
			}
			if result.Started {
				routable, loopback, ok := a.observePorts(deadline, ns)
				if ok {
					result.LoopbackPorts = loopback
					if len(routable) > 0 {
						result.ObservedPorts = routable
						return result, nil
					}
				}
			}
		}
		if sleep(deadline, a.poll) != nil {
			// Still up when the clock ran out, which is the app working.
			return result, nil
		}
	}
}

// observePorts reads the trial's listening sockets from the observer.
func (a *Adapter) observePorts(ctx context.Context, ns string) (routable, loopback []int, ok bool) {
	var out bytes.Buffer
	err := a.stream(ctx, ns, "trial", "observer", []string{"cat", "/proc/net/tcp", "/proc/net/tcp6"},
		remotecommand.StreamOptions{Stdout: &out, Stderr: io.Discard})
	if err != nil && out.Len() == 0 {
		return nil, nil, false
	}
	routable, loopback = listeningPorts(out.String())
	return routable, loopback, true
}

// trialLog is the app container's output, at most 64 KiB.
func (a *Adapter) trialLog(ctx context.Context, ns string) string {
	rc, err := a.cs.CoreV1().Pods(ns).GetLogs("trial", &corev1.PodLogOptions{
		Container: appContainer, LimitBytes: ptr.To(int64(64 << 10)),
	}).Stream(context.WithoutCancel(ctx))
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	return string(body)
}

// listeningPorts reads LISTEN sockets from /proc/net/tcp and tcp6, split into
// those traffic can arrive on and those bound only to loopback — a common,
// quiet mistake worth naming rather than reporting no port at all.
func listeningPorts(procNetTCP string) (routable, loopback []int) {
	const listenState = "0A"
	seen := map[int]bool{}
	for _, line := range strings.Split(procNetTCP, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != listenState {
			continue
		}
		hexAddr, hexPort, found := strings.Cut(fields[1], ":")
		if !found {
			continue
		}
		port, err := strconv.ParseInt(hexPort, 16, 32)
		if err != nil || port <= 0 || port > 65535 || seen[int(port)] {
			continue
		}
		seen[int(port)] = true
		if isLoopback(hexAddr) {
			loopback = append(loopback, int(port))
		} else {
			routable = append(routable, int(port))
		}
	}
	sort.Ints(routable)
	sort.Ints(loopback)
	return routable, loopback
}

// isLoopback reads a /proc/net local address: 8 little-endian hex digits for
// IPv4, so 127.x.x.x ends in 7F; 32 for IPv6, where ::1 is the one that
// matters.
func isLoopback(hexAddr string) bool {
	switch len(hexAddr) {
	case 8:
		return strings.EqualFold(hexAddr[6:8], "7F")
	case 32:
		return strings.EqualFold(hexAddr, "00000000000000000000000001000000")
	}
	return false
}
