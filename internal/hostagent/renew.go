package hostagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync"
	"time"
)

// Renewal (R-421). Every certificate the cluster issues from its authority is
// valid for leafValidity, and a process holding one may run for longer: left
// alone, every host's certificate would lapse within days of the others, and
// each host would find itself cut off and fence at once. So each is issued
// again while it still has RenewBefore left, and a running process takes the
// new one at its next handshake (tls.Config.GetCertificate,
// GetClientCertificate), never by restarting.

// RenewBefore is how long before it expires a certificate is issued again.
const RenewBefore = 30 * 24 * time.Hour

// renewRetry is how long a renewal that failed on a handshake waits before a
// handshake tries again, so a renewal that keeps failing does not slow every
// connection.
const renewRetry = time.Minute

// DueForRenewal reports whether a certificate valid until notAfter is due to
// be issued again at now.
func DueForRenewal(notAfter, now time.Time) bool {
	return !now.Add(RenewBefore).Before(notAfter)
}

// RenewingCert is a certificate a running process presents, issued again
// before it expires.
type RenewingCert struct {
	// Renew issues the next certificate, as of now.
	Renew func(ctx context.Context, now time.Time) (tls.Certificate, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// OnHandshake renews a certificate found due on a handshake before it is
	// presented: for a renewal made in memory from an authority, which is
	// quick. A renewal that asks another host is made by Run instead, off
	// the handshake.
	OnHandshake bool
	// OnError is told of a renewal that failed; the certificate held is kept.
	OnError func(error)

	renewing sync.Mutex // one renewal at a time
	mu       sync.Mutex
	cur      tls.Certificate
	retryAt  time.Time
}

// NewRenewingCert is a RenewingCert holding cert.
func NewRenewingCert(cert tls.Certificate, renew func(ctx context.Context, now time.Time) (tls.Certificate, error)) *RenewingCert {
	r := &RenewingCert{Renew: renew}
	r.Set(cert)
	return r
}

// InMemory renews cert from the authorities: the same subject, names and
// usages, for a new key. For a certificate only a holder of the authority's
// key issues, and which nothing records: Pando's proxy's, the leader's
// collector's, a control host's probe and listener certificates.
func (as Authorities) InMemory(cert tls.Certificate) *RenewingCert {
	r := NewRenewingCert(cert, func(_ context.Context, now time.Time) (tls.Certificate, error) {
		leaf := leafOf(cert)
		if leaf == nil {
			return tls.Certificate{}, errors.New("the certificate to renew cannot be read")
		}
		return as.Reissue(leaf, now)
	})
	r.OnHandshake = true
	return r
}

// Reissue issues a certificate like leaf, its subject, names and extended
// key usages, for a new key, as of now.
func (as Authorities) Reissue(leaf *x509.Certificate, now time.Time) (tls.Certificate, error) {
	return as.issueAt(now, &x509.Certificate{Subject: leaf.Subject, DNSNames: leaf.DNSNames, URIs: leaf.URIs}, leaf.ExtKeyUsage...)
}

func leafOf(cert tls.Certificate) *x509.Certificate {
	if cert.Leaf != nil {
		return cert.Leaf
	}
	if len(cert.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

func (r *RenewingCert) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Set replaces the certificate held.
func (r *RenewingCert) Set(cert tls.Certificate) {
	if cert.Leaf == nil {
		cert.Leaf = leafOf(cert)
	}
	r.mu.Lock()
	r.cur = cert
	r.mu.Unlock()
}

// Current is the certificate held now.
func (r *RenewingCert) Current() tls.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

// Due reports whether the certificate held is due for renewal.
func (r *RenewingCert) Due() bool {
	leaf := r.Current().Leaf
	return leaf == nil || DueForRenewal(leaf.NotAfter, r.now())
}

// Refresh renews the certificate when it is due, and reports whether it did.
// A failed renewal keeps the certificate held.
func (r *RenewingCert) Refresh(ctx context.Context) (bool, error) {
	r.renewing.Lock()
	defer r.renewing.Unlock()
	if !r.Due() {
		return false, nil
	}
	if r.Renew == nil {
		return false, errors.New("this certificate is renewed with the host's own, not by itself")
	}
	next, err := r.Renew(ctx, r.now())
	if err != nil {
		r.mu.Lock()
		r.retryAt = r.now().Add(renewRetry)
		r.mu.Unlock()
		if r.OnError != nil {
			r.OnError(err)
		}
		return false, err
	}
	r.Set(next)
	return true, nil
}

// Run renews the certificate whenever it is due, looking every interval,
// until ctx ends.
func (r *RenewingCert) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		_, _ = r.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// get is the certificate to present on a handshake, renewed first when
// OnHandshake says so and it is due.
func (r *RenewingCert) get() *tls.Certificate {
	if r.OnHandshake && r.Due() {
		r.mu.Lock()
		retry := r.now().Before(r.retryAt)
		r.mu.Unlock()
		if !retry {
			ctx, cancel := context.WithTimeout(context.Background(), headerTimeout)
			_, _ = r.Refresh(ctx)
			cancel()
		}
	}
	cert := r.Current()
	return &cert
}

// GetCertificate is tls.Config.GetCertificate.
func (r *RenewingCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.get(), nil
}

// GetClientCertificate is tls.Config.GetClientCertificate.
func (r *RenewingCert) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return r.get(), nil
}

// renewals are the client certificates ClientTLS has renewed, by the
// fingerprint of the one first given, so that every configuration made for
// one certificate renews it once between them.
var renewals sync.Map

func sharedRenewal(as Authorities, client tls.Certificate) *RenewingCert {
	key := Fingerprint256(client.Certificate[0]) + "/" + as.Fingerprint()
	if r, ok := renewals.Load(key); ok {
		return r.(*RenewingCert)
	}
	r, _ := renewals.LoadOrStore(key, as.InMemory(client))
	return r.(*RenewingCert)
}
