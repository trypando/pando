package assertion_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/clock"
)

func TestMintAndVerify(t *testing.T) {
	m, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	token, err := m.Mint(assertion.Claims{Sub: "usr_alice", Email: "alice@corp.com", Aud: "app_01HQ8"})
	require.NoError(t, err)

	claims, err := m.Verify(token)
	require.NoError(t, err)
	require.Equal(t, "usr_alice", claims.Sub)
	require.Equal(t, "app_01HQ8", claims.Aud)
	require.Equal(t, "https://pando.test", claims.Iss)
}

// R-055: the lifetime is one constant. A test that hard-coded 120 would pass
// while the constant and the behavior drifted apart.
func TestR055_LifetimeComesFromTheConstant(t *testing.T) {
	fake := clock.NewFake(time.Time{})
	m, err := assertion.NewMinter("https://pando.test", fake)
	require.NoError(t, err)

	token, err := m.Mint(assertion.Claims{Sub: "usr_alice", Aud: "app_01HQ8"})
	require.NoError(t, err)

	claims, err := m.Verify(token)
	require.NoError(t, err)
	require.Equal(t, int64(assertion.Lifetime/time.Second), claims.Exp-claims.Iat)

	// Still valid a second before expiry, refused after.
	fake.Advance(assertion.Lifetime - time.Second)
	_, err = m.Verify(token)
	require.NoError(t, err)

	fake.Advance(2 * time.Second)
	_, err = m.Verify(token)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expired")
}

// R-057: rotation is an overlap, not a cliff. An assertion signed by the old key
// must keep verifying until it expires, or every in-flight request breaks at the
// moment of rotation.
func TestR057_RotationOverlapsRatherThanCutsOver(t *testing.T) {
	fake := clock.NewFake(time.Time{})
	m, err := assertion.NewMinter("https://pando.test", fake)
	require.NoError(t, err)

	first := m.SigningKeyID()
	oldToken, err := m.Mint(assertion.Claims{Sub: "usr_alice", Aud: "app_01HQ8"})
	require.NoError(t, err)

	require.NoError(t, m.Rotate())
	second := m.SigningKeyID()
	require.NotEqual(t, first, second, "rotation produces a new signing key")

	_, err = m.Verify(oldToken)
	require.NoError(t, err, "an assertion signed before rotation still verifies")

	// Both keys are published, so an app that cached the JWKS before rotation
	// and one that fetches after can both verify.
	jwks := m.JWKS(context.Background())
	ids := keyIDs(t, jwks)
	require.Contains(t, ids, first)
	require.Contains(t, ids, second)

	// Once everything the old key signed has expired, it stops being published.
	fake.Advance(assertion.Lifetime + time.Second)
	require.NoError(t, m.Rotate())
	require.NotContains(t, keyIDs(t, m.JWKS(context.Background())), first,
		"a key that can no longer verify anything is not published")
}

// A caller must not get to pick the verification scheme — the "alg: none" family
// of attacks.
func TestAlgorithmConfusionIsRefused(t *testing.T) {
	m, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	token, err := m.Mint(assertion.Claims{Sub: "usr_alice", Aud: "app_01HQ8"})
	require.NoError(t, err)
	parts := strings.Split(token, ".")

	for _, alg := range []string{"none", "HS256", "RS256"} {
		header, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": m.SigningKeyID()})
		forged := base64.RawURLEncoding.EncodeToString(header) + "." + parts[1] + "." + parts[2]

		_, err := m.Verify(forged)
		require.Error(t, err, "alg %s must be refused", alg)
		require.Contains(t, err.Error(), "not signed the way Pando signs")
	}
}

func TestTamperedPayloadIsRefused(t *testing.T) {
	m, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	token, err := m.Mint(assertion.Claims{Sub: "usr_bob", Aud: "app_01HQ8"})
	require.NoError(t, err)
	parts := strings.Split(token, ".")

	// Swap the subject for someone else's, keeping the original signature.
	payload, _ := json.Marshal(assertion.Claims{
		Sub: "usr_alice", Aud: "app_01HQ8",
		Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(),
	})
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]

	_, err = m.Verify(forged)
	require.Error(t, err)
	require.Contains(t, err.Error(), "signature does not match")
}

func TestUnknownKeyIsRefused(t *testing.T) {
	a, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	b, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	token, err := b.Mint(assertion.Claims{Sub: "usr_alice", Aud: "app_01HQ8"})
	require.NoError(t, err)

	_, err = a.Verify(token)
	require.Error(t, err, "an assertion from another install must not verify here")
}

func TestMalformedTokensAreRefused(t *testing.T) {
	m, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	for _, bad := range []string{"", "a", "a.b", "a.b.c.d", "!!!.???.###"} {
		_, err := m.Verify(bad)
		require.Error(t, err, "should refuse %q", bad)
	}
}

// The JWKS is what apps verify against, so its shape is a contract.
func TestJWKSShape(t *testing.T) {
	m, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	keys, ok := m.JWKS(context.Background())["keys"].([]map[string]string)
	require.True(t, ok)
	require.Len(t, keys, 1)

	k := keys[0]
	require.Equal(t, "OKP", k["kty"])
	require.Equal(t, "Ed25519", k["crv"])
	require.Equal(t, "EdDSA", k["alg"])
	require.Equal(t, "sig", k["use"])
	require.NotEmpty(t, k["kid"])
	require.NotEmpty(t, k["x"])

	// The public key is published; nothing else is.
	encoded, err := json.Marshal(m.JWKS(context.Background()))
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private")
	require.NotContains(t, strings.ToLower(string(encoded)), "\"d\"", "no private scalar")
}

func keyIDs(t *testing.T, jwks map[string]any) []string {
	t.Helper()
	keys, ok := jwks["keys"].([]map[string]string)
	require.True(t, ok)

	var out []string
	for _, k := range keys {
		out = append(out, k["kid"])
	}
	return out
}

// TestR051_AnyReplicasJWKSVerifiesAnyReplicasAssertion asserts R-051 across
// replicas (issue #72). Each Pando process signs with a key of its own; an app
// that fetched the JWKS from one replica must still verify an assertion
// another signed, or the load balancer decides whom an app believes.
func TestR051_AnyReplicasJWKSVerifiesAnyReplicasAssertion(t *testing.T) {
	a, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	b, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	require.NotEqual(t, a.SigningKeyID(), b.SigningKeyID(), "each replica has a key of its own")

	published := func(ms ...*assertion.Minter) assertion.PeerKeys {
		return func(context.Context) ([]assertion.PublicKey, error) {
			var out []assertion.PublicKey
			for _, m := range ms {
				out = append(out, m.SigningKey())
			}
			return out, nil
		}
	}
	a.WithPeers(published(a, b))
	b.WithPeers(published(a, b))

	token, err := b.Mint(assertion.Claims{Sub: "usr_alice", Aud: "app_01HQ8"})
	require.NoError(t, err)

	// Verify as an app would: from the JWKS replica A serves, and nothing else.
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	var h struct{ Kid string }
	require.NoError(t, json.Unmarshal(header, &h))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)

	keys, ok := a.JWKS(context.Background())["keys"].([]map[string]string)
	require.True(t, ok)
	verified := false
	for _, k := range keys {
		if k["kid"] != h.Kid {
			continue
		}
		pub, err := base64.RawURLEncoding.DecodeString(k["x"])
		require.NoError(t, err)
		verified = ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig)
	}
	require.True(t, verified, "replica A's JWKS verifies what replica B signed")

	// A peer list that cannot be read still publishes the replica's own key.
	c, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	c.WithPeers(func(context.Context) ([]assertion.PublicKey, error) { return nil, context.DeadlineExceeded })
	require.Equal(t, []string{c.SigningKeyID()}, keyIDs(t, c.JWKS(context.Background())))
}
