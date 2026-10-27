package hosts

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/keyfile"
)

// HostCerts are a joined host's certificates while it runs (R-421): its own,
// which its agent serves and which it presents to the cluster listener, and
// its probe certificate, which it checks the other hosts with. Both are
// renewed together from the cluster listener once its own is due, written
// over the files the join left (JoinFiles), and taken by the next handshake.
type HostCerts struct {
	Host  *hostagent.RenewingCert
	Probe *hostagent.RenewingCert
}

// HostRenewal is how a joined host renews its certificates.
type HostRenewal struct {
	// Address is the cluster listener, https://host:port: the join token's.
	Address string
	// Dir is the cluster directory the join wrote to.
	Dir  string
	Name string
	// AuthorityPEM is the cluster authority's certificate (ca.crt).
	AuthorityPEM []byte
	// HTTP makes the client a renewal connects with; nil is one with a
	// one-minute timeout.
	HTTP func(*tls.Config) *http.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Certs holds host and probe, renewing them from the cluster when host is due.
func (r *HostRenewal) Certs(host, probe tls.Certificate) *HostCerts {
	c := &HostCerts{Probe: hostagent.NewRenewingCert(probe, nil)}
	c.Host = hostagent.NewRenewingCert(host, func(ctx context.Context, _ time.Time) (tls.Certificate, error) {
		next, nextProbe, err := r.Renew(ctx, c.Host.Current())
		if err != nil {
			return tls.Certificate{}, err
		}
		c.Probe.Set(nextProbe)
		return next, nil
	})
	c.Host.Now, c.Probe.Now = r.Now, r.Now
	return c
}

func (r *HostRenewal) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *HostRenewal) client(cfg *tls.Config) *http.Client {
	if r.HTTP != nil {
		return r.HTTP(cfg)
	}
	return &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: cfg}}
}

// Renew asks the cluster for new certificates, presenting current, for a new
// key, checks what it is given, and writes it to Dir. It returns the host's
// new certificate and its new probe certificate.
func (r *HostRenewal) Renew(ctx context.Context, current tls.Certificate) (host, probe tls.Certificate, err error) {
	hash, err := authorityHash(r.AuthorityPEM)
	if err != nil {
		return host, probe, err
	}
	key, err := r.nextKey(current)
	if err != nil {
		return host, probe, errs.Wrap(errs.Internal, "Pando could not make this host's next key.", err)
	}
	csr, err := hostCSR(key, r.Name)
	if err != nil {
		return host, probe, errs.Wrap(errs.Internal, "Pando could not make this host's next key.", err)
	}
	cfg := PinnedTLS(hash)
	cfg.Certificates = []tls.Certificate{current}
	var wire WireRenewResult
	if err := exchange(ctx, r.client(cfg), r.Address, RenewPath, WireRenewRequest{CSR: string(csr)}, &wire); err != nil {
		return host, probe, err
	}
	if err := checkIssued(JoinResult{Certificate: []byte(wire.Certificate), Authority: r.AuthorityPEM}, key, hash, r.now()); err != nil {
		return host, probe, err
	}
	if probe, err = tls.X509KeyPair([]byte(wire.HostClientCert), []byte(wire.HostClientKey)); err != nil {
		return host, probe, errs.Wrap(errs.Internal, "The cluster sent a probe certificate Pando cannot use.", err)
	}
	keyPEM, err := keyPEMOf(key)
	if err != nil {
		return host, probe, errs.Wrap(errs.Internal, "Pando could not keep this host's next key.", err)
	}
	if err := r.write(keyPEM, wire); err != nil {
		return host, probe, err
	}
	// Answered and written: the next renewal asks for a key of its own.
	_ = os.Remove(filepath.Join(r.Dir, HostNextKeyFile))
	host, err = tls.X509KeyPair([]byte(wire.Certificate), keyPEM)
	return host, probe, err
}

// nextKey is the key this host's renewal asks a certificate for (R-421):
// the one kept in HostNextKeyFile by a renewal not yet answered and written,
// or a new one, kept there before it is asked for. So a renewal tried again
// after its answer was lost asks for the same key, and the cluster answers it
// with the certificate it already issued rather than another
// (Service.Renew). A kept key that is current's already is left over from a
// renewal that finished, and is not used again.
func (r *HostRenewal) nextKey(current tls.Certificate) (*ecdsa.PrivateKey, error) {
	path := filepath.Join(r.Dir, HostNextKeyFile)
	if key, err := readECKey(path); err == nil && !holds(current, key) {
		return key, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	keyPEM, err := keyPEMOf(key)
	if err != nil {
		return nil, err
	}
	if err := keyfile.Replace(path, keyPEM); err != nil {
		return nil, err
	}
	return key, nil
}

// readECKey reads an EC key written by keyPEMOf.
func readECKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the host's own cluster directory
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an EC key")
	}
	return key, nil
}

// holds reports whether cert is for key.
func holds(cert tls.Certificate, key *ecdsa.PrivateKey) bool {
	signer, ok := cert.PrivateKey.(crypto.Signer)
	return ok && key.PublicKey.Equal(signer.Public())
}

func keyPEMOf(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// write puts the renewed certificates over the ones the join left, each
// file replaced whole, the key before its certificate.
func (r *HostRenewal) write(keyPEM []byte, wire WireRenewResult) error {
	for _, w := range []fileWrite{
		{filepath.Join(r.Dir, HostKeyFile), keyPEM},
		{filepath.Join(r.Dir, HostCertFile), []byte(wire.Certificate)},
		{filepath.Join(r.Dir, HostClientKeyFile), []byte(wire.HostClientKey)},
		{filepath.Join(r.Dir, HostClientCertFile), []byte(wire.HostClientCert)},
	} {
		if err := keyfile.Replace(w.path, w.data); err != nil {
			return errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not write this host's renewed certificate to %s (server.cluster_dir).", w.path), err)
		}
	}
	return nil
}

// authorityHash is the hash a join token pins for the authority in
// authorityPEM.
func authorityHash(authorityPEM []byte) (string, error) {
	block, _ := pem.Decode(authorityPEM)
	if block == nil {
		return "", errors.New("the cluster authority's certificate is not PEM")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("the cluster authority's certificate cannot be read: %w", err)
	}
	sum := sha256.Sum256(ca.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]), nil
}
