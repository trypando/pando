package edgecert

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/providers/dns/digitalocean"
	"github.com/go-acme/lego/v4/providers/dns/namecheap"
	"github.com/go-acme/lego/v4/providers/dns/porkbun"
	"github.com/go-acme/lego/v4/providers/dns/route53"
	"github.com/go-acme/lego/v4/registration"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Lego is the ACME client: lego, which Traefik also uses.
type Lego struct {
	// HTTPClient reaches the CA. Nil is lego's default.
	HTTPClient *http.Client
}

// ClientTrusting is an HTTP client for the CA that trusts the certificates in
// caFile beside the system's roots: a private ACME server, or a test CA such
// as Pebble, whose own certificate no public root signs (O-49). Empty caFile
// is nil, lego's default client.
func ClientTrusting(caFile string) (*http.Client, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile) //nolint:gosec // an operator's setting
	if err != nil {
		return nil, fmt.Errorf("could not read PANDO_ACME_CA_FILE %s: %w", caFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("PANDO_ACME_CA_FILE %s holds no PEM certificate", caFile)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}

// legoUser is the account as lego asks for it.
type legoUser struct {
	email string
	reg   *registration.Resource
	key   crypto.PrivateKey
}

func (u *legoUser) GetEmail() string                        { return u.email }
func (u *legoUser) GetRegistration() *registration.Resource { return u.reg }
func (u *legoUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

// Obtain orders one certificate.
func (l Lego) Obtain(ctx context.Context, account state.AcmeAccount, domains []string, solver Solver) (state.AcmeAccount, Issued, error) {
	user := &legoUser{email: account.Email}
	if account.KeyPEM.IsZero() {
		key, err := certcrypto.GeneratePrivateKey(certcrypto.EC256)
		if err != nil {
			return account, Issued{}, errs.Wrap(errs.Internal, "Could not make a key for the certificate account.", err)
		}
		user.key = key
		account.KeyPEM = secret.New(string(certcrypto.PEMEncode(key)))
		account.Registration = nil
	} else {
		key, err := certcrypto.ParsePEMPrivateKey([]byte(account.KeyPEM.Reveal()))
		if err != nil {
			return account, Issued{}, errs.Wrap(errs.Internal, "The stored certificate account key could not be read.", err)
		}
		user.key = key
	}
	if len(account.Registration) > 0 {
		var reg registration.Resource
		if err := json.Unmarshal(account.Registration, &reg); err == nil && reg.URI != "" {
			user.reg = &reg
		}
	}

	cfg := lego.NewConfig(user)
	cfg.CADirURL = account.Directory
	cfg.Certificate.KeyType = certcrypto.EC256
	if l.HTTPClient != nil {
		cfg.HTTPClient = l.HTTPClient
	}
	client, err := lego.NewClient(cfg)
	if err != nil {
		return account, Issued{}, caError("reach the certificate authority", err)
	}

	switch solver.Challenge {
	case api.ChallengeHTTP01:
		if solver.HTTP == nil {
			return account, Issued{}, errs.New(errs.Internal, "No HTTP-01 answer was set up.")
		}
		if err := client.Challenge.SetHTTP01Provider(httpProvider{ctx: ctx, solver: solver.HTTP}); err != nil {
			return account, Issued{}, caError("set up the HTTP challenge", err)
		}
	case api.ChallengeDNS01:
		provider, err := DNSProvider(solver.DNSProvider, solver.DNSCredentials)
		if err != nil {
			return account, Issued{}, err
		}
		if err := client.Challenge.SetDNS01Provider(provider); err != nil {
			return account, Issued{}, caError("set up the DNS challenge", err)
		}
	default:
		return account, Issued{}, errs.Newf(errs.ValidInvalid, "%q is not a way Pando can prove control of a hostname. Valid answers: http-01, dns-01.", solver.Challenge)
	}

	if user.reg == nil {
		reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return account, Issued{}, caError("register an account", err)
		}
		user.reg = reg
		raw, err := json.Marshal(reg)
		if err != nil {
			return account, Issued{}, errs.Wrap(errs.Internal, "Could not record the certificate account.", err)
		}
		account.Registration = raw
	}

	res, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: domains, Bundle: true})
	if err != nil {
		return account, Issued{}, caError(fmt.Sprintf("issue a certificate for %s", strings.Join(domains, ", ")), err)
	}
	return account, Issued{CertPEM: res.Certificate, KeyPEM: secret.New(string(res.PrivateKey))}, nil
}

func caError(what string, err error) error {
	return errs.Wrap(errs.AdapterFailed, "The certificate authority could not "+what+".", err).
		WithRemedy("Check that each hostname's DNS points at the edge's address and that port 80 reaches it from the internet, or, for DNS-01, that the DNS provider credentials can change the zone. Pando tries again within the hour.")
}

// httpProvider is lego's HTTP-01 provider, publishing answers through Pando's
// proxy rather than serving them itself.
type httpProvider struct {
	ctx    context.Context //nolint:containedctx // lego's interface carries none
	solver HTTPSolver
}

func (p httpProvider) Present(domain, token, keyAuth string) error {
	return p.solver.Present(p.ctx, domain, token, keyAuth)
}

func (p httpProvider) CleanUp(domain, token, _ string) error {
	return p.solver.CleanUp(p.ctx, domain, token)
}

// NamedDNSProviders are the DNS-01 providers Pando issues through itself, the
// five the Traefik adapter offers by name (design 03 §4.4).
var NamedDNSProviders = []string{"cloudflare", "route53", "digitalocean", "porkbun", "namecheap"}

// DNSProvider builds a DNS-01 provider from the settings' code and variables,
// read from the credentials given rather than from Pando's environment.
func DNSProvider(code string, creds map[string]secret.Value) (challenge.Provider, error) {
	get := func(k string) string { return creds[k].Reveal() }
	var (
		p   challenge.Provider
		err error
	)
	switch code {
	case "cloudflare":
		cfg := cloudflare.NewDefaultConfig()
		cfg.AuthToken, cfg.AuthEmail, cfg.AuthKey = get("CF_DNS_API_TOKEN"), get("CF_API_EMAIL"), get("CF_API_KEY")
		cfg.ZoneToken = get("CF_ZONE_API_TOKEN")
		p, err = cloudflare.NewDNSProviderConfig(cfg)
	case "route53":
		cfg := route53.NewDefaultConfig()
		cfg.AccessKeyID, cfg.SecretAccessKey = get("AWS_ACCESS_KEY_ID"), get("AWS_SECRET_ACCESS_KEY")
		cfg.SessionToken, cfg.Region, cfg.HostedZoneID = get("AWS_SESSION_TOKEN"), get("AWS_REGION"), get("AWS_HOSTED_ZONE_ID")
		p, err = route53.NewDNSProviderConfig(cfg)
	case "digitalocean":
		cfg := digitalocean.NewDefaultConfig()
		cfg.AuthToken = get("DO_AUTH_TOKEN")
		p, err = digitalocean.NewDNSProviderConfig(cfg)
	case "porkbun":
		cfg := porkbun.NewDefaultConfig()
		cfg.APIKey, cfg.SecretAPIKey = get("PORKBUN_API_KEY"), get("PORKBUN_SECRET_API_KEY")
		p, err = porkbun.NewDNSProviderConfig(cfg)
	case "namecheap":
		cfg := namecheap.NewDefaultConfig()
		cfg.APIUser, cfg.APIKey, cfg.ClientIP = get("NAMECHEAP_API_USER"), get("NAMECHEAP_API_KEY"), get("NAMECHEAP_CLIENT_IP")
		p, err = namecheap.NewDNSProviderConfig(cfg)
	default:
		names := append([]string(nil), NamedDNSProviders...)
		sort.Strings(names)
		return nil, errs.Newf(errs.ValidInvalid,
			"Pando issues the edge's certificates itself on this runtime, and can use the DNS providers %s. %q is not one of them.",
			strings.Join(names, ", "), code).
			WithRemedy("Choose one of those DNS providers, or use one certificate per hostname (HTTP-01) instead.")
	}
	if err != nil {
		return nil, errs.Wrap(errs.ValidInvalid, fmt.Sprintf("The %s DNS provider could not be set up from its credentials.", code), err).
			WithRemedy("Check the DNS provider credentials in the routing adapter's settings.")
	}
	return p, nil
}
