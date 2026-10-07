package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"
	"k8s.io/utils/ptr"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// apiStream runs a command through the API server's pods/exec subresource:
// the WebSocket protocol (Kubernetes 1.30 and later), falling back to SPDY on
// an older API server.
func (a *Adapter) apiStream(ctx context.Context, namespace, pod, container string, cmd []string, opts remotecommand.StreamOptions) error {
	if a.rest == nil {
		return errs.New(errs.AdapterUnavailable, "The Kubernetes runtime has not been set up.")
	}
	req := a.cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     opts.Stdin != nil,
			Stdout:    opts.Stdout != nil,
			Stderr:    opts.Stderr != nil,
			TTY:       opts.Tty,
		}, scheme.ParameterCodec)

	ws, err := remotecommand.NewWebSocketExecutor(a.rest, http.MethodGet, req.URL().String())
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(a.rest, http.MethodPost, req.URL())
	if err != nil {
		return err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	return exec.StreamWithContext(ctx, opts)
}

// Exec opens a session in a workload's running pod. Core has already
// authorized it and written the audit event (R-084, R-228).
func (a *Adapter) Exec(ctx context.Context, ref api.WorkloadRef, req api.ExecRequest) (api.ExecSession, error) {
	ns := namespaceFor(ref.BundleID)
	pods, err := a.workloadPods(ctx, ns, ref.Workload)
	if err != nil {
		return nil, err
	}
	p := newest(pods)
	if p == nil || p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
		return nil, errs.Newf(errs.NotFound, "There is nothing running called %q.", ref.Workload)
	}

	// The exec subresource takes no environment; env(1) sets it.
	cmd := req.Command
	if len(req.Env) > 0 {
		keys := make([]string, 0, len(req.Env))
		for k := range req.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		withEnv := []string{"env"}
		for _, k := range keys {
			withEnv = append(withEnv, k+"="+req.Env[k])
		}
		cmd = append(withEnv, cmd...)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &execSession{in: inW, out: outR, sizes: make(chan *remotecommand.TerminalSize, 4), done: make(chan struct{})}
	opts := remotecommand.StreamOptions{Stdin: inR, Stdout: outW, Tty: req.TTY, TerminalSizeQueue: s}
	if !req.TTY {
		opts.Stderr = outW
	}
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	go func() {
		err := a.stream(streamCtx, ns, p.Name, appContainer, cmd, opts)
		s.finish(err)
		_ = outW.CloseWithError(io.EOF)
		_ = inR.Close()
	}()
	return s, nil
}

// execSession is an exec stream: stdin in, stdout (and stderr, without a TTY)
// out.
type execSession struct {
	in     *io.PipeWriter
	out    *io.PipeReader
	sizes  chan *remotecommand.TerminalSize
	cancel context.CancelFunc

	once   sync.Once
	done   chan struct{}
	mu     sync.Mutex
	code   int
	exited bool
}

func (s *execSession) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *execSession) Write(p []byte) (int, error) { return s.in.Write(p) }

func (s *execSession) Close() error {
	_ = s.in.Close()
	s.cancel()
	return s.out.Close()
}

// Resize queues a terminal size for the stream.
func (s *execSession) Resize(rows, cols uint16) error {
	select {
	case s.sizes <- &remotecommand.TerminalSize{Height: rows, Width: cols}:
	default:
		// A full queue already has a newer size coming.
	}
	return nil
}

// Next is remotecommand's TerminalSizeQueue: nil ends it.
func (s *execSession) Next() *remotecommand.TerminalSize {
	select {
	case size := <-s.sizes:
		return size
	case <-s.done:
		return nil
	}
}

func (s *execSession) ExitCode() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code, s.exited
}

func (s *execSession) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var exit utilexec.ExitError
	switch {
	case err == nil:
		s.code, s.exited = 0, true
	case errors.As(err, &exit):
		s.code, s.exited = exit.ExitStatus(), true
	}
	s.once.Do(func() { close(s.done) })
}

// --- volume backups (R-212) ------------------------------------------------

// SnapshotVolume writes a volume's contents as a tar stream, from a helper pod
// with the claim mounted read-only.
func (a *Adapter) SnapshotVolume(ctx context.Context, h api.VolumeHandle, dst io.Writer) error {
	return a.withHelper(ctx, h, true, func(ns, pod string) error {
		var stderr bytes.Buffer
		err := a.stream(ctx, ns, pod, "helper", []string{"tar", "-cf", "-", "-C", volumeMountInPod, "."},
			remotecommand.StreamOptions{Stdout: dst, Stderr: &stderr})
		if err != nil {
			return errs.Wrap(errs.AdapterFailed, "Could not back up the app's storage.", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String())))
		}
		return nil
	})
}

// RestoreVolume replaces a volume's contents with a tar stream SnapshotVolume
// wrote. In place only (R-206): the claim is found by the app's identity in
// the handle, so it is the app recreated from the same spec.
func (a *Adapter) RestoreVolume(ctx context.Context, h api.VolumeHandle, src io.Reader) error {
	return a.withHelper(ctx, h, false, func(ns, pod string) error {
		var stderr bytes.Buffer
		script := fmt.Sprintf("rm -rf %[1]s/..?* %[1]s/.[!.]* %[1]s/* 2>/dev/null; exec tar -xf - -C %[1]s", volumeMountInPod)
		err := a.stream(ctx, ns, pod, "helper", []string{"sh", "-c", script},
			remotecommand.StreamOptions{Stdin: src, Stderr: &stderr})
		if err != nil {
			return errs.Wrap(errs.AdapterFailed, "Could not restore the app's storage.", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String())))
		}
		return nil
	})
}

// withHelper runs fn against a helper pod with the volume mounted, and removes
// the pod afterwards.
//
// A ReadWriteOnce claim attaches to one node, so while the app runs the helper
// must land beside it: it is given affinity to the app's pods that mount the
// claim.
func (a *Adapter) withHelper(ctx context.Context, h api.VolumeHandle, readOnly bool, fn func(ns, pod string) error) error {
	ns, claim, ok := strings.Cut(h.Handle, volumeHandleSep)
	if !ok || ns == "" || claim == "" {
		return errs.Newf(errs.ValidInvalid, "%q is not storage the Kubernetes runtime made.", h.Handle)
	}
	if _, err := a.cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, claim, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return errs.Newf(errs.NotFound, "The storage %s does not exist in the cluster.", h.Handle)
		}
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the app's storage.", err)
	}

	limits := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(helperCPUMillis, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(helperMemory, resource.BinarySI),
	}
	name := "pando-volume-helper-" + toLabel(claim)
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{labelManagedBy: managedBy, labelRole: roleVolumeHelper},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			TerminationGracePeriodSeconds: ptr.To(int64(1)),
			Containers: []corev1.Container{{
				Name: "helper", Image: a.config.HelperImage,
				Command:      []string{"sleep", "3600"},
				Resources:    corev1.ResourceRequirements{Limits: limits, Requests: limits.DeepCopy()},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: volumeMountInPod, ReadOnly: readOnly}},
			}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim, ReadOnly: readOnly},
			}}},
		},
	}
	if node := a.nodeMounting(ctx, ns, claim); node != "" {
		pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": node}
	}

	pods := a.cs.CoreV1().Pods(ns)
	_ = pods.Delete(ctx, name, metav1.DeleteOptions{})
	if err := a.waitGone(ctx, ns, name); err != nil {
		return err
	}
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not start the storage helper.", err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer done()
		_ = pods.Delete(cleanup, name, metav1.DeleteOptions{})
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := a.waitForPodIP(waitCtx, ns, name); err != nil {
		return errs.Wrap(errs.AdapterFailed, "The storage helper did not start.", err)
	}
	return fn(ns, name)
}

// nodeMounting is the node of a running app pod that mounts the claim, so
// the helper can share its ReadWriteOnce attachment. Empty when none runs.
func (a *Adapter) nodeMounting(ctx context.Context, ns, claim string) string {
	pods, err := a.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedBy})
	if err != nil {
		return ""
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" || p.Labels[labelRole] != "" || finished(&p) {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
				return p.Spec.NodeName
			}
		}
	}
	return ""
}
