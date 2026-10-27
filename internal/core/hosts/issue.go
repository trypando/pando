package hosts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"time"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/secret"
)

// listenerCertLifetime is how long the cluster listener's own certificate is
// valid. It is issued again at every start, and while the listener runs once
// it is due (hostagent.RenewBefore).
const listenerCertLifetime = 365 * 24 * time.Hour

// ListenerCommonName and listenerURI name the cluster listener's certificate
// (R-415). Every host holds a certificate from the same authority, so chaining
// to it says only "a member of this cluster": a host that checked no more
// would take any member, an app host included, for the listener, and hand it
// a join token's secret. Only listenerCert issues a certificate carrying both,
// and only a holder of the authority's key, a control host, can run it; a
// host's own certificate is named for the host (hostagent.ServerName), and a
// probe's, the collector's and the proxy's carry other subjects.
const ListenerCommonName = "pando-cluster-listener"

var listenerURI = &url.URL{Scheme: "urn", Opaque: "pando:cluster-listener"}

// Authorities is the authority as the host agent's package takes it.
func (a *Authority) Authorities() hostagent.Authorities {
	return hostagent.Authorities{{Cert: a.Cert, Key: a.Key}}
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// CertPEM is the authority's certificate, without its key: what every host is
// given to check the others' certificates against.
func (a *Authority) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.Cert.Raw})
}

// sign issues a certificate for pub from the authority.
func (a *Authority) sign(tmpl *x509.Certificate, pub any) ([]byte, error) {
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl.SerialNumber = sn
	tmpl.BasicConstraintsValid = true
	return x509.CreateCertificate(rand.Reader, tmpl, a.Cert, pub, a.Key)
}

// IssueHost signs a joining host's certificate request (R-415) as the host
// agent expects a host's certificate (hostagent.Authorities.IssueHost, R-416):
// named pando-agent.<name>.invalid, its roles as organizational units, for
// server and client authentication. Only the request's key is taken from it;
// every other field is the authority's choice. It is valid from now; a host
// renews it before it expires (Service.Renew, R-421).
func (a *Authority) IssueHost(csrPEM []byte, name string, roles []string, now time.Time) (certPEM []byte, sha string, err error) {
	pub, err := csrKey(csrPEM)
	if err != nil {
		return nil, "", err
	}
	der, err := a.Authorities().IssueHostAt(now, name, roles, pub)
	if err != nil {
		return nil, "", errs.Wrap(errs.Internal, "Pando could not issue the host's certificate.", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), hostagent.Fingerprint256(der), nil
}

// csrKey is the key of a host's certificate request, whose signature proves
// the host holds it.
func csrKey(csrPEM []byte) (any, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errs.New(errs.ValidInvalid, "The host's certificate request is not a PEM certificate request.")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err == nil {
		err = csr.CheckSignature()
	}
	if err != nil {
		return nil, errs.Wrap(errs.ValidInvalid, "The host's certificate request could not be read.", err)
	}
	return csr.PublicKey, nil
}

// csrKeySHA256 is the SHA-256 of the public key in a host's certificate
// request, hex: what tells a joining host trying again from another machine
// joining under the same name (R-415, state.Host.KeySHA256).
func csrKeySHA256(csrPEM []byte) (string, error) {
	pub, err := csrKey(csrPEM)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", errs.Wrap(errs.ValidInvalid, "The host's certificate request holds a key Pando cannot use.", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// IssueHostClient issues the certificate the host hostID checks the other
// hosts with (hostagent.Authorities.IssueHostClient, R-421), as PEM: it
// carries the host's ID, which is who its pings are from.
func (a *Authority) IssueHostClient(hostID string, now time.Time) (certPEM []byte, keyPEM secret.Value, err error) {
	cert, err := a.Authorities().IssueHostClientAt(now, hostID)
	if err != nil {
		return nil, secret.Value{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, secret.Value{}, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		secret.New(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))), nil
}

// ListenerTLS is the TLS configuration of the cluster listener (R-415): a
// certificate issued from the authority now, for names, sent with the
// authority's own so a joining host can check the chain against its token's
// hash before it sends anything. A client certificate is checked against the
// authority when one is presented: a joining host has none yet, and a host
// that has joined presents its own for the peer list (PeersPath). The
// certificate is issued again while the listener runs, before it expires.
func (a *Authority) ListenerTLS(names []string, now time.Time) (*tls.Config, error) {
	cert, err := a.listenerCert(names, now)
	if err != nil {
		return nil, err
	}
	renewing := hostagent.NewRenewingCert(cert, func(_ context.Context, now time.Time) (tls.Certificate, error) {
		return a.listenerCert(names, now)
	})
	renewing.OnHandshake = true
	pool := x509.NewCertPool()
	pool.AddCert(a.Cert)
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: renewing.GetCertificate,
		ClientAuth:     tls.VerifyClientCertIfGiven,
		ClientCAs:      pool,
	}, nil
}

// listenerCert is the cluster listener's certificate for names, as of now,
// sent with the authority's own.
func (a *Authority) listenerCert(names []string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: ListenerCommonName},
		URIs:        []*url.URL{listenerURI},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(listenerCertLifetime),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if n != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := a.sign(tmpl, &key.PublicKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, a.Cert.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

// errAuthorityMismatch is a server whose authority is not the one the token
// was minted for (R-415). Nothing has been sent to it.
var errAuthorityMismatch = errors.New("the server's certificate authority does not match the join token")

// errNotTheListener is a server whose certificate is from the cluster's
// authority but is not the cluster listener's: another host posing as it
// (R-415). Nothing has been sent to it.
var errNotTheListener = errors.New("the server's certificate is not the cluster listener's")

// PinnedTLS is the TLS configuration a host connects to the cluster listener
// with, joining or joined (R-415). It trusts exactly one authority, the one
// whose public key hashes to hash, and refuses the server before anything is
// sent unless the server's certificate chains to it and is the listener's
// (isListener). The server's name is not checked: the token's address may be
// an IP address or a name the listener's certificate does not carry, and the
// pinned authority with the listener's identity is the trust, as in kubeadm's
// discovery.
func PinnedTLS(hash string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Verified by VerifyConnection instead, against the pinned authority.
		InsecureSkipVerify: true, //nolint:gosec // G402: the chain is checked below, against the authority the join token pins.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if err := verifyPinned(cs.PeerCertificates, hash, time.Time{}); err != nil {
				return err
			}
			if !isListener(cs.PeerCertificates[0]) {
				return errNotTheListener
			}
			return nil
		},
	}
}

// isListener reports whether a certificate that chains to the cluster's
// authority is the cluster listener's: its subject and its only URI are
// listenerCert's.
func isListener(cert *x509.Certificate) bool {
	return cert.Subject.CommonName == ListenerCommonName &&
		len(cert.URIs) == 1 && cert.URIs[0].String() == listenerURI.String()
}

// verifyPinned checks that chain's first certificate chains, for server
// authentication, to the authority whose public key hashes to hash. Whose
// certificate it is, PinnedTLS checks.
func verifyPinned(chain []*x509.Certificate, hash string, at time.Time) error {
	if len(chain) == 0 {
		return errAuthorityMismatch
	}
	roots := x509.NewCertPool()
	pinned := false
	for _, c := range chain[1:] {
		sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
		if c.IsCA && hex.EncodeToString(sum[:]) == hash {
			roots.AddCert(c)
			pinned = true
		}
	}
	if !pinned {
		return errAuthorityMismatch
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{
		Roots:       roots,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime: at,
	}); err != nil {
		return errAuthorityMismatch
	}
	return nil
}
