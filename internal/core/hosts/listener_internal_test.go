package hosts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// handshake runs a TLS handshake against a server presenting cert, with the
// client configuration client, and returns the client's error.
func handshake(t *testing.T, cert tls.Certificate, client *tls.Config) error {
	t.Helper()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	server := tls.Server(s, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}})
	go func() { _ = server.Handshake() }()
	return tls.Client(c, client).Handshake()
}

// TestR415_AHostCertificateCannotPoseAsTheClusterListener asserts R-415: a
// joining or joined host takes the cluster listener to be only the
// certificate an authority-key holder issued for the listener, never another
// certificate from the same authority, which every host holds one of.
func TestR415_AHostCertificateCannotPoseAsTheClusterListener(t *testing.T) {
	a := testAuthority(t)
	now := time.Now()
	withCA := func(c tls.Certificate) tls.Certificate {
		c.Certificate = append(c.Certificate, a.Cert.Raw)
		return c
	}

	hostKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	hostDER, err := a.Authorities().IssueHostAt(now, "edge-2", []string{"app"}, &hostKey.PublicKey)
	require.NoError(t, err)
	controlDER, err := a.Authorities().IssueHostAt(now, "ctl-2", []string{"control"}, &hostKey.PublicKey)
	require.NoError(t, err)
	probe, err := a.Authorities().IssueHostClientAt(now, "host_01HQ8AAAAAAAAAAAAAAAAAAAAA")
	require.NoError(t, err)
	collector, err := a.Authorities().IssueCollector()
	require.NoError(t, err)
	proxy, err := a.Authorities().IssueClient()
	require.NoError(t, err)
	agentCertPEM, agentKeyPEM, err := a.Authorities().IssueServer("edge-2")
	require.NoError(t, err)
	agent, err := tls.X509KeyPair(agentCertPEM, agentKeyPEM)
	require.NoError(t, err)

	for what, cert := range map[string]tls.Certificate{
		"an app host's certificate":    {Certificate: [][]byte{hostDER, a.Cert.Raw}, PrivateKey: hostKey},
		"a control host's certificate": {Certificate: [][]byte{controlDER, a.Cert.Raw}, PrivateKey: hostKey},
		"a probe certificate":          withCA(probe),
		"the collector's certificate":  withCA(collector),
		"the proxy's certificate":      withCA(proxy),
		"a host agent's certificate":   withCA(agent),
	} {
		err := handshake(t, cert, PinnedTLS(a.Hash()))
		require.Error(t, err, what)
	}

	listener, err := a.listenerCert([]string{"127.0.0.1"}, now)
	require.NoError(t, err)
	require.NoError(t, handshake(t, listener, PinnedTLS(a.Hash())), "the cluster listener's own certificate")
}
