package edge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

type fakeIssuer struct {
	got  *api.CertificateIssue
	out  []api.EdgeCertificate
	err  error
	runs int
}

func (f *fakeIssuer) Ensure(_ context.Context, p *api.CertificateIssue) ([]api.EdgeCertificate, error) {
	f.got, f.runs = p, f.runs+1
	return f.out, f.err
}

// TestR174_TheEdgePassHandsTheRuntimeTheCertificatesPandoIssued asserts the
// join: an edge that asks Pando for certificates gets them in its plan from
// the issuer, in the leader's edge pass, and a failed order is reported
// without taking down what is already served.
func TestR174_TheEdgePassHandsTheRuntimeTheCertificatesPandoIssued(t *testing.T) {
	issue := &api.CertificateIssue{Email: "ops@example.com", Challenge: api.ChallengeHTTP01,
		Orders: []api.CertificateOrder{{Name: "pando-tls-a.example.com", Domains: []string{"a.example.com"}}}}
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}
	routing := &fakeRouting{needs: true, issue: issue}
	s := setup(t, rt, map[string]*fakeRouting{"rte_traefik": routing})
	issuer := &fakeIssuer{out: []api.EdgeCertificate{{Name: "pando-tls-a.example.com", CertPEM: []byte("CERT"), KeyPEM: secret.New("KEY")}}}
	s.Certificates = issuer

	require.NoError(t, s.Reconcile(context.Background()))
	require.Same(t, issue, issuer.got)
	require.Len(t, rt.edges["rte_traefik"].Certificates, 1)

	issuer.err = errs.New(errs.AdapterFailed, "The certificate authority could not issue a certificate for b.example.com.")
	require.NoError(t, s.Reconcile(context.Background()))
	require.Len(t, rt.edges["rte_traefik"].Certificates, 1, "what is held stays in service")
	st, _ := s.Status("rte_traefik")
	require.True(t, st.Running)
	require.Contains(t, st.Message, "b.example.com")
}

// TestR169_AnEdgeAskingForCertificatesWithNoIssuerIsRefused asserts an edge
// is not started serving HTTPS it cannot have.
func TestR169_AnEdgeAskingForCertificatesWithNoIssuerIsRefused(t *testing.T) {
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}
	routing := &fakeRouting{needs: true, issue: &api.CertificateIssue{Email: "ops@example.com"}}
	s := setup(t, rt, map[string]*fakeRouting{"rte_traefik": routing})
	err := s.Reconcile(context.Background())
	require.Error(t, err)
	require.Empty(t, rt.edges)
}
