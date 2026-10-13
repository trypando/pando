package docker

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// postgresTLSDir is where a provisioned Postgres finds its certificate. In the
// container's own filesystem, not the data volume: the files are rewritten on
// every start, so there is nothing in them worth backing up.
const postgresTLSDir = "/var/lib/postgresql/pando-tls"

// postgresTLS is the certificate and key a provisioned Postgres serves TLS with.
//
// Why TLS at all, on a network nothing outside the bundle can reach (R-134):
// clients disagree about the default. Go's lib/pq requires TLS unless the URL
// says sslmode=disable, and asyncpg refuses a URL that says it ("connect() got
// an unexpected keyword argument 'sslmode'", issue #90). A server that offers
// TLS satisfies both with a URL that says nothing: require and prefer connect
// over TLS, and a client that asks for disable is still let in, because the
// image's pg_hba accepts either.
//
// Derived from the service's password rather than generated, so the same
// service gets the same bytes on every deploy. Provision runs on every deploy
// and on every reconciler tick, and the runtime recreates a container whose
// environment changed: a fresh certificate each time would restart the
// database every fifteen seconds. Deriving it needs nothing stored that is not
// stored already — the password is the service's one persisted secret, and the
// key protects the same connection the password does. No client verifies it
// (there is no CA to verify against), which is what sslmode=require means.
//
// ECDSA P-256 because every Postgres client's TLS stack accepts it. The private
// scalar comes from HKDF over the password, and the signature is the
// deterministic RFC 6979 one crypto/ecdsa makes when given no random source.
func postgresTLS(host, password string) (secret.Value, secret.Value, error) {
	priv, err := derivedKey(password)
	if err != nil {
		return secret.Value{}, secret.Value{}, err
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return secret.Value{}, secret.Value{}, tlsFailed(err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return secret.Value{}, secret.Value{}, tlsFailed(err)
	}
	signer, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return secret.Value{}, secret.Value{}, tlsFailed(fmt.Errorf("unexpected key type %T", parsed))
	}

	// Fixed serial and validity, because the bytes must not depend on when
	// they were made. 9999-12-31 is RFC 5280's "no well-defined expiration":
	// a certificate nobody verifies has nothing to expire for, and one that
	// did expire would stop the database on a date nobody chose.
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(nil, tpl, tpl, &signer.PublicKey, signer)
	if err != nil {
		return secret.Value{}, secret.Value{}, tlsFailed(err)
	}

	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return secret.New(string(cert)), secret.New(string(key)), nil
}

// derivedKey is the P-256 key HKDF derives from the password.
//
// A 32-byte string is a valid P-256 scalar unless it is zero or at least the
// group order, odds of about one in 2^32. The counter makes the next candidate
// as deterministic as the first.
func derivedKey(password string) (*ecdh.PrivateKey, error) {
	var last error
	for i := range 8 {
		seed, err := hkdf.Key(sha256.New, []byte(password), nil, fmt.Sprintf("pando postgres tls %d", i), 32)
		if err != nil {
			return nil, tlsFailed(err)
		}
		priv, err := ecdh.P256().NewPrivateKey(seed)
		if err == nil {
			return priv, nil
		}
		last = err
	}
	return nil, tlsFailed(last)
}

func tlsFailed(err error) error {
	return errs.Wrap(errs.Internal, "Pando could not make the certificate the PostgreSQL service encrypts its connections with.", err)
}
