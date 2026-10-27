package hosts

import (
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
	"time"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

// listenerCertLifetime is how long the cluster listener's own certificate is
// valid. It is issued again at every start.
const listenerCertLifetime = 365 * 24 * time.Hour

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
// every other field is the authority's choice.
func (a *Authority) IssueHost(csrPEM []byte, name string, roles []string) (certPEM []byte, sha string, err error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, "", errs.New(errs.ValidInvalid, "The joining host's certificate request is not a PEM certificate request.")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err == nil {
		err = csr.CheckSignature()
	}
	if err != nil {
		return nil, "", errs.Wrap(errs.ValidInvalid, "The joining host's certificate request could not be read.", err)
	}
	der, err := hostagent.Authorities{{Cert: a.Cert, Key: a.Key}}.IssueHost(name, roles, csr.PublicKey)
	if err != nil {
		return nil, "", errs.Wrap(errs.Internal, "Pando could not issue the joining host's certificate.", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), hostagent.Fingerprint256(der), nil
}

// ListenerTLS is the TLS configuration of the cluster listener (R-415): a
// certificate issued from the authority now, for names, sent with the
// authority's own so a joining host can check the chain against its token's
// hash before it sends anything.
func (a *Authority) ListenerTLS(names []string, now time.Time) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "pando-cluster-listener"},
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
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der, a.Cert.Raw},
			PrivateKey:  key,
		}},
	}, nil
}

// errAuthorityMismatch is a server whose authority is not the one the token
// was minted for (R-415). Nothing has been sent to it.
var errAuthorityMismatch = errors.New("the server's certificate authority does not match the join token")

// PinnedTLS is the TLS configuration a joining host connects with (R-415). It
// trusts exactly one authority, the one whose public key hashes to hash, and
// refuses the server before anything is sent unless the server's certificate
// chains to it. The server's name is not checked: the token's address may be
// an IP address or a name the listener's certificate does not carry, and the
// pinned authority is the trust, as in kubeadm's discovery.
func PinnedTLS(hash string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Verified by VerifyConnection instead, against the pinned authority.
		InsecureSkipVerify: true, //nolint:gosec // G402: the chain is checked below, against the authority the join token pins.
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyPinned(cs.PeerCertificates, hash)
		},
	}
}

func verifyPinned(chain []*x509.Certificate, hash string) error {
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
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return errAuthorityMismatch
	}
	return nil
}
