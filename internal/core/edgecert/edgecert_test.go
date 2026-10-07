package edgecert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// memStore is the issuer's store in memory.
type memStore struct {
	mu         sync.Mutex
	certs      map[string]state.EdgeCertificate
	accounts   map[string]state.AcmeAccount
	challenges map[string]string
	now        func() time.Time
}

func newStore(now func() time.Time) *memStore {
	return &memStore{certs: map[string]state.EdgeCertificate{}, accounts: map[string]state.AcmeAccount{}, challenges: map[string]string{}, now: now}
}

func (m *memStore) Certificate(_ context.Context, name string) (state.EdgeCertificate, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.certs[name]
	return c, ok, nil
}
func (m *memStore) PutCertificate(_ context.Context, c state.EdgeCertificate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.AttemptedAt, c.LastError = m.now(), ""
	m.certs[c.Name] = c
	return nil
}
func (m *memStore) RecordFailure(_ context.Context, name string, domains []string, msg string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.certs[name]
	c.Name, c.AttemptedAt, c.LastError = name, at, msg
	if c.Domains == nil {
		c.Domains = domains
	}
	m.certs[name] = c
	return nil
}
func (m *memStore) Account(_ context.Context, email, dir string) (state.AcmeAccount, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[email+dir]
	return a, ok, nil
}
func (m *memStore) PutAccount(_ context.Context, a state.AcmeAccount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[a.Email+a.Directory] = a
	return nil
}
func (m *memStore) PutChallenge(_ context.Context, token, ka, _ string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.challenges[token] = ka
	return nil
}
func (m *memStore) DeleteChallenge(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.challenges, token)
	return nil
}
func (m *memStore) Challenge(_ context.Context, token string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ka, ok := m.challenges[token]
	return ka, ok, nil
}

// fakeCA plays an ACME server: it proves HTTP-01 by reading the answer back
// the way the CA would, through whatever answers the token's path.
type fakeCA struct {
	orders   int
	fail     error
	validity time.Duration
	now      func() time.Time
	answer   func(token string) (string, bool)
	answered bool
}

func (f *fakeCA) Obtain(ctx context.Context, acct state.AcmeAccount, domains []string, s Solver) (state.AcmeAccount, Issued, error) {
	f.orders++
	if acct.KeyPEM.IsZero() {
		acct.KeyPEM, acct.Registration = secret.New("ACCOUNT KEY"), []byte(`{"uri":"https://ca.test/acct/1"}`)
	}
	if f.fail != nil {
		return acct, Issued{}, f.fail
	}
	if s.Challenge == api.ChallengeHTTP01 {
		const token, ka = "tok-123", "tok-123.thumbprint"
		if err := s.HTTP.Present(ctx, domains[0], token, ka); err != nil {
			return acct, Issued{}, err
		}
		got, ok := f.answer(token)
		f.answered = ok && got == ka
		if err := s.HTTP.CleanUp(ctx, domains[0], token); err != nil {
			return acct, Issued{}, err
		}
		if !f.answered {
			return acct, Issued{}, errors.New("the challenge was not answered")
		}
	}
	cert, key := selfSigned(domains, f.now().Add(f.validity))
	return acct, Issued{CertPEM: cert, KeyPEM: secret.New(string(key))}, nil
}

func selfSigned(domains []string, notAfter time.Time) ([]byte, []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains,
		NotBefore: notAfter.Add(-90 * 24 * time.Hour), NotAfter: notAfter,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kder, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

type fixedClock struct{ t *time.Time }

func (c fixedClock) Now() time.Time                         { return *c.t }
func (c fixedClock) Since(t time.Time) time.Duration        { return c.t.Sub(t) }
func (c fixedClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (c fixedClock) Sleep(time.Duration)                    {}

func setup(t *testing.T) (*Issuer, *memStore, *fakeCA, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	nowf := func() time.Time { return now }
	store := newStore(nowf)
	ca := &fakeCA{validity: 90 * 24 * time.Hour, now: nowf}
	ca.answer = func(token string) (string, bool) {
		ka, ok, _ := Challenges{Store: store}.KeyAuthorization(context.Background(), token)
		return ka, ok
	}
	i := &Issuer{Store: store, ACME: ca, Clock: fixedClock{&now}, Directory: "https://ca.test/dir"}
	return i, store, ca, &now
}

func httpPlan() *api.CertificateIssue {
	return &api.CertificateIssue{Email: "ops@example.com", Challenge: api.ChallengeHTTP01, Orders: []api.CertificateOrder{
		{Name: "pando-tls-a.example.com", Domains: []string{"a.example.com"}},
	}}
}

// TestR169_ThePandoLeaderIssuesOnceAndEveryReplicaServesIt asserts the one
// issuer: the first pass orders the certificate, answering HTTP-01 from the
// store every replica's proxy reads, keeps the account and the certificate,
// and hands the certificate to the edge; later passes order nothing.
func TestR169_ThePandoLeaderIssuesOnceAndEveryReplicaServesIt(t *testing.T) {
	ctx := context.Background()
	i, store, ca, _ := setup(t)

	certs, err := i.Ensure(ctx, httpPlan())
	require.NoError(t, err)
	require.Equal(t, 1, ca.orders)
	require.True(t, ca.answered, "the CA read the answer through the store the proxy reads")
	require.Empty(t, store.challenges, "the answer is withdrawn once checked")
	require.Len(t, certs, 1)
	require.Equal(t, "pando-tls-a.example.com", certs[0].Name)
	require.NotEmpty(t, certs[0].KeyPEM.Reveal())

	acct, ok, _ := store.Account(ctx, "ops@example.com", "https://ca.test/dir")
	require.True(t, ok, "the account key is kept, so the next order reuses the account")
	require.Equal(t, "ACCOUNT KEY", acct.KeyPEM.Reveal())

	certs, err = i.Ensure(ctx, httpPlan())
	require.NoError(t, err)
	require.Equal(t, 1, ca.orders, "a current certificate is not ordered again")
	require.Len(t, certs, 1)
}

// TestR169_ACertificateIsRenewedAheadOfExpiry asserts renewal 30 days before
// expiry, and that a renamed order is ordered again.
func TestR169_ACertificateIsRenewedAheadOfExpiry(t *testing.T) {
	ctx := context.Background()
	i, _, ca, now := setup(t)
	_, err := i.Ensure(ctx, httpPlan())
	require.NoError(t, err)

	*now = now.Add(59 * 24 * time.Hour)
	_, err = i.Ensure(ctx, httpPlan())
	require.NoError(t, err)
	require.Equal(t, 1, ca.orders, "31 days left: not yet")

	*now = now.Add(2 * 24 * time.Hour)
	_, err = i.Ensure(ctx, httpPlan())
	require.NoError(t, err)
	require.Equal(t, 2, ca.orders, "29 days left: renewed")

	plan := httpPlan()
	plan.Orders[0].Domains = []string{"a.example.com", "www.a.example.com"}
	_, err = i.Ensure(ctx, plan)
	require.NoError(t, err)
	require.Equal(t, 3, ca.orders)
}

// TestR169_AFailedRenewalKeepsTheOldCertificateAndWaits asserts a failed
// order leaves the certificate it would replace in service, says why, and is
// not retried until an hour has passed.
func TestR169_AFailedRenewalKeepsTheOldCertificateAndWaits(t *testing.T) {
	ctx := context.Background()
	i, _, ca, now := setup(t)
	_, err := i.Ensure(ctx, httpPlan())
	require.NoError(t, err)

	*now = now.Add(70 * 24 * time.Hour)
	ca.fail = errs.New(errs.AdapterFailed, "The certificate authority could not issue a certificate for a.example.com.")
	certs, err := i.Ensure(ctx, httpPlan())
	require.Error(t, err)
	require.Len(t, certs, 1, "the old certificate still serves")
	require.Equal(t, 2, ca.orders)

	_, err = i.Ensure(ctx, httpPlan())
	require.Error(t, err, "still reported")
	require.Equal(t, 2, ca.orders, "not retried within the hour")

	*now = now.Add(RetryAfter + time.Minute)
	ca.fail = nil
	_, err = i.Ensure(ctx, httpPlan())
	require.NoError(t, err)
	require.Equal(t, 3, ca.orders)
}

// TestR169_AnExpiredCertificateIsNotServed asserts an expired certificate is
// not handed to the edge while its renewal is failing.
func TestR169_AnExpiredCertificateIsNotServed(t *testing.T) {
	ctx := context.Background()
	i, _, ca, now := setup(t)
	_, err := i.Ensure(ctx, httpPlan())
	require.NoError(t, err)
	*now = now.Add(91 * 24 * time.Hour)
	ca.fail = errors.New("unreachable")
	certs, err := i.Ensure(ctx, httpPlan())
	require.Error(t, err)
	require.Empty(t, certs)
}

// TestR169_DNS01UsesTheFiveNamedProvidersFromTheirCredentials asserts the
// DNS-01 provider is built from the routing adapter's credentials, not from
// Pando's environment, and an unnamed provider is a readable refusal.
func TestR169_DNS01UsesTheFiveNamedProvidersFromTheirCredentials(t *testing.T) {
	p, err := DNSProvider("digitalocean", map[string]secret.Value{"DO_AUTH_TOKEN": secret.New("t")})
	require.NoError(t, err)
	require.NotNil(t, p)

	_, err = DNSProvider("porkbun", map[string]secret.Value{})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "missing credentials are named, not looked for in the environment")

	_, err = DNSProvider("ovh", nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "cloudflare")
}
