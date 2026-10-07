package edgecert

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// brokenStore fails the one method named in failing.
type brokenStore struct {
	*memStore
	failing string
}

var errStore = errors.New("the database is unreachable")

func (b brokenStore) Certificate(ctx context.Context, name string) (state.EdgeCertificate, bool, error) {
	if b.failing == "Certificate" {
		return state.EdgeCertificate{}, false, errStore
	}
	return b.memStore.Certificate(ctx, name)
}

func (b brokenStore) PutCertificate(ctx context.Context, c state.EdgeCertificate) error {
	if b.failing == "PutCertificate" {
		return errStore
	}
	return b.memStore.PutCertificate(ctx, c)
}

func (b brokenStore) RecordFailure(ctx context.Context, name string, domains []string, msg string, at time.Time) error {
	if b.failing == "RecordFailure" {
		return errStore
	}
	return b.memStore.RecordFailure(ctx, name, domains, msg, at)
}

func (b brokenStore) Account(ctx context.Context, email, dir string) (state.AcmeAccount, bool, error) {
	if b.failing == "Account" {
		return state.AcmeAccount{}, false, errStore
	}
	return b.memStore.Account(ctx, email, dir)
}

func (b brokenStore) PutAccount(ctx context.Context, a state.AcmeAccount) error {
	if b.failing == "PutAccount" {
		return errStore
	}
	return b.memStore.PutAccount(ctx, a)
}

// TestR169_AStoreFailureIsReportedAndNothingIsServedFromIt asserts that the
// issuer reports each store failure rather than treating it as "no certificate"
// and ordering one, and that an order whose result cannot be kept is not
// served: a certificate the other replicas cannot read would diverge.
func TestR169_AStoreFailureIsReportedAndNothingIsServedFromIt(t *testing.T) {
	for _, method := range []string{"Certificate", "Account", "PutAccount", "PutCertificate", "RecordFailure"} {
		t.Run(method, func(t *testing.T) {
			i, store, ca, _ := setup(t)
			i.Store = brokenStore{memStore: store, failing: method}
			if method == "RecordFailure" {
				ca.fail = errors.New("the CA is down")
			}
			certs, err := i.Ensure(context.Background(), httpPlan())
			require.ErrorIs(t, err, errStore)
			require.Empty(t, certs)
		})
	}
}

// TestR105_AnOrderThatCannotBeMadeSaysWhatIsMissing asserts the refusals made
// before the CA is asked, each a message the operator can act on.
func TestR105_AnOrderThatCannotBeMadeSaysWhatIsMissing(t *testing.T) {
	i, _, _, _ := setup(t)

	certs, err := i.Ensure(context.Background(), nil)
	require.NoError(t, err)
	require.Nil(t, certs, "no plan, nothing to issue")

	plan := httpPlan()
	plan.Email = ""
	_, err = i.Ensure(context.Background(), plan)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.ErrorContains(t, err, "needs an email address")
	require.Contains(t, errs.As(err).Remedy, "routing adapter's settings")

	i, _, ca, _ := setup(t)
	plan = httpPlan()
	plan.Orders[0].Domains = nil
	_, err = i.Ensure(context.Background(), plan)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.ErrorContains(t, err, "names no hostnames")
	require.Zero(t, ca.orders)

	// Within the retry window the same order reports the recorded failure,
	// naming the certificate, rather than indexing its empty hostname list.
	_, err = i.Ensure(context.Background(), plan)
	require.ErrorContains(t, err, "The certificate for pando-tls-a.example.com could not be issued")
	require.ErrorContains(t, err, "names no hostnames")
}

// badChainCA returns a "certificate" that is not one.
type badChainCA struct{ chain []byte }

func (b badChainCA) Obtain(_ context.Context, acct state.AcmeAccount, _ []string, _ Solver) (state.AcmeAccount, Issued, error) {
	return acct, Issued{CertPEM: b.chain, KeyPEM: secret.New("k")}, nil
}

// TestR169_ACertificateThatCannotBeReadIsNotStored asserts that a CA response
// that is not a certificate, or is one Pando cannot parse, is an adapter error
// and is never kept.
func TestR169_ACertificateThatCannotBeReadIsNotStored(t *testing.T) {
	for name, chain := range map[string][]byte{
		"not PEM":         []byte("hello"),
		"PEM, not a cert": []byte("-----BEGIN CERTIFICATE-----\naGVsbG8=\n-----END CERTIFICATE-----\n"),
	} {
		t.Run(name, func(t *testing.T) {
			i, store, _, _ := setup(t)
			i.ACME = badChainCA{chain: chain}
			certs, err := i.Ensure(context.Background(), httpPlan())
			require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
			require.Empty(t, certs)
			held, _, _ := store.Certificate(context.Background(), "pando-tls-a.example.com")
			require.Empty(t, held.CertPEM)
			require.NotEmpty(t, held.LastError, "the failure is recorded for the retry window")
		})
	}
}

// TestR169_AnIssuerWithNoSettingsUsesLetsEncryptAndTheWallClock asserts the
// defaults: Let's Encrypt's directory, the real clock, and a silent logger.
func TestR169_AnIssuerWithNoSettingsUsesLetsEncryptAndTheWallClock(t *testing.T) {
	store := newStore(func() time.Time { return time.Now().UTC() })
	var dir string
	ca := &recordingCA{dir: &dir}
	i := &Issuer{Store: store, ACME: ca}
	before := time.Now().UTC()
	_, err := i.Ensure(context.Background(), &api.CertificateIssue{Email: "ops@example.com", Challenge: api.ChallengeDNS01, DNSProvider: "cloudflare",
		Orders: []api.CertificateOrder{{Name: "c", Domains: []string{"a.example.com"}}}})
	require.Error(t, err)
	require.Equal(t, LetsEncrypt, dir)
	held, ok, _ := store.Certificate(context.Background(), "c")
	require.True(t, ok)
	require.False(t, held.AttemptedAt.Before(before), "the failure is stamped with the wall clock")
}

type recordingCA struct{ dir *string }

func (r *recordingCA) Obtain(_ context.Context, acct state.AcmeAccount, _ []string, s Solver) (state.AcmeAccount, Issued, error) {
	*r.dir = acct.Directory
	if s.HTTP != nil {
		return acct, Issued{}, errors.New("DNS-01 was asked for; no HTTP answer should be set up")
	}
	return acct, Issued{}, errors.New("refused")
}

type failingChallenges struct{}

func (failingChallenges) Challenge(context.Context, string) (string, bool, error) {
	return "", false, errStore
}

// TestR169_TheRouterAnswersNoChallengeWithoutAStore asserts the router's view:
// with no store there is no pending answer, and a store failure is an error
// rather than a silent "not found".
func TestR169_TheRouterAnswersNoChallengeWithoutAStore(t *testing.T) {
	ka, ok, err := Challenges{}.KeyAuthorization(context.Background(), "tok")
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, ka)

	_, _, err = Challenges{Store: failingChallenges{}}.KeyAuthorization(context.Background(), "tok")
	require.ErrorIs(t, err, errStore)
}
