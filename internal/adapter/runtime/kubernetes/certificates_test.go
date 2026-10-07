package kubernetes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/secret"
)

// TestR174_IssuedCertificatesAreTLSSecretsEveryEdgeReplicaReads asserts the
// runtime's half of the one issuer: what Pando issued becomes kubernetes.io/tls
// Secrets in the edge's namespace under the names the routes use, a
// certificate dropped from the plan is removed, and removing the edge removes
// its certificates.
func TestR174_IssuedCertificatesAreTLSSecretsEveryEdgeReplicaReads(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	plan := api.EdgePlan{
		Name: "rte_traefik", Image: "traefik:v3.2", ReadsRoutesFrom: api.EdgeConfigKubernetesAPI,
		Certificates: []api.EdgeCertificate{
			{Name: "pando-tls-a.example.com", CertPEM: []byte("CERT A"), KeyPEM: secret.New("KEY A")},
			{Name: "pando-tls-wildcard", CertPEM: []byte("CERT W"), KeyPEM: secret.New("KEY W")},
		},
	}
	require.NoError(t, a.ApplyEdge(ctx, plan))

	s, err := cs.CoreV1().Secrets("pando-edge").Get(ctx, "pando-tls-a.example.com", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, corev1.SecretTypeTLS, s.Type)
	require.Equal(t, "CERT A", string(s.Data[corev1.TLSCertKey]))
	require.Equal(t, "KEY A", string(s.Data[corev1.TLSPrivateKeyKey]))

	d, err := cs.AppsV1().Deployments("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.NoError(t, err)
	digest := d.Annotations[annoEdgeDigest]

	plan.Certificates = plan.Certificates[1:]
	plan.Certificates[0].CertPEM = []byte("CERT W2")
	require.NoError(t, a.ApplyEdge(ctx, plan))
	_, err = cs.CoreV1().Secrets("pando-edge").Get(ctx, "pando-tls-a.example.com", metav1.GetOptions{})
	require.Error(t, err, "a certificate no longer asked for is removed")
	w, err := cs.CoreV1().Secrets("pando-edge").Get(ctx, "pando-tls-wildcard", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "CERT W2", string(w.Data[corev1.TLSCertKey]), "a renewal replaces the Secret")
	d, err = cs.AppsV1().Deployments("pando-edge").Get(ctx, "pando-edge-rte-traefik", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, digest, d.Annotations[annoEdgeDigest], "a renewal does not restart the edge: Traefik reloads Secrets")

	require.NoError(t, a.RemoveEdge(ctx, "rte_traefik"))
	_, err = cs.CoreV1().Secrets("pando-edge").Get(ctx, "pando-tls-wildcard", metav1.GetOptions{})
	require.Error(t, err)
}
