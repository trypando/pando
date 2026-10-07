package kubernetes

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/trypando/pando/internal/adapter/api"
)

const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 2 1 0000000000000000 100 0 0 10 0
`

// TestR097_TheTrialReadsTheAppsListeningSocketsFromBesideIt asserts R-097 on
// this runtime: ports are observed from a container sharing the app's network
// namespace, a loopback-only port is named as such, and the trial's namespace
// is removed afterwards.
func TestR097_TheTrialReadsTheAppsListeningSocketsFromBesideIt(t *testing.T) {
	a, cs := testAdapter(t, nil)
	var asked []string
	a.stream = func(_ context.Context, ns, pod, container string, cmd []string, opts remotecommand.StreamOptions) error {
		asked = append(asked, container)
		_, _ = io.WriteString(opts.Stdout, procNetTCP)
		return nil
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ctx := context.Background()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			p, err := cs.CoreV1().Pods("pando-trial-trl-1").Get(ctx, "trial", metav1.GetOptions{})
			if err != nil || p.Status.Phase == corev1.PodRunning {
				continue
			}
			p.Status = running(true)
			_, _ = cs.CoreV1().Pods(p.Namespace).UpdateStatus(ctx, p, metav1.UpdateOptions{})
		}
	}()

	result, err := a.Trial(context.Background(), api.TrialRequest{TrialID: "trl_1", Image: "example/app", Timeout: 5 * time.Second})
	require.NoError(t, err)
	require.True(t, result.Started)
	require.Nil(t, result.ExitCode, "still running: the healthy outcome")
	require.Equal(t, []int{3000}, result.ObservedPorts)
	require.Equal(t, []int{8080}, result.LoopbackPorts)
	require.Contains(t, asked, "observer")

	_, err = cs.CoreV1().Namespaces().Get(context.Background(), "pando-trial-trl-1", metav1.GetOptions{})
	require.Error(t, err, "nothing of the trial is left running")
}

// TestR084_ExecRunsInTheWorkloadsPodAndReportsItsExitCode asserts an exec
// session reaches the workload's running pod and carries its exit status.
func TestR084_ExecRunsInTheWorkloadsPodAndReportsItsExitCode(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)
	pod := listPods(t, cs, testNS, "web")[0]
	setStatus(t, cs, pod, running(true))

	var gotPod string
	var gotCmd []string
	a.stream = func(_ context.Context, ns, p, container string, cmd []string, opts remotecommand.StreamOptions) error {
		gotPod, gotCmd = p, cmd
		_, _ = io.WriteString(opts.Stdout, "hello\n")
		return utilexec.CodeExitError{Code: 3}
	}
	s, err := a.Exec(ctx, api.WorkloadRef{BundleID: testBundle, Workload: "web"},
		api.ExecRequest{Command: []string{"sh"}, Env: map[string]string{"TERM": "xterm"}})
	require.NoError(t, err)
	out, _ := io.ReadAll(s)
	require.Equal(t, "hello\n", string(out))
	code, ok := s.ExitCode()
	require.True(t, ok)
	require.Equal(t, 3, code)
	require.Equal(t, pod.Name, gotPod)
	require.Equal(t, []string{"env", "TERM=xterm", "sh"}, gotCmd)
	require.NoError(t, s.Close())
}

// TestR212_AVolumeIsBackedUpAndRestoredThroughAHelperBesideIt asserts the
// snapshot reads the claim read-only and the restore writes it, each from a
// helper pod that is removed afterwards.
func TestR212_AVolumeIsBackedUpAndRestoredThroughAHelperBesideIt(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	h, err := a.CreateVolume(ctx, api.VolumeRequest{VolumeID: "vol_01DATA", BundleID: testBundle})
	require.NoError(t, err)

	var readOnly []bool
	stop := make(chan struct{})
	defer close(stop)
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
					readOnly = append(readOnly, p.Spec.Containers[0].VolumeMounts[0].ReadOnly)
					p.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.9"}
					_, _ = cs.CoreV1().Pods(testNS).UpdateStatus(ctx, &p, metav1.UpdateOptions{})
				}
			}
		}
	}()

	a.stream = func(_ context.Context, _, _, _ string, cmd []string, opts remotecommand.StreamOptions) error {
		if opts.Stdout != nil {
			_, _ = io.WriteString(opts.Stdout, "TAR")
		}
		if opts.Stdin != nil {
			_, _ = io.ReadAll(opts.Stdin)
		}
		return nil
	}
	var buf bytes.Buffer
	require.NoError(t, a.SnapshotVolume(ctx, h, &buf))
	require.Equal(t, "TAR", buf.String())
	require.NoError(t, a.RestoreVolume(ctx, h, bytes.NewBufferString("TAR")))
	require.Equal(t, []bool{true, false}, readOnly, "read-only to back up, writable to restore")

	left, err := cs.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{LabelSelector: labelRole + "=" + roleVolumeHelper})
	require.NoError(t, err)
	require.Empty(t, left.Items)
}
