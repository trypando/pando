package hosts

import (
	"context"
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
	if err := issueHostCert(dir, certPath, keyPath, a, name, roles, now); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, ok := usableHostCert(certPath, keyPath, name, now)
	if !ok {
		return tls.Certificate{}, "", fmt.Errorf("the certificate written to %s cannot be read back", certPath)
	}
	return cert, hostagent.Fingerprint256(cert.Certificate[0]), nil
}

// RenewingControlHostCert is ControlHostCert held by a running control host,
// issued again, and written over the old one, once it is due (R-421): the
// cluster listener answers pings with what it holds at each handshake.
func RenewingControlHostCert(dir string, a *Authority, name string, roles []string, now func() time.Time) (*hostagent.RenewingCert, error) {
	cert, _, err := ControlHostCert(dir, a, name, roles, now())
	if err != nil {
		return nil, err
	}
	r := hostagent.NewRenewingCert(cert, func(_ context.Context, at time.Time) (tls.Certificate, error) {
		next, _, err := ControlHostCert(dir, a, name, roles, at)
		return next, err
	})
	r.Now, r.OnHandshake = now, true
	return r, nil
}

// issueHostCert issues the host's certificate for a new key, as of now, and
// writes both.
func issueHostCert(dir, certPath, keyPath string, a *Authority, name string, roles []string, now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := a.Authorities().IssueHostAt(now, name, roles, &key.PublicKey)
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
	if err != nil || !slices.Contains(leaf.DNSNames, hostagent.ServerName(name)) || hostagent.DueForRenewal(leaf.NotAfter, now) {
		return tls.Certificate{}, false
	}
	cert.Leaf = leaf
	return cert, true
}
