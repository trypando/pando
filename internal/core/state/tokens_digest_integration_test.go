//go:build integration

package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/secret"
)

// storedDigest reads a token's stored digest.
func storedDigest(t *testing.T, db *state.DB, tokenID string) string {
	t.Helper()
	var digest string
	require.NoError(t, db.QueryRow(context.Background(),
		`SELECT hash FROM tokens WHERE id = $1`, tokenID).Scan(&digest))
	return digest
}

// TestR063_ATokenIsStoredAsADigestOfItsSecret asserts R-063 with the digest
// issue #93 chose: a token is stored as a SHA-256 digest of its secret, never
// the secret, and only the secret it was issued with authenticates it.
func TestR063_ATokenIsStoredAsADigestOfItsSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	tokens := state.NewTokens(db)

	issued, err := tokens.Create(ctx, state.TokenDelegated, "ci", alice.ID, alice.ID, nil)
	require.NoError(t, err)
	_, plaintext, _ := strings.Cut(issued.Secret.Reveal(), ".")

	digest := storedDigest(t, db, issued.Token.ID)
	require.True(t, strings.HasPrefix(digest, "sha256:"), digest)
	require.NotContains(t, digest, plaintext)

	tok, err := tokens.Authenticate(ctx, issued.Secret)
	require.NoError(t, err)
	require.Equal(t, issued.Token.ID, tok.ID)

	wrong := secret.New(issued.Token.ID + "." + strings.Repeat("A", len(plaintext)))
	_, err = tokens.Authenticate(ctx, wrong)
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
}

// TestR063_AnArgon2idTokenStillWorksAndIsRewrittenOnUse asserts that a token
// stored before issue #93, as an argon2id digest, keeps working, and is
// rewritten as a SHA-256 digest the first time it is used. A wrong secret
// neither authenticates nor rewrites it.
func TestR063_AnArgon2idTokenStillWorksAndIsRewrittenOnUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	tokens := state.NewTokens(db)

	issued, err := tokens.Create(ctx, state.TokenDelegated, "old", alice.ID, alice.ID, nil)
	require.NoError(t, err)
	_, plaintext, _ := strings.Cut(issued.Secret.Reveal(), ".")

	old, err := hash.New(secret.New(plaintext))
	require.NoError(t, err)
	_, err = db.Exec(ctx, `UPDATE tokens SET hash = $2 WHERE id = $1`, issued.Token.ID, old)
	require.NoError(t, err)

	wrong := secret.New(issued.Token.ID + "." + strings.Repeat("B", len(plaintext)))
	_, err = tokens.Authenticate(ctx, wrong)
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
	require.Equal(t, old, storedDigest(t, db, issued.Token.ID), "a failed use rewrites nothing")

	_, err = tokens.Authenticate(ctx, issued.Secret)
	require.NoError(t, err)
	rewritten := storedDigest(t, db, issued.Token.ID)
	require.True(t, strings.HasPrefix(rewritten, "sha256:"), rewritten)

	_, err = tokens.Authenticate(ctx, issued.Secret)
	require.NoError(t, err, "and it works as rewritten")
}

// TestR062_LastUseIsRecordedToTheMinute asserts R-062 without a write per
// request: last_used_at is set at first use, left alone by a use within a
// minute of it, and moved by a use after that.
func TestR062_LastUseIsRecordedToTheMinute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	tokens := state.NewTokens(db)

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
