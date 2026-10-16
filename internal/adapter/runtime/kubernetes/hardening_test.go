package kubernetes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestR240_AnAppsPodsCannotGainPrivilegesAndKeepOnlyTheCapabilitiesTheyNeed
// asserts the Docker adapter's hardening on Kubernetes (finding 8 of the
// 2026-10-16 security assessment): no privilege escalation through a setuid
// binary, every capability dropped but the set an ordinary image starts with,
// and never NET_RAW. A process limit is the kubelet's, per node.
func TestR240_AnAppsPodsCannotGainPrivilegesAndKeepOnlyTheCapabilitiesTheyNeed(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	_, err := a.Apply(ctx, webPlan())
	require.NoError(t, err)

	pods, err := cs.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, pods.Items)
	for _, p := range pods.Items {
		for _, c := range p.Spec.Containers {
			sc := c.SecurityContext
			require.NotNil(t, sc, p.Name)
			require.False(t, *sc.AllowPrivilegeEscalation, p.Name)
			require.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, p.Name)
			require.NotContains(t, sc.Capabilities.Add, corev1.Capability("NET_RAW"), p.Name)
			require.Contains(t, sc.Capabilities.Add, corev1.Capability("NET_BIND_SERVICE"), p.Name)
		}
	}
}

// TestR240_APodFromBeforeTheSecurityPolicyIsReplaced asserts that the policy is
// part of a pod's digest, so a pod created without it does not count as
// current and is replaced on the next apply.
func TestR240_APodFromBeforeTheSecurityPolicyIsReplaced(t *testing.T) {
	w := webPlan().Workloads[0]
	now := planDigest(w, nil, []byte("k"), "", "")
	require.Equal(t, now, planDigest(w, nil, []byte("k"), "", ""), "stable")

	was := workloadSecurityLevel
	t.Cleanup(func() { workloadSecurityLevel = was })
	workloadSecurityLevel = "0"
	require.NotEqual(t, now, planDigest(w, nil, []byte("k"), "", ""),
		"a pod made under another policy does not match its plan")
}
