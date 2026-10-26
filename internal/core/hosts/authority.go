// Package hosts is the install's cluster: the hosts that have joined it and
// the join tokens they join with (R-414, R-415, docs/design/notes-cluster.md).
//
// It models membership, roles and liveness. It never chooses where a workload
// runs: that stays the runtime adapter's (R-256), and nothing here may rank
// hosts for one (R-010).
package hosts

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
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

// DefaultAuthorityPath is where the cluster's certificate authority lives
// unless server.cluster_authority_path says otherwise: beside the token key.
// A control host is given a copy when it joins (R-415, R-417).
const DefaultAuthorityPath = "/var/lib/pando/cluster-ca.pem"

// authorityLifetime is how long the cluster's authority is valid. Long, since
// every host's certificate chains to it and replacing it means every host
// joining again.
const authorityLifetime = 20 * 365 * 24 * time.Hour

// Authority is the cluster's certificate authority. A host's certificate is
// issued from it at join, and a joining host checks it against the hash in its
// join token before sending anything (R-415).
type Authority struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

// Hash is the SHA-256 of the authority's public key, hex: what a join token
// carries, as kubeadm's discovery hash does.
func (a *Authority) Hash() string {
	sum := sha256.Sum256(a.Cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// LoadOrCreateAuthority reads the authority at path, generating it on first
// use. Written by keyfile.Create, so two replicas starting on an empty shared
// volume end with one authority, as with the token key.
func LoadOrCreateAuthority(path string) (*Authority, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		a, perr := parseAuthority(data)
		if perr != nil {
			return nil, errs.Wrap(errs.Internal, fmt.Sprintf("The cluster authority at %s could not be read. No host can join or be reached without it.", path), perr).
				WithRemedy("Restore the file from a backup of this installation, or copy it from another control host.")
		}
		return a, nil

	case errors.Is(err, os.ErrNotExist):
		pemData, err := newAuthorityPEM()
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Pando could not generate the cluster authority.", err)
		}
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not create %s for the cluster authority.", dir), err)
		}
		if err := keyfile.Create(path, pemData); err != nil {
			if errors.Is(err, os.ErrExist) {
				return LoadOrCreateAuthority(path)
			}
			return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not write the cluster authority to %s.", path), err)
		}
		return parseAuthority(pemData)

	default:
		return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not read the cluster authority at %s.", path), err)
	}
}

func newAuthorityPEM() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "pando-cluster"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(authorityLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	out = append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...)
	return out, nil
}

func parseAuthority(data []byte) (*Authority, error) {
	var a Authority
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		switch block.Type {
		case "CERTIFICATE":
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, err
			}
			a.Cert = cert
		case "PRIVATE KEY":
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			signer, ok := key.(crypto.Signer)
			if !ok {
				return nil, errors.New("the cluster authority's key cannot sign")
			}
			a.Key = signer
		}
	}
	if a.Cert == nil || a.Key == nil {
		return nil, errors.New("the file must hold one certificate and one private key")
	}
	if !a.Cert.IsCA {
		return nil, errors.New("the certificate is not a certificate authority")
	}
	return &a, nil
}
