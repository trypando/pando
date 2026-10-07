package kubernetes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR174_AnEdgeThatCannotRunOnKubernetesIsRefused asserts the refusals made
// before anything is written: storage of its own, which replicas cannot
// share, and a proxy alias that cannot name a Service.
func TestR174_AnEdgeThatCannotRunOnKubernetesIsRefused(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)

	err := a.ApplyEdge(ctx, api.EdgePlan{Name: "rte_traefik", Image: "traefik:v3.2", Mounts: []api.EdgeMount{{Path: "/data", Volume: "acme"}}})
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.ErrorContains(t, err, "storage of its own")

	err = a.ApplyEdge(ctx, api.EdgePlan{Name: "rte_traefik", Image: "traefik:v3.2", ProxyAlias: "Pando_Proxy"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "http://pando-proxy:8080")

	deployments, err := cs.AppsV1().Deployments("pando-edge").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, deployments.Items)

	volumes, err := a.EdgeVolumes(ctx)
	require.NoError(t, err)
	require.Empty(t, volumes, "an edge on Kubernetes has no storage to back up")

	state, err := a.ObserveEdge(ctx, "rte_never_applied")
	require.NoError(t, err)
	require.False(t, state.Present)
}

// TestR174_TheEdgeIsPutBackAsPlanned asserts what an edge pass repairs and
// what it carries: a certificate Secret of the wrong type is replaced, a proxy
// alias changed by hand is pointed back at Pando, the edge's settings reach
// it as a Secret and a changed one rolls the Deployment, UDP ports are
// published as UDP, and an edge with no ports loses its Service.
func TestR174_TheEdgeIsPutBackAsPlanned(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, func(c *Config) { c.APIServerCIDR = "172.18.0.2/32" })
	plan := edgePlan()
	plan.Env = map[string]secret.Value{"CF_DNS_API_TOKEN": secret.New("token-1")}
	plan.Ports = append(plan.Ports, api.EdgePort{Host: 443, Container: 8443, Protocol: "udp"})

	_, err := cs.CoreV1().Secrets("pando-edge").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pando-tls-a.example.com", Namespace: "pando-edge"},
		Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{"stale": []byte("x")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, a.ApplyEdge(ctx, plan))

	cert, err := cs.CoreV1().Secrets("pando-edge").Get(ctx, "pando-tls-a.example.com", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, corev1.SecretTypeTLS, cert.Type, "a Secret's type cannot change, so it is replaced")
	require.Equal(t, "CERT", string(cert.Data[corev1.TLSCertKey]))

	env, err := cs.CoreV1().Secrets("pando-edge").Get(ctx, "pando-edge-rte-traefik-env", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "token-1", string(env.Data["CF_DNS_API_TOKEN"]))

	d, err := cs.AppsV1().Deployments("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, corev1.ProtocolUDP, d.Spec.Template.Spec.Containers[0].Ports[2].Protocol)
	digest := d.Annotations[annoEdgeDigest]
	svc, err := cs.CoreV1().Services("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, corev1.ProtocolUDP, svc.Spec.Ports[2].Protocol)
	require.Equal(t, "udp-8443", svc.Spec.Ports[2].Name)

	policy, err := cs.NetworkingV1().NetworkPolicies("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.NoError(t, err)
	last := policy.Spec.Egress[len(policy.Spec.Egress)-1]
	require.Equal(t, "172.18.0.2/32", last.To[0].IPBlock.CIDR, "only the edge may reach the API server")

	alias, err := cs.CoreV1().Services("pando-edge").Get(ctx, "pando-proxy", metav1.GetOptions{})
	require.NoError(t, err)
	alias.Spec.ExternalName = "somewhere.else.example.com"
	_, err = cs.CoreV1().Services("pando-edge").Update(ctx, alias, metav1.UpdateOptions{})
	require.NoError(t, err)

	plan.Env["CF_DNS_API_TOKEN"] = secret.New("token-2")
	plan.Ports = nil
	require.NoError(t, a.ApplyEdge(ctx, plan))
	alias, err = cs.CoreV1().Services("pando-edge").Get(ctx, "pando-proxy", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "pando.pando.svc.cluster.local", alias.Spec.ExternalName)
	d, err = cs.AppsV1().Deployments("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotEqual(t, digest, d.Annotations[annoEdgeDigest], "a changed setting rolls the edge")
	require.NotContains(t, d.Annotations[annoEdgeDigest], "token", "values are hashed, never stored")
	_, err = cs.CoreV1().Services("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.Error(t, err, "an edge with no ports publishes none")

	failOn(cs, "delete", "services")
	_, err = cs.CoreV1().Services("pando-edge").Create(ctx, svc, metav1.CreateOptions{})
	require.NoError(t, err)
	err = a.ApplyEdge(ctx, plan)
	require.ErrorIs(t, err, errAPI)
	require.ErrorContains(t, err, "Could not remove the edge's Service")
}
