package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// TestR084_ATerminalSessionCarriesInputAndResizes asserts the TTY half of an
// exec session: what is written reaches the command's stdin, resizes reach
// the stream as terminal sizes, stderr is the terminal's (not a second
// stream), the size queue ends with the session, and a session ended by
// something other than the command's exit reports no exit code.
func TestR084_ATerminalSessionCarriesInputAndResizes(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)
	setStatus(t, cs, listPods(t, cs, testNS, "web")[0], running(true))

	var (
		mu       sync.Mutex
		sizes    []remotecommand.TerminalSize
		input    string
		stderrOK bool
		queueEnd = make(chan struct{})
	)
	a.stream = func(_ context.Context, _, _, _ string, _ []string, opts remotecommand.StreamOptions) error {
		stderrOK = opts.Stderr == nil && opts.Tty
		go func() {
			for {
				size := opts.TerminalSizeQueue.Next()
				if size == nil {
					close(queueEnd)
					return
				}
				mu.Lock()
				sizes = append(sizes, *size)
				mu.Unlock()
			}
		}()
		buf := make([]byte, 5)
		_, _ = io.ReadFull(opts.Stdin, buf)
		input = string(buf)
		require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(sizes) == 1 }, time.Second, time.Millisecond)
		_, _ = io.WriteString(opts.Stdout, "bye\n")
		return errors.New("the connection to the kubelet was lost")
	}

	s, err := a.Exec(ctx, api.WorkloadRef{BundleID: testBundle, Workload: "web"}, api.ExecRequest{Command: []string{"sh"}, TTY: true})
	require.NoError(t, err)
	require.NoError(t, s.Resize(40, 120))
	_, err = s.Write([]byte("exit\n"))
	require.NoError(t, err)
	out, _ := io.ReadAll(s)
	require.Equal(t, "bye\n", string(out))
	<-queueEnd

	require.Equal(t, "exit\n", input)
	require.Equal(t, []remotecommand.TerminalSize{{Width: 120, Height: 40}}, sizes)
	require.True(t, stderrOK, "a terminal has one output stream")
	_, exited := s.ExitCode()
	require.False(t, exited, "a lost connection is not the command's exit")
	require.NoError(t, s.Close())

	// A full queue drops a size rather than blocking the terminal.
	full := &execSession{sizes: make(chan *remotecommand.TerminalSize, 1), done: make(chan struct{})}
	require.NoError(t, full.Resize(1, 1))
	require.NoError(t, full.Resize(2, 2))
	require.Len(t, full.sizes, 1)
}

// TestR084_ExecReachesOnlyARunningWorkload asserts that exec into a workload
// with no running pod is a not-found error naming it, and that a failure to
// read the pods is reported rather than taken as "nothing running".
func TestR084_ExecReachesOnlyARunningWorkload(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)
	setStatus(t, cs, listPods(t, cs, testNS, "web")[0], exited(1))

	_, err = a.Exec(ctx, api.WorkloadRef{BundleID: testBundle, Workload: "web"}, api.ExecRequest{Command: []string{"sh"}})
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	require.ErrorContains(t, err, `There is nothing running called "web".`)

	_, err = a.Exec(ctx, api.WorkloadRef{BundleID: testBundle, Workload: "nope"}, api.ExecRequest{Command: []string{"sh"}})
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	failOn(cs, "list", "pods")
	_, err = a.Exec(ctx, api.WorkloadRef{BundleID: testBundle, Workload: "web"}, api.ExecRequest{Command: []string{"sh"}})
	require.ErrorIs(t, err, errAPI)
}

// helperKubelet starts every volume helper pod, recording each.
func helperKubelet(t *testing.T, cs *fake.Clientset) func() []corev1.Pod {
	t.Helper()
	ctx := context.Background()
	var (
		mu   sync.Mutex
		seen []corev1.Pod
	)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			list, err := cs.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{LabelSelector: labelRole + "=" + roleVolumeHelper})
			if err != nil {
				continue
			}
			for _, p := range list.Items {
				if p.Status.Phase != corev1.PodRunning {
					mu.Lock()
					seen = append(seen, p)
					mu.Unlock()
					p.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.9"}
					_, _ = cs.CoreV1().Pods(testNS).UpdateStatus(ctx, &p, metav1.UpdateOptions{})
				}
			}
		}
	}()
	return func() []corev1.Pod {
		mu.Lock()
		defer mu.Unlock()
		return append([]corev1.Pod(nil), seen...)
	}
}

// TestR212_ABackupHelperLandsOnTheNodeHoldingTheVolume asserts that while the
// app runs, the helper is placed on the node its pod mounts the claim from (a
// ReadWriteOnce claim attaches to one node), and that a failed tar is reported
// with what tar said.
func TestR212_ABackupHelperLandsOnTheNodeHoldingTheVolume(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)
	db := listPods(t, cs, testNS, "db")[0]
	db.Spec.NodeName = "node-b"
	_, err = cs.CoreV1().Pods(testNS).Update(ctx, &db, metav1.UpdateOptions{})
	require.NoError(t, err)
	setStatus(t, cs, listPods(t, cs, testNS, "db")[0], running(true))
	helpers := helperKubelet(t, cs)

	a.stream = func(_ context.Context, _, _, _ string, cmd []string, opts remotecommand.StreamOptions) error {
		_, _ = io.WriteString(opts.Stderr, "tar: short read\n")
		return fmt.Errorf("command terminated with exit code 2")
	}
	h := api.VolumeHandle{VolumeID: "vol_01DATA", Handle: testNS + volumeHandleSep + pvcName("vol_01DATA")}
	err = a.SnapshotVolume(ctx, h, io.Discard)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.ErrorContains(t, err, "tar: short read")
	err = a.RestoreVolume(ctx, h, bytes.NewBufferString("TAR"))
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.ErrorContains(t, err, "Could not restore")

	seen := helpers()
	require.NotEmpty(t, seen)
	for _, p := range seen {
		require.Equal(t, map[string]string{"kubernetes.io/hostname": "node-b"}, p.Spec.NodeSelector)
	}
}

// TestR212_ABackupOfStorageThatIsNotThereIsRefused asserts the refusals made
// before a helper is started.
func TestR212_ABackupOfStorageThatIsNotThereIsRefused(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)

	err := a.SnapshotVolume(ctx, api.VolumeHandle{Handle: "not-a-handle"}, io.Discard)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	err = a.SnapshotVolume(ctx, api.VolumeHandle{Handle: testNS + volumeHandleSep + "pando-vol-gone"}, io.Discard)
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	require.ErrorContains(t, err, "does not exist")

	failOn(cs, "get", "persistentvolumeclaims")
	err = a.RestoreVolume(ctx, api.VolumeHandle{Handle: testNS + volumeHandleSep + "pando-vol-gone"}, bytes.NewReader(nil))
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.ErrorIs(t, err, errAPI)
}

// TestR084_ExecGoesThroughTheAPIServersExecSubresource asserts the stream
// that is not replaced in tests: it needs the adapter configured, and posts
// to the pod's exec subresource, reporting what the API server refused.
func TestR084_ExecGoesThroughTheAPIServersExecSubresource(t *testing.T) {
	ctx := context.Background()
	unset := New()
	err := unset.apiStream(ctx, "ns", "pod", "app", []string{"sh"}, remotecommand.StreamOptions{})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))

	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		http.Error(w, "exec is forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfig, fmt.Appendf(nil, `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: %q}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: ctx, context: {cluster: c, user: u}}]
current-context: ctx
`, srv.URL), 0o600))
	cfg := fmt.Sprintf(`{"kubeconfig": %q, "pod_cidr": "10.244.0.0/16", "service_cidr": "10.96.0.0/12"}`, kubeconfig)
	a := New()
	require.NoError(t, a.Configure(ctx, []byte(cfg)))

	var out bytes.Buffer
	err = a.apiStream(ctx, "pando-app", "web-1", appContainer, []string{"ls"}, remotecommand.StreamOptions{Stdout: &out})
	require.Error(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, paths)
	require.Contains(t, paths[0], "/api/v1/namespaces/pando-app/pods/web-1/exec")
	require.Contains(t, paths[0], "command=ls")
	require.Contains(t, paths[0], "container="+appContainer)
}

// TestR254_TheKubernetesRuntimeIsConfiguredFromAKubeconfigOrTheCluster asserts
// Configure: a kubeconfig file is read with the context named, defaults are
// filled, and each way it cannot connect is a message that says what to set.
func TestR254_TheKubernetesRuntimeIsConfiguredFromAKubeconfigOrTheCluster(t *testing.T) {
	ctx := context.Background()
	a := New()
	require.Equal(t, Kind, a.Kind())
	require.Equal(t, api.CategoryRuntime, a.Category())

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- {name: one, cluster: {server: "https://one.example.com:6443"}}
- {name: two, cluster: {server: "https://two.example.com:6443"}}
users: [{name: u, user: {token: t}}]
contexts:
- {name: first, context: {cluster: one, user: u}}
- {name: second, context: {cluster: two, user: u}}
current-context: first
`), 0o600))
	require.NoError(t, a.Configure(ctx, fmt.Appendf(nil,
		`{"kubeconfig": %q, "context": "second", "pod_cidr": "10.244.0.0/16", "service_cidr": "10.96.0.0/12"}`, kubeconfig)))
	require.Equal(t, "https://two.example.com:6443", a.rest.Host, "the context named, not the file's current one")
	require.Equal(t, defaultPandoNamespace, a.config.PandoNamespace)
	require.Equal(t, defaultHelperImage, a.config.HelperImage)
	require.NotNil(t, a.mc, "metrics are read through the same connection")
	require.NotNil(t, a.stream)

	err := New().Configure(ctx, []byte(`{not json`))
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))

	err = New().Configure(ctx, []byte(`{"pod_cidr": "10.244.0.0/16"}`))
	require.Error(t, err, "settings that fail validation are refused before connecting")

	missing := filepath.Join(t.TempDir(), "nope")
	err = New().Configure(ctx, fmt.Appendf(nil, `{"kubeconfig": %q, "pod_cidr": "10.244.0.0/16", "service_cidr": "10.96.0.0/12"}`, missing))
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "clear the kubeconfig setting")

	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	err = New().Configure(ctx, []byte(`{"pod_cidr": "10.244.0.0/16", "service_cidr": "10.96.0.0/12"}`))
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.ErrorContains(t, err, "not running inside a Kubernetes cluster")
	require.Contains(t, errs.As(err).Remedy, "deploy/kubernetes")
}
