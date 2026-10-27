package hosts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/keyfile"
)

// hostCertRenewBefore is how long before it expires a control host's own
// certificate is issued again, at its next start.
const hostCertRenewBefore = 30 * 24 * time.Hour

// ControlHostCert is the certificate a control host answers the cluster's
// liveness checks with (R-421), and its fingerprint, which ControlPeers
// records as the host's cert_sha256. It is host.crt and host.key in dir: what
// a joined control host was issued at its join. When they are missing,
// unreadable, issued for another name or due for renewal, a new one is
// issued from the authority, which a control host holds, and written there:
// the install's first host, which never joined, gets its own this way, and
// keeps it across restarts so its pin does not change at each start.
func ControlHostCert(dir string, a *Authority, name string, roles []string, now time.Time) (tls.Certificate, string, error) {
	certPath, keyPath := filepath.Join(dir, HostCertFile), filepath.Join(dir, HostKeyFile)
	if cert, ok := usableHostCert(certPath, keyPath, name, now); ok {
		return cert, hostagent.Fingerprint256(cert.Certificate[0]), nil
	}
	if err := issueHostCert(dir, certPath, keyPath, a, name, roles); err != nil {
		return tls.Certificate{}, "", err
	}
	// Read back as of now: the authority issues by the real clock.
	cert, ok := usableHostCert(certPath, keyPath, name, time.Now())
	if !ok {
		return tls.Certificate{}, "", fmt.Errorf("the certificate written to %s cannot be read back", certPath)
	}
	return cert, hostagent.Fingerprint256(cert.Certificate[0]), nil
}

// issueHostCert issues the host's certificate for a new key and writes both.
func issueHostCert(dir, certPath, keyPath string, a *Authority, name string, roles []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := hostagent.Authorities{{Cert: a.Cert, Key: a.Key}}.IssueHost(name, roles, &key.PublicKey)
	if err != nil {
		return fmt.Errorf("could not issue this host's certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("could not create %s (server.cluster_dir) for this host's certificate: %w", dir, err)
	}
	// The key first: a crash between the two leaves a pair that does not
	// match, which the next start issues again.
	for _, f := range []struct {
		path  string
		block *pem.Block
	}{
		{keyPath, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}},
		{certPath, &pem.Block{Type: "CERTIFICATE", Bytes: der}},
	} {
		if err := keyfile.Replace(f.path, pem.EncodeToMemory(f.block)); err != nil {
			return fmt.Errorf("could not write this host's certificate to %s (server.cluster_dir): %w", f.path, err)
		}
	}
	return nil
}

// usableHostCert reads the host's certificate pair, and reports whether it is
// for name and not due for renewal.
func usableHostCert(certPath, keyPath, name string, now time.Time) (tls.Certificate, bool) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil || len(cert.Certificate) == 0 {
		return tls.Certificate{}, false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || !slices.Contains(leaf.DNSNames, hostagent.ServerName(name)) || now.Add(hostCertRenewBefore).After(leaf.NotAfter) {
		return tls.Certificate{}, false
	}
	cert.Leaf = leaf
	return cert, true
}
