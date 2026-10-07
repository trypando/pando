//go:build integration

package state_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	secretslocal "github.com/trypando/pando/internal/adapter/secrets/local"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

func edgeStore(t *testing.T) (*state.DB, *state.EdgeCertificates) {
	t.Helper()
	ctx := context.Background()
	db := connected(t)
	sa := secretslocal.New()
	cfg, err := json.Marshal(map[string]string{"key_path": filepath.Join(t.TempDir(), "secrets.key")})
	require.NoError(t, err)
	require.NoError(t, sa.Configure(ctx, cfg))
	return db, state.NewEdgeCertificates(db, sa, "sek_local")
}

// TestR169_AFailedOrderIsRecordedBesideTheCertificateItWouldReplace asserts
// what the issuer relies on: a name never issued reads as absent, a failure
// alone reads back with its time and message and no certificate, a failure
// after an issuance keeps the certificate in service, and a new issuance
// clears the failure.
func TestR169_AFailedOrderIsRecordedBesideTheCertificateItWouldReplace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, store := edgeStore(t)
	name := "pando-tls-" + strings.ToLower(t.Name())

	_, ok, err := store.Certificate(ctx, name)
	require.NoError(t, err)
	require.False(t, ok, "never issued")

	at := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.RecordFailure(ctx, name, []string{"a.example.com"}, "the CA said no", at))
	got, ok, err := store.Certificate(ctx, name)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "the CA said no", got.LastError)
	require.True(t, got.AttemptedAt.Equal(at))
	require.True(t, got.NotAfter.IsZero())
	require.Empty(t, got.CertPEM, "a failure alone holds no certificate")
	require.Equal(t, []string{"a.example.com"}, got.Domains)

	notAfter := at.Add(90 * 24 * time.Hour)
	require.NoError(t, store.PutCertificate(ctx, state.EdgeCertificate{
		Name: name, Domains: []string{"a.example.com", "b.example.com"}, CertPEM: []byte("CHAIN"), KeyPEM: secret.New("KEY"), NotAfter: notAfter,
	}))
	got, _, err = store.Certificate(ctx, name)
	require.NoError(t, err)
	require.Empty(t, got.LastError, "issuing clears the failure")
	require.Equal(t, []string{"a.example.com", "b.example.com"}, got.Domains)

	later := at.Add(time.Hour)
	require.NoError(t, store.RecordFailure(ctx, name, []string{"c.example.com"}, "renewal refused", later))
	got, _, err = store.Certificate(ctx, name)
	require.NoError(t, err)
	require.Equal(t, "renewal refused", got.LastError)
	require.True(t, got.AttemptedAt.Equal(later))
	require.Equal(t, "CHAIN", string(got.CertPEM), "the certificate in service is kept")
	require.Equal(t, "KEY", got.KeyPEM.Reveal())
	require.Equal(t, []string{"a.example.com", "b.example.com"}, got.Domains, "and the names it covers")
}

// TestR169_TheACMEAccountIsKeptPerEmailAndDirectory asserts that the account
// key and registration read back for the CA they were made at, are replaced
// in place, and that another CA's directory has no account.
func TestR169_TheACMEAccountIsKeptPerEmailAndDirectory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, store := edgeStore(t)
	email := strings.ToLower(t.Name()) + "@example.com"

	_, ok, err := store.Account(ctx, email, "https://ca.test/dir")
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, store.PutAccount(ctx, state.AcmeAccount{Email: email, Directory: "https://ca.test/dir", KeyPEM: secret.New("KEY-1")}))
	got, ok, err := store.Account(ctx, email, "https://ca.test/dir")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "KEY-1", got.KeyPEM.Reveal())
	require.Empty(t, got.Registration, "registered later")

	reg := json.RawMessage(`{"uri":"https://ca.test/acct/1"}`)
	require.NoError(t, store.PutAccount(ctx, state.AcmeAccount{Email: email, Directory: "https://ca.test/dir", KeyPEM: secret.New("KEY-2"), Registration: reg}))
	got, _, err = store.Account(ctx, email, "https://ca.test/dir")
	require.NoError(t, err)
	require.Equal(t, "KEY-2", got.KeyPEM.Reveal())
	require.JSONEq(t, string(reg), string(got.Registration))
	require.Equal(t, email, got.Email)

	_, ok, err = store.Account(ctx, email, "https://other-ca.test/dir")
	require.NoError(t, err)
	require.False(t, ok, "an account is per CA")
}

// TestR169_AChallengeIsAnsweredUntilItExpiresOrIsWithdrawn asserts the
// HTTP-01 answers every replica's proxy reads: answered while pending,
// replaced on a second Present, gone once withdrawn, never answered once
// expired, and withdrawing one sweeps the expired.
func TestR169_AChallengeIsAnsweredUntilItExpiresOrIsWithdrawn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, store := edgeStore(t)
	token := strings.ToLower(t.Name())
	stale := token + "-stale"

	require.NoError(t, store.PutChallenge(ctx, token, "ka-1", "a.example.com", time.Now().Add(time.Minute)))
	require.NoError(t, store.PutChallenge(ctx, token, "ka-2", "a.example.com", time.Now().Add(time.Minute)))
	ka, ok, err := store.Challenge(ctx, token)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ka-2", ka)

	require.NoError(t, store.PutChallenge(ctx, stale, "ka-old", "b.example.com", time.Now().Add(-time.Minute)))
	_, ok, err = store.Challenge(ctx, stale)
	require.NoError(t, err)
	require.False(t, ok, "an expired challenge is not answered")

	require.NoError(t, store.DeleteChallenge(ctx, token))
	_, ok, err = store.Challenge(ctx, token)
	require.NoError(t, err)
	require.False(t, ok)
	var left int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM edge_acme_challenges WHERE token = $1`, stale).Scan(&left))
	require.Zero(t, left, "withdrawing one challenge sweeps the expired")
}

// TestR190_WithoutASecretsAdapterNoCertificateIsKept asserts that the store
// refuses rather than keep a key in the clear (R-190), and says what to do.
func TestR190_WithoutASecretsAdapterNoCertificateIsKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sealed := edgeStore(t)
	name := "pando-tls-" + strings.ToLower(t.Name())
	email := name + "@example.com"
	require.NoError(t, sealed.PutCertificate(ctx, state.EdgeCertificate{Name: name, Domains: []string{"a.example.com"}, CertPEM: []byte("C"), KeyPEM: secret.New("K"), NotAfter: time.Now().Add(time.Hour)}))
	require.NoError(t, sealed.PutAccount(ctx, state.AcmeAccount{Email: email, Directory: "d", KeyPEM: secret.New("K")}))

	bare := state.NewEdgeCertificates(db, nil, "")
	err := bare.PutCertificate(ctx, state.EdgeCertificate{Name: name + "-x", KeyPEM: secret.New("K")})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "Configure a secrets adapter")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(bare.PutAccount(ctx, state.AcmeAccount{Email: email + "x", KeyPEM: secret.New("K")})))
	_, _, err = bare.Certificate(ctx, name)
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	_, _, err = bare.Account(ctx, email, "d")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
}

// TestR169_AnUnreachableDatabaseIsAnInternalErrorNotAMissingCertificate
// asserts that a failed read is never mistaken for "no certificate", which
// would have the issuer order one on every pass.
func TestR169_AnUnreachableDatabaseIsAnInternalErrorNotAMissingCertificate(t *testing.T) {
	t.Parallel()
	_, store := edgeStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := store.Certificate(ctx, "c")
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	_, _, err = store.Account(ctx, "e", "d")
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	_, _, err = store.Challenge(ctx, "t")
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Equal(t, errs.Internal, errs.CodeOf(store.RecordFailure(ctx, "c", nil, "m", time.Now())))
	require.Equal(t, errs.Internal, errs.CodeOf(store.PutChallenge(ctx, "t", "k", "d", time.Now())))
	require.Equal(t, errs.Internal, errs.CodeOf(store.DeleteChallenge(ctx, "t")))
}

// TestR190_EdgeCertificatesAndTheAccountKeyAreStoredOnlyAsCiphertext asserts
// R-190 for the certificates Pando issues for the edge: no column holds a key
// or a certificate in the clear, and both read back whole.
func TestR190_EdgeCertificatesAndTheAccountKeyAreStoredOnlyAsCiphertext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	sa := secretslocal.New()
	cfg, err := json.Marshal(map[string]string{"key_path": filepath.Join(t.TempDir(), "secrets.key")})
	require.NoError(t, err)
	require.NoError(t, sa.Configure(ctx, cfg))
	store := state.NewEdgeCertificates(db, sa, "sek_local")

	const key = "-----BEGIN EC PRIVATE KEY-----must-never-be-stored-in-the-clear"
	name := "pando-tls-" + strings.ToLower(t.Name())
	notAfter := time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Second)
	require.NoError(t, store.PutCertificate(ctx, state.EdgeCertificate{
		Name: name, Domains: []string{"a.example.com"}, CertPEM: []byte("CERTIFICATE"), KeyPEM: secret.New(key), NotAfter: notAfter,
	}))
	require.NoError(t, store.PutAccount(ctx, state.AcmeAccount{Email: name + "@example.com", Directory: "https://ca.test/dir", KeyPEM: secret.New(key)}))

	var rows string
	require.NoError(t, db.QueryRow(ctx, `
		SELECT coalesce(string_agg(encode(ciphertext, 'escape'), ''), '') FROM (
			SELECT ciphertext FROM edge_certificates UNION ALL SELECT ciphertext FROM edge_acme_accounts) t`).Scan(&rows))
	require.NotContains(t, rows, "must-never-be-stored")
	require.NotContains(t, rows, "CERTIFICATE")

	got, ok, err := store.Certificate(ctx, name)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, key, got.KeyPEM.Reveal())
	require.Equal(t, "CERTIFICATE", string(got.CertPEM))
	require.True(t, got.NotAfter.Equal(notAfter))

	require.NoError(t, store.PutChallenge(ctx, name, "ka", "a.example.com", time.Now().Add(time.Minute)))
	ka, ok, err := store.Challenge(ctx, name)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ka", ka)
}
