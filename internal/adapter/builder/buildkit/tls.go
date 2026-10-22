package buildkit

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/keyfile"
)

// Mutual TLS between Pando and each BuildKit it runs (R-117).
//
// A build step shares its daemon's network namespace, so it can reach the
// daemon's API on loopback; with the listener open, a step could drive the
// daemon. Every daemon's listener therefore requires a client certificate
// issued by the install's build authority, whose key never leaves Pando's data
// directory. A daemon is given the authority's certificate and a server
// certificate of its own, issued when it starts: server authentication only,
// so a step that read it could not present it as a client. Pando issues
// itself a client certificate for each daemon, held in memory only. Nothing is
// stored per app.

// buildAuthorityFile is the install's build authority in Pando's data
// directory: its certificate followed by its key, 0600.
const buildAuthorityFile = "buildkit-ca.pem"

// Validity of what is issued. The authority outlives any one Pando; a leaf
// lives as long as the daemon it was issued for, which is removed after its
// idle window, so a year is never reached in practice.
const (
	buildAuthorityValidity = 10 * 365 * 24 * time.Hour
	buildLeafValidity      = 365 * 24 * time.Hour
)

// buildClientName is the subject of the certificate Pando presents to a
// daemon.
const buildClientName = "pando-build-client"

// authority is the install's build authority.
type authority struct {
	cert *x509.Certificate
	key  crypto.Signer
}

// leaf is a certificate and its key as PEM, as a daemon reads them.
type leaf struct {
	certPEM, keyPEM []byte
}

// loadAuthority reads the build authority from dir, creating it the first
// time. Replicas sharing one data directory may start at once: the one that
// loses the race to create the file reads the winner's (keyfile.Create).
func loadAuthority(dir string) (*authority, error) {
	path := filepath.Join(dir, buildAuthorityFile)
	data, err := os.ReadFile(path) //nolint:gosec // G304: Pando's own data directory, nothing from a request
	if errors.Is(err, os.ErrNotExist) {
		fresh, gerr := newAuthorityPEM()
		if gerr != nil {
			return nil, gerr
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if cerr := keyfile.Create(path, fresh); cerr != nil && !errors.Is(cerr, os.ErrExist) {
			return nil, cerr
		}
		data, err = os.ReadFile(path) //nolint:gosec // as above
	}
	if err != nil {
		return nil, err
	}
	return parseAuthority(data)
}

// newAuthorityPEM generates an authority, as its certificate followed by its
// key.
func newAuthorityPEM() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "Pando build authority"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(buildAuthorityValidity),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := sign(tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = pem.Encode(&out, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return out.Bytes(), nil
}

// parseAuthority reads an authority written by newAuthorityPEM.
func parseAuthority(data []byte) (*authority, error) {
	var a authority
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		switch block.Type {
		case "CERTIFICATE":
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, err
			}
			a.cert = cert
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			signer, ok := k.(crypto.Signer)
			if !ok {
				return nil, errors.New("the key cannot sign")
			}
			a.key = signer
		}
	}
	if a.cert == nil || a.key == nil || !a.cert.IsCA {
		return nil, errors.New("the file does not hold an authority's certificate and key")
	}
	if pub, ok := a.cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(a.key.Public()) {
		return nil, errors.New("the key does not belong to the certificate")
	}
	return &a, nil
}

// certPEM is the authority's certificate, which is all a daemon is given of
// it.
func (a *authority) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.cert.Raw})
}

// issueServer issues a daemon its certificate for name: server
// authentication only.
func (a *authority) issueServer(name string) (leaf, error) {
	cert, err := a.issue(pkix.Name{CommonName: name}, []string{name}, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return leaf{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return leaf{}, err
	}
	return leaf{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// clientTLS is Pando's side of a connection to the daemon whose certificate
// names serverName: a client certificate of its own, issued now and kept in
// memory, and the daemon's checked against the authority and that name.
func (a *authority) clientTLS(serverName string) (*tls.Config, error) {
	cert, err := a.issue(pkix.Name{CommonName: buildClientName}, nil, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		// Go's own check is replaced, not skipped: gRPC dials the daemon by
		// its container's name and refuses a ServerName that differs from it,
		// so the daemon's certificate is checked against the authority and
		// its own name in VerifyConnection, on every handshake.
		InsecureSkipVerify: true, //nolint:gosec // G402: verified in VerifyConnection, below
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyDaemon(cs, pool, serverName)
		},
	}, nil
}

// verifyDaemon checks the certificate a daemon presented: issued by the build
// authority, for server authentication, to the name it was issued for.
func verifyDaemon(cs tls.ConnectionState, roots *x509.CertPool, serverName string) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("the build service presented no certificate")
	}
	intermediates := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		intermediates.AddCert(c)
	}
	_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		DNSName:       serverName,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err
}

func (a *authority) issue(subject pkix.Name, dnsNames []string, usage x509.ExtKeyUsage) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now().UTC()
	notAfter := now.Add(buildLeafValidity)
	if notAfter.After(a.cert.NotAfter) {
		notAfter = a.cert.NotAfter
	}
	der, err := sign(&x509.Certificate{
		Subject:     subject,
		DNSNames:    dnsNames,
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, nil
}

// sign gives tmpl a random serial and signs it with parent's key.
func sign(tmpl, parent *x509.Certificate, pub crypto.PublicKey, key crypto.Signer) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl.SerialNumber = serial
	return x509.CreateCertificate(rand.Reader, tmpl, parent, pub, key)
}

// daemonServerName is the name a daemon's certificate is issued for: random,
// and in a domain that never resolves, so it names one daemon and nothing
// else. Pando dials the daemon by its container's name and checks this one.
func daemonServerName() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "d" + hex.EncodeToString(b) + ".buildkit.pando.invalid", nil
}

// authorityUnavailable is the error when the build authority cannot be read
// or made.
func authorityUnavailable(dir string, err error) error {
	return errs.Wrap(errs.AdapterUnavailable,
		fmt.Sprintf("Pando could not read or create its build certificate authority, %s.", filepath.Join(dir, buildAuthorityFile)), err).
		WithRemedy("Pando's data directory must be writable by Pando. If the file exists and is damaged, remove it; Pando makes a new one and every build service it starts afterwards uses it.")
}
