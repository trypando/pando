// Package hostagent is the forwarding agent that stands on each app host of a
// multi-host Docker install where Pando's own container stands on one host
// (O-45, design 06 §4, notes-multi-host-docker-issue-72.md).
//
// The agent decides nothing. Pando's proxy authenticates the request,
// authorizes it, mints the assertion and strips every X-Pando-* header and
// pando_* cookie (R-023, R-053, R-173) before it opens a connection here. The
// agent's two checks are the ones that keep it from being a way around that:
// a connection must present Pando's client certificate, and it is carried
// only to a container on one of the Pando-managed app networks the agent was
// joined to on its own host.
//
// This package is shared by the agent (cmd/pando host-agent) and the runtime
// adapter that dials it. It imports nothing from core.
package hostagent

import (
	"bytes"
	"crypto"
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
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// ClientCommonName is the subject every certificate Pando's proxy presents to
// an agent carries. An agent refuses any other, so a certificate the same
// authority issued to an agent (a server certificate) cannot be used to open a
// connection through another agent.
const ClientCommonName = "pando-proxy"

// RoleControl is the organizational unit a certificate issued for a control
// host carries: the cluster's own name for the role (R-414).
const RoleControl = "control"

// serverNamePrefix starts the DNS name in an agent's certificate. The proxy
// checks it by name rather than by address, so an agent reached through a NAT
// or a load balancer address still verifies.
const serverNamePrefix = "pando-agent."

// ServerName is the name an agent's certificate is issued for, and what the
// dialer verifies, for the host named host in the adapter's configuration.
func ServerName(host string) string { return serverNamePrefix + host + ".invalid" }

// Validity of what is issued. The authority is long-lived and is replaced by
// overlap (Authorities); leaf certificates are issued again at each start of
// Pando and each re-creation of an agent, so they are short.
const (
	authorityValidity = 10 * 365 * 24 * time.Hour
	leafValidity      = 90 * 24 * time.Hour
)

// Authority is one install certificate authority for agents: its certificate
// and its key.
type Authority struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

// Authorities is the configured set, newest first. The first issues; every
// one is trusted, which is how the authority is replaced without a moment in
// which an agent and the proxy disagree: add the new one first, wait for the
// agents to be re-created, then remove the old one.
type Authorities []Authority

// NewAuthority generates an authority and returns it as PEM: the certificate
// followed by its private key. That text is what the adapter's agent_ca
// credential holds, sealed like every adapter credential (R-190).
func NewAuthority() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Pando host agent authority"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(authorityValidity),
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
	var out bytes.Buffer
	_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = pem.Encode(&out, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return out.Bytes(), nil
}

// ParseAuthorities reads one or more authorities from PEM, each a certificate
// followed by its key.
func ParseAuthorities(data []byte) (Authorities, error) {
	var out Authorities
	var pending *x509.Certificate
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
				return nil, fmt.Errorf("the agent authority's certificate could not be read: %w", err)
			}
			if !cert.IsCA {
				return nil, errors.New("the agent authority's certificate is not a certificate authority")
			}
			pending = cert
		case "PRIVATE KEY", "EC PRIVATE KEY":
			if pending == nil {
				return nil, errors.New("the agent authority's private key comes before its certificate; put each certificate first, then its key")
			}
			key, err := parseKey(block)
			if err != nil {
				return nil, err
			}
			if !samePublicKey(pending.PublicKey, key.Public()) {
				return nil, errors.New("the agent authority's private key does not belong to the certificate before it")
			}
			out = append(out, Authority{Cert: pending, Key: key})
			pending = nil
		}
	}
	if pending != nil {
		return nil, errors.New("the agent authority's certificate has no private key after it")
	}
	if len(out) == 0 {
		return nil, errors.New("no agent authority was found; generate one with `pando host-agent new-authority`")
	}
	return out, nil
}

func parseKey(block *pem.Block) (crypto.Signer, error) {
	if block.Type == "EC PRIVATE KEY" {
		return x509.ParseECPrivateKey(block.Bytes)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the agent authority's private key could not be read: %w", err)
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("the agent authority's private key cannot sign")
	}
	return signer, nil
}

func samePublicKey(a, b crypto.PublicKey) bool {
	ak, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && ak.Equal(b)
}

// Pool is every authority's certificate, for verifying either side.
func (as Authorities) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	for _, a := range as {
		pool.AddCert(a.Cert)
	}
	return pool
}

// PoolPEM is Pool as PEM, which is what an agent is given: certificates
// only, never a key.
func (as Authorities) PoolPEM() []byte {
	var out bytes.Buffer
	for _, a := range as {
		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: a.Cert.Raw})
	}
	return out.Bytes()
}

// Fingerprint names the set, so an agent issued under a different one is
// known to need re-creating.
func (as Authorities) Fingerprint() string {
	h := sha256.New()
	for _, a := range as {
		h.Write(a.Cert.Raw)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// IssueClient issues the certificate Pando's proxy presents to agents. Held
// in memory only: every replica issues its own from the authority at start.
//
// It carries RoleControl: only a control host holds an authority's key, so
// only a control host can issue one, and an agent carries runtime calls for
// no other certificate (R-416).
func (as Authorities) IssueClient() (tls.Certificate, error) {
	return as.issue(pkix.Name{CommonName: ClientCommonName, OrganizationalUnit: []string{RoleControl}}, nil, x509.ExtKeyUsageClientAuth)
}

// IssueServer issues an agent's certificate for the host named host, as PEM.
// Server authentication only: it cannot be presented to another agent as
// Pando's.
func (as Authorities) IssueServer(host string) (certPEM, keyPEM []byte, err error) {
	name := ServerName(host)
	cert, err := as.issue(pkix.Name{CommonName: name}, []string{name}, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	var c, k bytes.Buffer
	_ = pem.Encode(&c, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	_ = pem.Encode(&k, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return c.Bytes(), k.Bytes(), nil
}

func (as Authorities) issue(subject pkix.Name, dnsNames []string, usage x509.ExtKeyUsage) (tls.Certificate, error) {
	return as.issueAt(time.Now(), &x509.Certificate{Subject: subject, DNSNames: dnsNames}, usage)
}

// issueAt issues a certificate with tmpl's subject and names for a new key,
// as of now.
func (as Authorities) issueAt(now time.Time, tmpl *x509.Certificate, usages ...x509.ExtKeyUsage) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	der, err := as.signAt(now, tmpl, &key.PublicKey, usages...)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// sign issues a certificate for pub from the first authority, as DER, as of
// now.
func (as Authorities) sign(subject pkix.Name, dnsNames []string, pub crypto.PublicKey, usages ...x509.ExtKeyUsage) ([]byte, error) {
	return as.signAt(time.Now(), &x509.Certificate{Subject: subject, DNSNames: dnsNames}, pub, usages...)
}

// signAt issues a certificate for pub from the first authority, as DER: the
// subject and names in tmpl, valid from now for leafValidity, or until the
// authority expires if that is sooner.
func (as Authorities) signAt(now time.Time, tmpl *x509.Certificate, pub crypto.PublicKey, usages ...x509.ExtKeyUsage) ([]byte, error) {
	if len(as) == 0 {
		return nil, errors.New("no agent authority is configured")
	}
	ca := as[0]
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	notAfter := now.Add(leafValidity)
	if notAfter.After(ca.Cert.NotAfter) {
		notAfter = ca.Cert.NotAfter
	}
	cert := &x509.Certificate{
		SerialNumber: serial,
		Subject:      tmpl.Subject,
		DNSNames:     tmpl.DNSNames,
		URIs:         tmpl.URIs,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usages,
	}
	return x509.CreateCertificate(rand.Reader, cert, ca.Cert, pub, ca.Key)
}

// IssueHost issues a joined host's certificate for its key pub, as DER: the
// one its agent serves with, and the one it presents to other hosts (R-416,
// notes-cluster.md). It names the host as ServerName does, so a dialer
// verifies it by name, and carries the host's roles as organizational units.
// It is never Pando's proxy's: its subject is not ClientCommonName, which an
// agent requires of every connection, so a host's own certificate opens
// nothing through another host's agent.
func (as Authorities) IssueHost(name string, roles []string, pub crypto.PublicKey) ([]byte, error) {
	return as.IssueHostAt(time.Now(), name, roles, pub)
}

// IssueHostAt is IssueHost as of now: how a host's certificate is renewed
// (R-421: a certificate left to expire would cut every host off at once).
func (as Authorities) IssueHostAt(now time.Time, name string, roles []string, pub crypto.PublicKey) ([]byte, error) {
	server := ServerName(name)
	tmpl := &x509.Certificate{Subject: pkix.Name{CommonName: server, OrganizationalUnit: roles}, DNSNames: []string{server}}
	return as.signAt(now, tmpl, pub, x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth)
}

// IsControl reports whether a verified client certificate was issued for a
// control host: Pando's proxy's, carrying RoleControl. Only a holder of the
// authority's key — a control host — can issue one (R-416).
func IsControl(cert *x509.Certificate) bool {
	return cert != nil && cert.Subject.CommonName == ClientCommonName &&
		slices.Contains(cert.Subject.OrganizationalUnit, RoleControl)
}

// Fingerprint256 is a certificate's fingerprint as the cluster records it:
// the SHA-256 of its DER, hex (cluster_hosts.cert_sha256).
func Fingerprint256(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// ClientTLS is the dialer's TLS configuration: Pando's client certificate,
// and the agent's verified against the authorities and the host's name.
//
// The client certificate is issued again from the authorities before it
// expires (sharedRenewal), so a process that runs past leafValidity keeps
// connecting (R-421): every configuration made for one certificate shares
// its renewal.
func ClientTLS(as Authorities, client tls.Certificate, host string) *tls.Config {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    as.Pool(),
		ServerName: ServerName(host),
	}
	if len(client.Certificate) == 0 || len(as) == 0 {
		cfg.Certificates = []tls.Certificate{client}
		return cfg
	}
	cfg.GetClientCertificate = sharedRenewal(as, client).GetClientCertificate
	return cfg
}

// PinnedClientTLS is ClientTLS that also requires the agent's certificate to
// be the one its host joined the cluster with (cluster_hosts.cert_sha256), so
// a machine given another certificate under the host's name is not taken for
// it. An empty pin is ClientTLS. pin may name several fingerprints, separated
// by commas: while a host's certificate is renewed, the one it holds and the
// one it was just issued are both its own (state.Host.Pins).
func PinnedClientTLS(as Authorities, client tls.Certificate, host, pin string) *tls.Config {
	cfg := ClientTLS(as, client, host)
	if pin == "" {
		return cfg
	}
	pins := strings.Split(pin, ",")
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 || !slices.Contains(pins, Fingerprint256(cs.PeerCertificates[0].Raw)) {
			return fmt.Errorf("the agent's certificate is not the one host %s joined the cluster with", host)
		}
		return nil
	}
	return cfg
}

// ServerTLS is the agent's TLS configuration: its own certificate, and a
// client certificate required, issued by one of the authorities, for client
// authentication, and naming Pando's proxy.
func ServerTLS(certPEM, keyPEM, authoritiesPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("the agent's certificate could not be read: %w", err)
	}
	cfg, err := RenewingServerTLS(nil, authoritiesPEM)
	if err != nil {
		return nil, err
	}
	cfg.Certificates = []tls.Certificate{cert}
	return cfg, nil
}

// RenewingServerTLS is ServerTLS serving cert, which is renewed while the
// agent runs (R-421): each handshake presents the certificate held then. A nil
// cert leaves Certificates to the caller.
func RenewingServerTLS(cert *RenewingCert, authoritiesPEM []byte) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(authoritiesPEM) {
		return nil, errors.New("the agent was given no authority to check Pando's certificate against")
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientCAs:  pool,
		// Verified against ClientCAs, and Go checks the client-auth usage.
		ClientAuth: tls.RequireAndVerifyClientCert,
		// No resumption: every connection presents and proves the
		// certificate afresh.
		SessionTicketsDisabled: true,
		// VerifyConnection rather than VerifyPeerCertificate, because it
		// runs on every handshake, resumed or not.
		VerifyConnection: func(cs tls.ConnectionState) error {
			chains := cs.VerifiedChains
			if len(chains) == 0 || len(chains[0]) == 0 {
				return errors.New("no verified client certificate")
			}
			// Pando's proxy, or a host checking this one, which may only
			// ping (answerPing).
			if cn := chains[0][0].Subject.CommonName; cn != ClientCommonName && cn != HostCommonName {
				return fmt.Errorf("client certificate %q is not Pando's proxy or a host of its cluster", cn)
			}
			return nil
		},
	}
	if cert != nil {
		cfg.GetCertificate = cert.GetCertificate
	}
	return cfg, nil
}
