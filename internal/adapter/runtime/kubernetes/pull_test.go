package kubernetes

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

const kubeletHubLimit = `failed to pull and unpack image "docker.io/library/nginx:1.27": failed to copy: httpReadSeeker: ` +
	`failed open: unexpected status code https://registry-1.docker.io/v2/library/nginx/manifests/sha256:abc: ` +
	`429 Too Many Requests - Server message: toomanyrequests: You have reached your unauthenticated pull rate limit.`

func pullWaiting(reason, message string) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  appContainer,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
		}},
	}
}

// TestR105_ANodesPullRefusedForTheDownloadLimitEndsTheTrial asserts R-105 on
// Kubernetes: a trial whose image a node could not pull for Docker Hub's limit
// ends with that said, rather than waiting out its clock and being reported
// as an app that works.
func TestR105_ANodesPullRefusedForTheDownloadLimitEndsTheTrial(t *testing.T) {
	a, cs := testAdapter(t, nil)
	trialKubelet(t, cs, "pando-trial-trl-3", pullWaiting("ImagePullBackOff", kubeletHubLimit))

	start := time.Now()
	_, err := a.Trial(context.Background(), api.TrialRequest{TrialID: "trl_3", Image: "nginx:1.27", Timeout: 10 * time.Second})
	e := errs.As(err)
	require.NotNil(t, e)
	require.Equal(t, errs.AdapterRegistryRateLimited, e.Code)
	require.Contains(t, e.Message, "Docker Hub is limiting how many images this server may download")
	require.Contains(t, e.Message, "nginx:1.27")
	require.Contains(t, e.Remedy, "Add a registry credential to the app")
	require.Less(t, time.Since(start), 5*time.Second)

	_, err = cs.CoreV1().Namespaces().Get(context.Background(), "pando-trial-trl-3", metav1.GetOptions{})
	require.Error(t, err, "the trial's namespace is removed")
}

// A pull that failed for another reason is not called a download limit.
func TestOnlyADownloadLimitIsReadFromAPullFailure(t *testing.T) {
	p := &corev1.Pod{Status: pullWaiting("ErrImagePull", "manifest unknown")}
	require.NoError(t, pullLimited(p, false))
	p = &corev1.Pod{Status: pullWaiting("ContainerCreating", kubeletHubLimit)}
	require.NoError(t, pullLimited(p, false))

	p = &corev1.Pod{
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: appContainer, Image: "ghcr.io/acme/web:1"}}},
		Status: pullWaiting("ErrImagePull", "429 Too Many Requests"),
	}
	err := pullLimited(p, true)
	require.Equal(t, errs.AdapterRegistryRateLimited, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "The registry ghcr.io")
	require.Contains(t, errs.As(err).Remedy, "The app's registry credential was used")
}

// Apply reports the limit when it sees the pod refused, in the same words.
func TestR105_ApplySaysWhenANodeWasRefusedTheImage(t *testing.T) {
	a, _ := testAdapter(t, nil)
	a.scheduleWait = 5 * time.Second
	scheduler(t, a, func(p *corev1.Pod) {
		p.Spec.NodeName = "node-a"
		p.Status = pullWaiting("ErrImagePull", kubeletHubLimit)
	})
	plan := webPlan()
	plan.Workloads[1].PullAuth = &api.RegistryAuth{Registry: "index.docker.io", Username: "bot", Password: secret.New("pat")}
	_, err := a.Apply(context.Background(), plan)
	require.Equal(t, errs.AdapterRegistryRateLimited, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "paid Docker Hub plan")
}
