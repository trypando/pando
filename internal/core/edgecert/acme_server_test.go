package edgecert

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/go-acme/lego/v4/certcrypto"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// acmeServer is an in-process ACME CA (RFC 8555) sufficient for lego to
// register, order, answer HTTP-01 and finalize. It does not check JWS
// signatures; it does check, as a CA would, that the HTTP-01 answer is
// published before it validates the challenge.
type acmeServer struct {
	t   *testing.T
	srv *httptest.Server

	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate

	mu            sync.Mutex
	registrations int
	accountKeys   []string // thumbprints of the JWKs that registered
	domains       []string // the order's identifiers
	issuedNames   []string // the SANs of the last certificate issued
	lastCert      []byte
	// published reports the key authorization the client published for a
	// token, the CA's view of http://<domain>/.well-known/acme-challenge/<token>.
	published func(domain, token string) (string, bool)
	// refuseFinalize makes finalization fail with a CA error.
	refuseFinalize bool
}

func newACMEServer(t *testing.T) *acmeServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fake ACME Root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	s := &acmeServer{t: t, caKey: key, caCert: caCert}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *acmeServer) url(p string) string { return s.srv.URL + p }

func (s *acmeServer) client() Lego { return Lego{HTTPClient: s.srv.Client()} }

// jws is the flattened JSON serialization lego posts.
type jws struct {
	Protected string `json:"protected"`
	Payload   string `json:"payload"`
}

func (s *acmeServer) decode(r *http.Request) (header map[string]any, payload []byte) {
	var body jws
	require.NoError(s.t, json.NewDecoder(r.Body).Decode(&body))
	h, err := base64.RawURLEncoding.DecodeString(body.Protected)
	require.NoError(s.t, err)
	require.NoError(s.t, json.Unmarshal(h, &header))
	payload, err = base64.RawURLEncoding.DecodeString(body.Payload)
	require.NoError(s.t, err)
	return header, payload
}

func (s *acmeServer) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *acmeServer) problem(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "urn:ietf:params:acme:error:" + typ, "detail": detail, "status": status})
}

func (s *acmeServer) authz(i int, status string) map[string]any {
	chal := map[string]any{"type": "http-01", "url": s.url(fmt.Sprintf("/chal/%d", i)), "token": fmt.Sprintf("token%d", i), "status": status}
	return map[string]any{
		"status":     status,
		"identifier": map[string]string{"type": "dns", "value": s.domains[i]},
		"challenges": []any{chal},
	}
}

func (s *acmeServer) order(status string) map[string]any {
	ids := make([]map[string]string, len(s.domains))
	authzs := make([]string, len(s.domains))
	for i, d := range s.domains {
		ids[i] = map[string]string{"type": "dns", "value": d}
		authzs[i] = s.url(fmt.Sprintf("/authz/%d", i))
	}
	o := map[string]any{"status": status, "identifiers": ids, "authorizations": authzs, "finalize": s.url("/finalize")}
	if status == "valid" {
		o["certificate"] = s.url("/cert")
	}
	return o
}

func (s *acmeServer) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", time.Now().UnixNano()))
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case r.URL.Path == "/dir":
		s.reply(w, http.StatusOK, map[string]string{
			"newNonce": s.url("/nonce"), "newAccount": s.url("/account"),
			"newOrder": s.url("/order"), "revokeCert": s.url("/revoke"), "keyChange": s.url("/key"),
		})
	case r.URL.Path == "/nonce":
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/account":
		header, _ := s.decode(r)
		raw, err := json.Marshal(header["jwk"])
		require.NoError(s.t, err)
		var jwk jose.JSONWebKey
		require.NoError(s.t, jwk.UnmarshalJSON(raw))
		tp, err := jwk.Thumbprint(crypto.SHA256)
		require.NoError(s.t, err)
		s.registrations++
		s.accountKeys = append(s.accountKeys, base64.RawURLEncoding.EncodeToString(tp))
		w.Header().Set("Location", s.url("/acct/1"))
		s.reply(w, http.StatusCreated, map[string]any{"status": "valid"})
	case r.URL.Path == "/order":
		_, payload := s.decode(r)
		var req struct {
			Identifiers []struct{ Value string } `json:"identifiers"`
		}
		require.NoError(s.t, json.Unmarshal(payload, &req))
		s.domains = nil
		for _, id := range req.Identifiers {
			s.domains = append(s.domains, id.Value)
		}
		w.Header().Set("Location", s.url("/orders/1"))
		s.reply(w, http.StatusCreated, s.order("pending"))
	case strings.HasPrefix(r.URL.Path, "/authz/"):
		var i int
		_, _ = fmt.Sscanf(r.URL.Path, "/authz/%d", &i)
		s.reply(w, http.StatusOK, s.authz(i, "pending"))
	case strings.HasPrefix(r.URL.Path, "/chal/"):
		var i int
		_, _ = fmt.Sscanf(r.URL.Path, "/chal/%d", &i)
		token := fmt.Sprintf("token%d", i)
		// The CA fetches the answer from the hostname. Here it asks the
		// solver what it published, and expects token.thumbprint (RFC 8555 §8.1).
		got, ok := s.published(s.domains[i], token)
		want := token + "." + s.accountKeys[len(s.accountKeys)-1]
		if !ok || got != want {
			s.problem(w, http.StatusForbidden, "unauthorized", fmt.Sprintf("the answer at %s was %q, not %q", s.domains[i], got, want))
			return
		}
		s.reply(w, http.StatusOK, map[string]any{"type": "http-01", "url": r.URL.String(), "token": token, "status": "valid"})
	case r.URL.Path == "/finalize":
		if s.refuseFinalize {
			s.problem(w, http.StatusForbidden, "rejectedIdentifier", "the CA will not issue for this name")
			return
		}
		_, payload := s.decode(r)
		var req struct {
			CSR string `json:"csr"`
		}
		require.NoError(s.t, json.Unmarshal(payload, &req))
		der, err := base64.RawURLEncoding.DecodeString(req.CSR)
		require.NoError(s.t, err)
		csr, err := x509.ParseCertificateRequest(der)
		require.NoError(s.t, err)
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: csr.Subject.CommonName},
			DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(90 * 24 * time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		certDER, err := x509.CreateCertificate(rand.Reader, leaf, s.caCert, csr.PublicKey, s.caKey)
		require.NoError(s.t, err)
		s.issuedNames = csr.DNSNames
		s.lastCert = certDER
		w.Header().Set("Location", s.url("/orders/1"))
		s.reply(w, http.StatusOK, s.order("valid"))
	case r.URL.Path == "/orders/1":
		s.reply(w, http.StatusOK, s.order("valid"))
	case r.URL.Path == "/cert":
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		w.WriteHeader(http.StatusOK)
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: s.lastCert})
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: s.caCert.Raw})
	default:
		s.problem(w, http.StatusNotFound, "malformed", "no such resource "+r.URL.Path)
	}
}

// publishingSolver is the HTTP-01 answer as Pando's proxy would serve it.
type publishingSolver struct {
	mu        sync.Mutex
	answers   map[string]string // domain/token -> key authorization
	presented []string
	cleaned   []string
}

func (p *publishingSolver) Present(_ context.Context, domain, token, keyAuth string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.answers == nil {
		p.answers = map[string]string{}
	}
	p.answers[domain+"/"+token] = keyAuth
	p.presented = append(p.presented, domain)
	return nil
}

func (p *publishingSolver) CleanUp(_ context.Context, domain, token string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.answers, domain+"/"+token)
	p.cleaned = append(p.cleaned, domain)
	return nil
}

func (p *publishingSolver) lookup(domain, token string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.answers[domain+"/"+token]
	return v, ok
}

// TestR169_LegoRegistersAnswersHTTP01AndReturnsTheChain asserts R-169 against
// a CA speaking ACME: a first order registers an account and keeps its key, the
// HTTP-01 answer is published through Pando's solver and withdrawn after, and
// the certificate that comes back covers the names asked for. A second order
// with the returned account reuses the registration rather than making another.
func TestR169_LegoRegistersAnswersHTTP01AndReturnsTheChain(t *testing.T) {
	ca := newACMEServer(t)
	solver := &publishingSolver{}
	ca.published = solver.lookup
	ctx := context.Background()

	account := state.AcmeAccount{Email: "ops@example.com", Directory: ca.url("/dir")}
	names := []string{"app.example.com", "www.example.com"}
	account, issued, err := ca.client().Obtain(ctx, account, names, Solver{Challenge: api.ChallengeHTTP01, HTTP: solver})
	require.NoError(t, err)

	require.False(t, account.KeyPEM.IsZero(), "the new account key is returned to be stored")
	require.NotEmpty(t, account.Registration, "the registration is returned to be stored")
	require.Equal(t, 1, ca.registrations)

	require.ElementsMatch(t, names, solver.presented, "each name's answer was published")
	require.ElementsMatch(t, names, solver.cleaned, "and withdrawn afterwards")
	require.Empty(t, solver.answers)

	block, _ := pem.Decode(issued.CertPEM)
	require.NotNil(t, block)
	leaf, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.ElementsMatch(t, names, leaf.DNSNames)
	key, err := certcrypto.ParsePEMPrivateKey([]byte(issued.KeyPEM.Reveal()))
	require.NoError(t, err)
	require.Equal(t, leaf.PublicKey, key.(crypto.Signer).Public(), "the key returned is the certificate's")
	require.Equal(t, "[redacted]", fmt.Sprint(issued.KeyPEM), "R-194: the certificate key is a secret")

	// The stored account is reused: the same key, no second registration.
	again, _, err := ca.client().Obtain(ctx, account, names[:1], Solver{Challenge: api.ChallengeHTTP01, HTTP: solver})
	require.NoError(t, err)
	require.Equal(t, 1, ca.registrations, "a stored registration is not registered again")
	require.Equal(t, account.KeyPEM.Reveal(), again.KeyPEM.Reveal())
	require.Equal(t, []string{names[0]}, ca.issuedNames)
}

// TestR169_ACAThatRefusesIsAnAdapterErrorWithARemedy asserts that the CA's
// refusals reach the operator as R-105 errors: a challenge the CA could not
// validate, a refused finalization, and a CA that cannot be reached.
func TestR169_ACAThatRefusesIsAnAdapterErrorWithARemedy(t *testing.T) {
	ctx := context.Background()

	t.Run("the HTTP-01 answer is not where the CA looks", func(t *testing.T) {
		ca := newACMEServer(t)
		ca.published = func(string, string) (string, bool) { return "", false }
		solver := &publishingSolver{}
		account := state.AcmeAccount{Email: "ops@example.com", Directory: ca.url("/dir")}
		account, _, err := ca.client().Obtain(ctx, account, []string{"app.example.com"}, Solver{Challenge: api.ChallengeHTTP01, HTTP: solver})
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		require.ErrorContains(t, err, "issue a certificate for app.example.com")
		require.Contains(t, errs.As(err).Remedy, "port 80")
		require.NotEmpty(t, account.Registration, "the account registered before the order failed is still returned")
		require.Equal(t, []string{"app.example.com"}, solver.cleaned, "a failed answer is still withdrawn")
	})

	t.Run("finalization is refused", func(t *testing.T) {
		ca := newACMEServer(t)
		solver := &publishingSolver{}
		ca.published = solver.lookup
		ca.refuseFinalize = true
		_, _, err := ca.client().Obtain(ctx, state.AcmeAccount{Directory: ca.url("/dir")}, []string{"app.example.com"}, Solver{Challenge: api.ChallengeHTTP01, HTTP: solver})
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	})

	t.Run("the CA cannot be reached", func(t *testing.T) {
		gone := httptest.NewServer(http.NotFoundHandler())
		gone.Close()
		_, _, err := Lego{}.Obtain(ctx, state.AcmeAccount{Directory: gone.URL + "/dir"}, []string{"app.example.com"}, Solver{Challenge: api.ChallengeHTTP01, HTTP: &publishingSolver{}})
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		require.ErrorContains(t, err, "reach the certificate authority")
	})
}

// TestR169_AnObtainThatCannotBeginIsRefusedBeforeTheCAIsAsked asserts the
// refusals Obtain makes itself: an unreadable stored key, a challenge Pando
// does not know, an HTTP-01 order with no answer to publish, and a DNS
// provider outside the named five. None of them registers an account.
func TestR169_AnObtainThatCannotBeginIsRefusedBeforeTheCAIsAsked(t *testing.T) {
	ca := newACMEServer(t)
	ca.published = func(string, string) (string, bool) { return "", false }
	ctx := context.Background()
	fresh := state.AcmeAccount{Directory: ca.url("/dir")}

	_, _, err := ca.client().Obtain(ctx, state.AcmeAccount{Directory: ca.url("/dir"), KeyPEM: secret.New("not a key")}, []string{"a.example.com"}, Solver{Challenge: api.ChallengeHTTP01, HTTP: &publishingSolver{}})
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.ErrorContains(t, err, "account key could not be read")

	_, _, err = ca.client().Obtain(ctx, fresh, []string{"a.example.com"}, Solver{Challenge: "tls-alpn-01"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.ErrorContains(t, err, "Valid answers: http-01, dns-01.")

	_, _, err = ca.client().Obtain(ctx, fresh, []string{"a.example.com"}, Solver{Challenge: api.ChallengeHTTP01})
	require.Equal(t, errs.Internal, errs.CodeOf(err))

	_, _, err = ca.client().Obtain(ctx, fresh, []string{"a.example.com"}, Solver{Challenge: api.ChallengeDNS01, DNSProvider: "ovh"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))

	require.Zero(t, ca.registrations)
}

// TestR169_EachNamedDNSProviderIsBuiltFromTheSettingsCredentials asserts that
// the remaining named providers are configured from the credentials passed,
// not from Pando's environment, without reaching the provider.
func TestR169_EachNamedDNSProviderIsBuiltFromTheSettingsCredentials(t *testing.T) {
	for code, creds := range map[string]map[string]secret.Value{
		"cloudflare": {"CF_DNS_API_TOKEN": secret.New("cf-token")},
		"route53":    {"AWS_ACCESS_KEY_ID": secret.New("AKIA"), "AWS_SECRET_ACCESS_KEY": secret.New("s"), "AWS_REGION": secret.New("us-east-1"), "AWS_HOSTED_ZONE_ID": secret.New("Z1")},
		"namecheap":  {"NAMECHEAP_API_USER": secret.New("u"), "NAMECHEAP_API_KEY": secret.New("k"), "NAMECHEAP_CLIENT_IP": secret.New("203.0.113.7")},
	} {
		p, err := DNSProvider(code, creds)
		require.NoError(t, err, code)
		require.NotNil(t, p, code)
	}
	_, err := DNSProvider("cloudflare", nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "cloudflare with no credentials")
	require.ErrorContains(t, err, "The cloudflare DNS provider could not be set up")
}
