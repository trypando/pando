package hash_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/secret"
)

func TestRoundTrip(t *testing.T) {
	h, err := hash.New(secret.New("correct horse battery staple"))
	require.NoError(t, err)

	ok, err := hash.Verify(secret.New("correct horse battery staple"), h)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = hash.Verify(secret.New("wrong password"), h)
	require.NoError(t, err)
	require.False(t, ok)
}

// The stored hash must not contain the password, and must be salted so that two
// users with the same password do not share a hash.
func TestHashIsSaltedAndDoesNotContainTheSecret(t *testing.T) {
	const password = "hunter2-THE-ACTUAL-SECRET"

	a, err := hash.New(secret.New(password))
	require.NoError(t, err)
	b, err := hash.New(secret.New(password))
	require.NoError(t, err)

	require.NotContains(t, a, password)
	require.NotEqual(t, a, b, "identical passwords must not produce identical hashes")
	require.True(t, strings.HasPrefix(a, "$argon2id$"))
}

// A corrupted row must deny access, not crash the login path.
func TestMalformedHashDeniesRatherThanPanics(t *testing.T) {
	for _, bad := range []string{
		"",
		"not-a-hash",
		"$argon2id$v=19$m=65536,t=3,p=2$onlyfourfields",
		"$bcrypt$v=19$m=65536,t=3,p=2$c2FsdA$a2V5",
		"$argon2id$v=999$m=65536,t=3,p=2$c2FsdA$a2V5",
	} {
		ok, err := hash.Verify(secret.New("x"), bad)
		require.False(t, ok, "must not verify %q", bad)
		require.Error(t, err)
	}
}

// TestR042_AMalformedStoredHashDeniesRatherThanPanics asserts R-042.
//
// "Secure" for local users has to include behaving under a stored hash that is
// not what this package wrote. Found by FuzzVerifyEncodedHash: argon2.IDKey
// panics rather than erroring on a zero time cost or zero parallelism, and an
// empty key field compares equal to an empty candidate — so before the bounds
// check in decode, one malformed row crashed the sign-in path and another
// accepted every password. The upper bounds are from issue #78: a row naming
// 4 GiB of memory made every sign-in for that account allocate it.
func TestR042_AMalformedStoredHashDeniesRatherThanPanics(t *testing.T) {
	const goodSalt = "c2FsdHNhbHQ" // eight bytes, the RFC 9106 minimum
	// The derived-key field of a well-formed hash: sixteen bytes, base64.
	// Named goodDigest and spelled as words rather than "0123456789abcdef"
	// because a constant named *Key holding a base64 blob is what a secret
	// scanner is built to find, and .gitleaks.toml says the answer to that is
	// to fix the fixture rather than widen the allowlist.
	const goodDigest = "YSB2YWxpZCBrZXkgaGVyZQ" // "a valid key here"

	for name, encoded := range map[string]string{
		"zero time cost":     "$argon2id$v=19$m=65536,t=0,p=2$" + goodSalt + "$" + goodDigest,
		"zero parallelism":   "$argon2id$v=19$m=65536,t=3,p=0$" + goodSalt + "$" + goodDigest,
		"everything zero":    "$argon2id$v=19$m=0,t=0,p=0$$",
		"memory below floor": "$argon2id$v=19$m=4,t=3,p=2$" + goodSalt + "$" + goodDigest,
		"memory above cap":   "$argon2id$v=19$m=4294967295,t=3,p=2$" + goodSalt + "$" + goodDigest,
		"time above cap":     "$argon2id$v=19$m=65536,t=4294967295,p=2$" + goodSalt + "$" + goodDigest,
		"empty key":          "$argon2id$v=19$m=65536,t=3,p=2$" + goodSalt + "$",
		"short salt":         "$argon2id$v=19$m=65536,t=3,p=2$c2E$" + goodDigest,
		"truncated":          "$argon2id$",
		"empty":              "",
	} {
		t.Run(name, func(t *testing.T) {
			ok, err := hash.Verify(secret.New("any password at all"), encoded)
			require.Error(t, err, "a malformed hash must report an error")
			require.False(t, ok, "a malformed hash must never verify")
		})
	}
}
