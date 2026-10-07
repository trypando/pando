// Package edgecert issues the edge's certificates when the edge cannot issue
// its own (R-169, R-174; notes-kubernetes-runtime-issue-72.md, "Certificates
// with several replicas").
//
// On Kubernetes the edge is several Traefik replicas. Each ordering its own
// certificates would multiply Let's Encrypt rate-limit use and fail HTTP-01
// whenever the CA's request reached another replica, and Traefik's open-source
// edition has no leader election for ACME. So Pando's leader is the one ACME
// client, in its edge pass (a cluster.Job): it orders what the routing adapter
// asks for, renews ahead of expiry, keeps every certificate and the account key
// sealed in Postgres (R-190), and hands them to the runtime, which writes them
// where every replica reads them. HTTP-01 challenges are answered by Pando's
// proxy from the database, on whichever replica the CA's request reaches.
//
// lego is the ACME client, the library Traefik itself uses, so the DNS-01
// provider codes and credential variables the Traefik adapter's settings
// already name mean the same thing here.
package edgecert

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Defaults [P].
const (
	// RenewBefore is how long before expiry a certificate is renewed. Let's
	// Encrypt issues for 90 days and recommends renewing at 60.
	RenewBefore = 30 * 24 * time.Hour

	// RetryAfter is how long a failed order waits before it is tried again,
	// so a hostname whose DNS is wrong does not spend the CA's limit of five
	// failed validations an hour.
	RetryAfter = time.Hour

	// LetsEncrypt is the CA used unless another directory is set.
	LetsEncrypt = "https://acme-v02.api.letsencrypt.org/directory"

	// challengeLifetime bounds how long a pending HTTP-01 answer is served.
	challengeLifetime = 15 * time.Minute
)

// Store is what the issuer keeps. *state.EdgeCertificates in production.
type Store interface {
	Certificate(ctx context.Context, name string) (state.EdgeCertificate, bool, error)
	PutCertificate(ctx context.Context, c state.EdgeCertificate) error
	RecordFailure(ctx context.Context, name string, domains []string, message string, at time.Time) error
	Account(ctx context.Context, email, directory string) (state.AcmeAccount, bool, error)
	PutAccount(ctx context.Context, a state.AcmeAccount) error
	PutChallenge(ctx context.Context, token, keyAuthorization, domain string, expires time.Time) error
	DeleteChallenge(ctx context.Context, token string) error
	Challenge(ctx context.Context, token string) (string, bool, error)
}

// ACME orders one certificate. Lego in production; a fake in tests.
type ACME interface {
	// Obtain orders a certificate for domains with the account, proving
	// control with solver. It returns the account as the CA now knows it — a
	// new key and registration the first time — and the certificate.
	Obtain(ctx context.Context, account state.AcmeAccount, domains []string, solver Solver) (state.AcmeAccount, Issued, error)
}

// Issued is a certificate chain and its key, in PEM.
type Issued struct {
	CertPEM []byte
	KeyPEM  secret.Value
}

// Solver is how an order proves control of its names.
type Solver struct {
	Challenge string // api.ChallengeHTTP01 or api.ChallengeDNS01

	// HTTP answers HTTP-01: Present makes the key authorization available
	// at the token's path on every replica, CleanUp withdraws it.
	HTTP HTTPSolver

	DNSProvider    string
	DNSCredentials map[string]secret.Value
}

// HTTPSolver publishes HTTP-01 answers.
type HTTPSolver interface {
	Present(ctx context.Context, domain, token, keyAuthorization string) error
	CleanUp(ctx context.Context, domain, token string) error
}

// Issuer orders, renews and hands out the edge's certificates.
type Issuer struct {
	Store  Store
	ACME   ACME
	Clock  clock.Clock
	Logger *zap.Logger

	// Directory is the ACME directory URL; LetsEncrypt when empty.
	Directory string
}

func (i *Issuer) now() time.Time {
	if i.Clock == nil {
		return time.Now().UTC()
	}
	return i.Clock.Now().UTC()
}

func (i *Issuer) directory() string {
	if i.Directory == "" {
		return LetsEncrypt
	}
	return i.Directory
}

func (i *Issuer) logger() *zap.Logger {
	if i.Logger == nil {
		return zap.NewNop()
	}
	return i.Logger
}

// Ensure orders every certificate the plan asks for that is missing, covers
// other names, or is within RenewBefore of expiry, and returns every
// certificate it holds for the plan that has not expired. An order that fails
// leaves the certificate it replaces in service; the failure is returned with
// the certificates, so the edge still serves what there is.
func (i *Issuer) Ensure(ctx context.Context, plan *api.CertificateIssue) ([]api.EdgeCertificate, error) {
	if plan == nil {
		return nil, nil
	}
	now := i.now()
	var (
		out      []api.EdgeCertificate
		failures []error
	)
	for _, order := range plan.Orders {
		held, found, err := i.Store.Certificate(ctx, order.Name)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		valid := found && !held.NotAfter.IsZero() && held.NotAfter.After(now)
		current := valid && sameNames(held.Domains, order.Domains) && held.NotAfter.Sub(now) > RenewBefore
		waiting := found && held.LastError != "" && now.Sub(held.AttemptedAt) < RetryAfter

		if !current && !waiting {
			issued, err := i.order(ctx, plan, order)
			if err != nil {
				i.logger().Warn("certificate not issued", zap.String("certificate", order.Name), zap.Error(err))
				if rerr := i.Store.RecordFailure(ctx, order.Name, order.Domains, errText(err), now); rerr != nil {
					failures = append(failures, rerr)
				}
				failures = append(failures, err)
			} else {
				held, valid = issued, true
			}
		} else if waiting && !current && held.LastError != "" {
			failures = append(failures, errs.Newf(errs.AdapterFailed,
				"The certificate for %s could not be issued, and Pando tries again after %s: %s",
				order.Domains[0], held.AttemptedAt.Add(RetryAfter).Format(time.RFC3339), held.LastError))
		}

		// A certificate for other names than the order's is still served
		// until its replacement is issued: a renamed console is better
		// served with the old name's certificate than with none.
		if valid {
			out = append(out, api.EdgeCertificate{Name: order.Name, CertPEM: held.CertPEM, KeyPEM: held.KeyPEM})
		}
	}
	return out, errors.Join(failures...)
}

// order runs one ACME order and stores what it produced.
func (i *Issuer) order(ctx context.Context, plan *api.CertificateIssue, order api.CertificateOrder) (state.EdgeCertificate, error) {
	if len(order.Domains) == 0 {
		return state.EdgeCertificate{}, errs.Newf(errs.Internal, "The certificate %s names no hostnames.", order.Name)
	}
	if plan.Email == "" {
		return state.EdgeCertificate{}, errs.New(errs.ValidInvalid,
			"Let's Encrypt needs an email address to issue certificates, and the routing adapter's settings do not have one.").
			WithRemedy("Set the certificate email address in the routing adapter's settings.")
	}
	account, _, err := i.Store.Account(ctx, plan.Email, i.directory())
	if err != nil {
		return state.EdgeCertificate{}, err
	}
	account.Email, account.Directory = plan.Email, i.directory()

	solver := Solver{Challenge: plan.Challenge, DNSProvider: plan.DNSProvider, DNSCredentials: plan.DNSCredentials}
	if plan.Challenge == api.ChallengeHTTP01 {
		solver.HTTP = storeSolver{store: i.Store, now: i.now}
	}
	updated, issued, err := i.ACME.Obtain(ctx, account, order.Domains, solver)
	if !updated.KeyPEM.IsZero() && (account.KeyPEM.Reveal() != updated.KeyPEM.Reveal() || string(account.Registration) != string(updated.Registration)) {
		// Kept even when the order failed: the CA knows the account now.
		if perr := i.Store.PutAccount(ctx, updated); perr != nil {
			return state.EdgeCertificate{}, perr
		}
	}
	if err != nil {
		return state.EdgeCertificate{}, err
	}

	notAfter, err := expiry(issued.CertPEM)
	if err != nil {
		return state.EdgeCertificate{}, err
	}
	c := state.EdgeCertificate{
		Name: order.Name, Domains: order.Domains,
		CertPEM: issued.CertPEM, KeyPEM: issued.KeyPEM, NotAfter: notAfter,
	}
	if err := i.Store.PutCertificate(ctx, c); err != nil {
		return state.EdgeCertificate{}, err
	}
	i.logger().Info("certificate issued", zap.String("certificate", order.Name),
		zap.Strings("domains", order.Domains), zap.Time("not_after", notAfter))
	return c, nil
}

// expiry reads the leaf certificate's NotAfter.
func expiry(chain []byte) (time.Time, error) {
	block, _ := pem.Decode(chain)
	if block == nil {
		return time.Time{}, errs.New(errs.AdapterFailed, "The certificate authority returned something that is not a certificate.")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, errs.Wrap(errs.AdapterFailed, "The certificate authority returned a certificate Pando could not read.", err)
	}
	return cert.NotAfter.UTC(), nil
}

func sameNames(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func errText(err error) string {
	if e := errs.As(err); e != nil {
		return e.Message
	}
	return err.Error()
}

// storeSolver publishes HTTP-01 answers in the database, where the proxy on
// every replica reads them.
type storeSolver struct {
	store Store
	now   func() time.Time
}

func (s storeSolver) Present(ctx context.Context, domain, token, keyAuthorization string) error {
	return s.store.PutChallenge(ctx, token, keyAuthorization, domain, s.now().Add(challengeLifetime))
}

func (s storeSolver) CleanUp(ctx context.Context, _, token string) error {
	return s.store.DeleteChallenge(ctx, token)
}

// Challenges answers HTTP-01 requests from the database, for the router.
type Challenges struct {
	Store interface {
		Challenge(ctx context.Context, token string) (string, bool, error)
	}
}

// KeyAuthorization is the answer for a pending token.
func (c Challenges) KeyAuthorization(ctx context.Context, token string) (string, bool, error) {
	if c.Store == nil {
		return "", false, nil
	}
	ka, ok, err := c.Store.Challenge(ctx, token)
	if err != nil {
		return "", false, fmt.Errorf("reading the challenge: %w", err)
	}
	return ka, ok, nil
}
