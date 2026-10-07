//go:build integration

package state_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/secret"
)

// testTokenKey is the API token key the state tests use. In production it is
// a random file under /var/lib/pando (core/tokenkey).
var testTokenKey = []byte("0123456789abcdef0123456789abcdef")

// storedDigest reads a token's stored digest.
func storedDigest(t *testing.T, db *state.DB, tokenID string) string {
	t.Helper()
	var digest string
	require.NoError(t, db.QueryRow(context.Background(),
		`SELECT hash FROM tokens WHERE id = $1`, tokenID).Scan(&digest))
	return digest
}

// keyed is what a token's secret is stored as under key.
func keyed(key []byte, plaintext string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(plaintext))
	return "hmac-sha256:" + hex.EncodeToString(m.Sum(nil))
}

// TestR063_ATokenIsStoredAsAnHMACOfItsSecret asserts R-063 with the digest the
// product owner chose for issue #93: a token is stored as HMAC-SHA-256 of its
// secret under the install's token key, never the secret and never an unkeyed
// digest, and only the secret it was issued with authenticates it.
func TestR063_ATokenIsStoredAsAnHMACOfItsSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	tokens := state.NewTokens(db, testTokenKey)

	issued, err := tokens.Create(ctx, state.TokenDelegated, "ci", alice.ID, alice.ID, nil)
	require.NoError(t, err)
	_, plaintext, _ := strings.Cut(issued.Secret.Reveal(), ".")

	digest := storedDigest(t, db, issued.Token.ID)
	require.Equal(t, keyed(testTokenKey, plaintext), digest)
	require.NotContains(t, digest, plaintext)
	unkeyed := sha256.Sum256([]byte(plaintext))
	require.NotContains(t, digest, hex.EncodeToString(unkeyed[:]),
		"a dump alone must not be enough to check a guess")

	tok, err := tokens.Authenticate(ctx, issued.Secret)
	require.NoError(t, err)
	require.Equal(t, issued.Token.ID, tok.ID)

	wrong := secret.New(issued.Token.ID + "." + strings.Repeat("A", len(plaintext)))
	_, err = tokens.Authenticate(ctx, wrong)
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))

	otherKey := state.NewTokens(db, []byte("fedcba9876543210fedcba9876543210"))
	_, err = otherKey.Authenticate(ctx, issued.Secret)
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err), "the right secret under another key is refused")
}

// TestR063_OlderTokenDigestsStillWorkAndAreRewrittenOnUse asserts that a token
// stored before HMAC — as argon2id, from before issue #93, or as the unkeyed
// SHA-256 an earlier build for it wrote — keeps working, and is rewritten as
// HMAC-SHA-256 the first time it is used. A wrong secret neither
// authenticates nor rewrites it.
func TestR063_OlderTokenDigestsStillWorkAndAreRewrittenOnUse(t *testing.T) {
	t.Parallel()

	sha := func(plaintext string) string {
		sum := sha256.Sum256([]byte(plaintext))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	argon := func(plaintext string) string {
		d, err := hash.New(secret.New(plaintext))
		require.NoError(t, err)
		return d
	}

	for name, older := range map[string]func(string) string{"argon2id": argon, "sha256": sha} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := connected(t)
			alice := seedUser(t, db, "alice")
			tokens := state.NewTokens(db, testTokenKey)

			issued, err := tokens.Create(ctx, state.TokenDelegated, "old", alice.ID, alice.ID, nil)
			require.NoError(t, err)
			_, plaintext, _ := strings.Cut(issued.Secret.Reveal(), ".")

			old := older(plaintext)
			_, err = db.Exec(ctx, `UPDATE tokens SET hash = $2 WHERE id = $1`, issued.Token.ID, old)
			require.NoError(t, err)

			wrong := secret.New(issued.Token.ID + "." + strings.Repeat("B", len(plaintext)))
			_, err = tokens.Authenticate(ctx, wrong)
			require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
			require.Equal(t, old, storedDigest(t, db, issued.Token.ID), "a failed use rewrites nothing")

			_, err = tokens.Authenticate(ctx, issued.Secret)
			require.NoError(t, err)
			require.Equal(t, keyed(testTokenKey, plaintext), storedDigest(t, db, issued.Token.ID))

			_, err = tokens.Authenticate(ctx, issued.Secret)
			require.NoError(t, err, "and it works as rewritten")
		})
	}
}

// TestR063_AReplicaWithADifferentTokenKeyRefusesToStart asserts R-063 holds
// across replicas: the token key is not in the database, so a replica given a
// different one must stop before it rejects every token the others issued.
func TestR063_AReplicaWithADifferentTokenKeyRefusesToStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)

	require.NoError(t, state.NewTokens(db, testTokenKey).VerifyKey(ctx), "the first replica seeds the check")
	require.NoError(t, state.NewTokens(db, testTokenKey).VerifyKey(ctx), "a replica with the same key passes")

	err := state.NewTokens(db, []byte("fedcba9876543210fedcba9876543210")).VerifyKey(ctx)
	var e *errs.Error
	require.ErrorAs(t, err, &e, "a replica with its own key is refused")
	require.Contains(t, e.Message, "different API token key")
	require.Contains(t, e.Remedy, "same key file")
}

// TestR062_LastUseIsRecordedToTheMinute asserts R-062 without a write per
// request: last_used_at is set at first use, left alone by a use within a
// minute of it, and moved by a use after that.
func TestR062_LastUseIsRecordedToTheMinute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	tokens := state.NewTokens(db, testTokenKey)

	issued, err := tokens.Create(ctx, state.TokenDelegated, "loop", alice.ID, alice.ID, nil)
	require.NoError(t, err)

	lastUsed := func() *time.Time {
		var at *time.Time
		require.NoError(t, db.QueryRow(ctx, `SELECT last_used_at FROM tokens WHERE id = $1`, issued.Token.ID).Scan(&at))
		return at
	}
	setLastUsed := func(ago time.Duration) time.Time {
		at := time.Now().UTC().Add(-ago).Truncate(time.Microsecond)
		_, err := db.Exec(ctx, `UPDATE tokens SET last_used_at = $2 WHERE id = $1`, issued.Token.ID, at)
		require.NoError(t, err)
		return at
	}

	require.Nil(t, lastUsed())
	_, err = tokens.Authenticate(ctx, issued.Secret)
	require.NoError(t, err)
	require.NotNil(t, lastUsed(), "the first use is recorded")

	recent := setLastUsed(20 * time.Second)
	_, err = tokens.Authenticate(ctx, issued.Secret)
	require.NoError(t, err)
	require.True(t, recent.Equal(*lastUsed()), "a use within the minute writes nothing")

	stale := setLastUsed(2 * time.Minute)
	_, err = tokens.Authenticate(ctx, issued.Secret)
	require.NoError(t, err)
	require.True(t, lastUsed().After(stale), "a use after it moves the time")
}
