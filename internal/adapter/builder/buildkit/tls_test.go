package buildkit

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// serveLikeBuildKit listens with the TLS buildkitd builds from --tlscacert,
// --tlscert and --tlskey (cmd/buildkitd serverCredentials): the daemon's
// certificate, and a client certificate required and checked against the CA.
func serveLikeBuildKit(t *testing.T, ca *authority, server leaf) string {
	t.Helper()
	cert, err := tls.X509KeyPair(server.certPEM, server.keyPEM)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca.certPEM()))
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				_, _ = c.Write([]byte("ok"))
				_ = c.Close()
			}()
		}
	}()
	return l.Addr().String()
}

// reads reports whether a client with cfg got a byte from the server, which
// it does only once the server accepted its certificate.
func reads(addr string, cfg *tls.Config) bool {
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return false
	}
	defer func() { _ = c.Close() }()
	b := make([]byte, 2)
	_, err = c.Read(b)
	return err == nil
}

// TestR117_ABuildKitRefusesAClientWithoutPandosCertificate asserts the
// listener a build step can reach serves only a client presenting a
// certificate from the install's build authority: none, one from another
// authority, and the daemon's own server certificate — which a step might read
// — are all refused.
func TestR117_ABuildKitRefusesAClientWithoutPandosCertificate(t *testing.T) {
	ca, err := loadAuthority(t.TempDir())
	require.NoError(t, err)
	name, err := daemonServerName()
	require.NoError(t, err)
	server, err := ca.issueServer(name)
	require.NoError(t, err)
	addr := serveLikeBuildKit(t, ca, server)

	pando, err := ca.clientTLS(name)
	require.NoError(t, err)
	require.True(t, reads(addr, pando), "Pando's own client certificate is served")

	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	require.False(t, reads(addr, &tls.Config{RootCAs: roots, ServerName: name, MinVersion: tls.VersionTLS12}), "no certificate")

	stolen, err := tls.X509KeyPair(server.certPEM, server.keyPEM)
	require.NoError(t, err)
	require.False(t, reads(addr, &tls.Config{RootCAs: roots, ServerName: name, MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{stolen}}), "the daemon's own certificate, presented as a client's")

	other, err := loadAuthority(t.TempDir())
	require.NoError(t, err)
	foreign, err := other.clientTLS(name)
	require.NoError(t, err)
	foreign.RootCAs = roots
	require.False(t, reads(addr, foreign), "a certificate from another authority")

	// And Pando checks the daemon: one with another daemon's name is refused.
	elsewhere, err := ca.clientTLS("d00.buildkit.pando.invalid")
	require.NoError(t, err)
	require.False(t, reads(addr, elsewhere))
}

// The build authority is made once and read back by every later start, and
// by every replica sharing the data directory.
func TestTheBuildAuthorityIsKeptInPandosDataDirectory(t *testing.T) {
	dir := t.TempDir()
	first, err := loadAuthority(dir)
	require.NoError(t, err)
	again, err := loadAuthority(dir)
	require.NoError(t, err)
	require.Equal(t, first.cert.Raw, again.cert.Raw)

	info, err := os.Stat(filepath.Join(dir, buildAuthorityFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the key is Pando's alone")

	require.NoError(t, os.WriteFile(filepath.Join(dir, buildAuthorityFile), []byte("not PEM"), 0o600))
	_, err = loadAuthority(dir)
	require.Error(t, err)

	_, err = parseAuthority(append(first.certPEM(), (&authority{}).mustKeyPEM(t)...))
	require.ErrorContains(t, err, "does not belong")

	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))
	_, err = loadAuthority(filepath.Join(blocked, "data"))
	require.Error(t, err)
}

// mustKeyPEM is a key that belongs to no certificate here.
func (*authority) mustKeyPEM(t *testing.T) []byte {
	t.Helper()
	other, err := newAuthorityPEM()
	require.NoError(t, err)
	_, rest := pemSplit(other)
	return rest
}

// pemSplit is an authority's PEM as its certificate and its key.
func pemSplit(b []byte) (cert, key []byte) {
	for i := 1; i < len(b); i++ {
		if b[i-1] == '\n' && b[i] == '-' && string(b[i:i+16]) == "-----BEGIN PRIVA" {
			return b[:i], b[i:]
		}
	}
	return b, nil
}
