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
	"github.com/trypando/pando/internal/secret"
)

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
