// Package assertion mints the identity assertions the proxy passes to apps, and
// publishes the keys that verify them.
//
// An assertion is the only thing an app can trust about who is calling. The
// convenience headers alongside it are explicitly unverified (R-053), so the
// signature here is what separates "Pando says this is Alice" from "a client
// typed Alice into a header".
package assertion

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/errs"
)

// Lifetime is how long an assertion is valid (R-055).
//
// It is also the interval at which long-lived connections are re-authorized
// (O-13) and the width of the revocation window (design 06 §3.1). One constant,
// referenced from all three places: two clocks measuring the same thing drift
// apart the first time someone tunes one of them.
const Lifetime = 120 * time.Second

// AnonymousSubject is the constant `sub` for an unauthenticated request (R-056).
//
// A constant rather than an empty claim, so an app can tell "Pando says nobody is
// signed in" from "this request did not come through Pando" — the latter being
// the absence of the assertion header entirely.
const AnonymousSubject = "anonymous"

// Header is where the assertion travels.
const Header = "X-Pando-Assertion"

// Claims is the assertion body.
type Claims struct {
	// Sub is users.id, stable across email change and independent of the
	// identity adapter (R-054). Apps key their data on it, which makes it the
	// single most important stability guarantee in the system.
	Sub string `json:"sub"`

	Email  string   `json:"email,omitempty"`
	Name   string   `json:"name,omitempty"`
	Groups []string `json:"groups,omitempty"`

	// Aud is the app ID. It is what stops an assertion minted for one app being
	// replayed against another.
	Aud string `json:"aud"`

	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Iss string `json:"iss"`
}

// Key is an Ed25519 signing key with an identifier.
type Key struct {
	ID      string
	Public  ed25519.PublicKey
	private ed25519.PrivateKey

	// NotAfter is when this key stops being used for signing. It stays in the
	// JWKS past that point so assertions already issued still verify — which is
	// what makes rotation an overlap rather than a cliff (R-057).
	NotAfter time.Time
}

// Minter signs assertions and publishes the verifying keys.
type Minter struct {
	mu      sync.RWMutex
	issuer  string
	clock   clock.Clock
	signing *Key
	retired []*Key
	peers   PeerKeys
}

// NewMinter builds a minter with a freshly generated key.
func NewMinter(issuer string, c clock.Clock) (*Minter, error) {
	if c == nil {
		c = clock.System{}
	}
	m := &Minter{issuer: issuer, clock: c}
	if err := m.Rotate(); err != nil {
		return nil, err
	}
	return m, nil
}

// Rotate generates a new signing key and retires the current one.
//
// The retired key keeps verifying until every assertion it signed has expired,
// which is at most Lifetime. Publishing both and switching the signer is the
// propagation window R-057 asks for.
func (m *Minter) Rotate() error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not generate a signing key.", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.signing != nil {
		m.signing.NotAfter = m.clock.Now()
		m.retired = append(m.retired, m.signing)
	}
	m.signing = &Key{ID: keyID(pub), Public: pub, private: priv}

	// Drop keys whose assertions have all expired. Keeping them would publish a
	// growing list of keys that can no longer verify anything.
	cutoff := m.clock.Now().Add(-Lifetime)
	kept := m.retired[:0]
	for _, k := range m.retired {
		if k.NotAfter.After(cutoff) {
			kept = append(kept, k)
		}
	}
	m.retired = kept
	return nil
}

// Mint signs an assertion for one request.
//
// Minted per request rather than cached: the lifetime is short enough that
// caching buys little, and a cached assertion would outlive a revocation by
// however long the cache held it.
func (m *Minter) Mint(c Claims) (string, error) {
	m.mu.RLock()
	key := m.signing
	issuer := m.issuer
	m.mu.RUnlock()

	if key == nil {
		return "", errs.New(errs.Internal, "No signing key is available.")
	}
	if c.Aud == "" {
		// Without an audience an assertion is replayable against every app on
		// the install. Refusing here means a caller that forgot cannot produce
		// one by accident.
		return "", errs.New(errs.Internal, "An assertion must name the app it is for.")
	}

	now := m.clock.Now()
	c.Iat = now.Unix()
	c.Exp = now.Add(Lifetime).Unix()
	c.Iss = issuer

	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": key.ID})
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not build the assertion.", err)
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not build the assertion.", err)
	}

	signingInput := encode(header) + "." + encode(payload)
	sig := ed25519.Sign(key.private, []byte(signingInput))
	return signingInput + "." + encode(sig), nil
}

// Verify checks an assertion and returns its claims.
//
// Provided so Pando's own tests and tooling can verify what it mints. Apps
// verify independently using the published JWKS — that is the point of
// publishing it.
func (m *Minter) Verify(token string) (Claims, error) {
	headerB64, payloadB64, sigB64, ok := split(token)
	if !ok {
		return Claims{}, errs.New(errs.AuthInvalid, "That assertion is malformed.")
	}

	headerJSON, err := decode(headerB64)
	if err != nil {
		return Claims{}, errs.New(errs.AuthInvalid, "That assertion is malformed.")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return Claims{}, errs.New(errs.AuthInvalid, "That assertion is malformed.")
	}
	if header.Alg != "EdDSA" {
		// Refusing an unexpected algorithm outright is what stops the "alg: none"
		// family of attacks, where a caller picks the verification scheme.
		return Claims{}, errs.New(errs.AuthInvalid, "That assertion is not signed the way Pando signs.")
	}

	sig, err := decode(sigB64)
	if err != nil {
		return Claims{}, errs.New(errs.AuthInvalid, "That assertion is malformed.")
	}

	key := m.keyByID(header.Kid)
	if key == nil {
		return Claims{}, errs.New(errs.AuthInvalid, "That assertion was signed with a key Pando does not publish.")
	}
	if !ed25519.Verify(key.Public, []byte(headerB64+"."+payloadB64), sig) {
		return Claims{}, errs.New(errs.AuthInvalid, "That assertion's signature does not match.")
	}

	payload, err := decode(payloadB64)
	if err != nil {
		return Claims{}, errs.New(errs.AuthInvalid, "That assertion is malformed.")
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, errs.New(errs.AuthInvalid, "That assertion is malformed.")
	}

	if m.clock.Now().Unix() >= claims.Exp {
		return Claims{}, errs.New(errs.AuthInvalid, "That assertion has expired.")
	}
	return claims, nil
}

// PeerKeys lists the verifying keys of the other Pando processes serving this
// install (issue #72).
//
// Each replica signs with a key of its own that never leaves its memory, and
// publishes the public half where the others can read it. An app may fetch the
// JWKS from one replica and receive an assertion signed by another, so every
// replica's JWKS has to carry every key that may have signed something still
// valid — otherwise the app sees a kid nobody publishes and refuses a caller
// Pando vouched for (R-051).
type PeerKeys func(ctx context.Context) ([]PublicKey, error)

// PublicKey is a verifying key, without the private half.
type PublicKey struct {
	ID     string
	Public ed25519.PublicKey
}

// WithPeers sets where the other replicas' keys are read from.
func (m *Minter) WithPeers(p PeerKeys) *Minter {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peers = p
	return m
}

// SigningKey is the public half of the key this process signs with, for
// publishing to the other replicas.
func (m *Minter) SigningKey() PublicKey {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.signing == nil {
		return PublicKey{}
	}
	return PublicKey{ID: m.signing.ID, Public: m.signing.Public}
}

// JWKS returns the public keys, for publication at /.well-known/jwks.json:
// this process's own, and every peer's.
//
// Read from the peers per request rather than cached. An app that meets an
// unknown kid fetches the JWKS again, and a cached list would answer that fetch
// without the key it came for. If the peers cannot be read the process's own
// keys are still published: the alternative is publishing nothing, which fails
// every app rather than some.
func (m *Minter) JWKS(ctx context.Context) map[string]any {
	m.mu.RLock()
	own := make([]PublicKey, 0, len(m.retired)+1)
	if m.signing != nil {
		own = append(own, PublicKey{ID: m.signing.ID, Public: m.signing.Public})
	}
	for _, k := range m.retired {
		own = append(own, PublicKey{ID: k.ID, Public: k.Public})
	}
	peers := m.peers
	m.mu.RUnlock()

	all := own
	if peers != nil {
		if theirs, err := peers(ctx); err == nil {
			all = append(all, theirs...)
		}
	}

	seen := make(map[string]bool, len(all))
	keys := make([]map[string]string, 0, len(all))
	for _, k := range all {
		if seen[k.ID] || len(k.Public) != ed25519.PublicKeySize {
			continue
		}
		seen[k.ID] = true
		keys = append(keys, map[string]string{
			"kty": "OKP",
			"crv": "Ed25519",
			"use": "sig",
			"alg": "EdDSA",
			"kid": k.ID,
			"x":   base64.RawURLEncoding.EncodeToString(k.Public),
		})
	}
	return map[string]any{"keys": keys}
}

// KeyID is the kid of a public key: derived from the key, so every replica
// names a peer's key the same way without being told.
func KeyID(pub ed25519.PublicKey) string { return keyID(pub) }

// SigningKeyID returns the key currently used to sign.
func (m *Minter) SigningKeyID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.signing == nil {
		return ""
	}
	return m.signing.ID
}

func (m *Minter) keyByID(id string) *Key {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.signing != nil && m.signing.ID == id {
		return m.signing
	}
	for _, k := range m.retired {
		if k.ID == id {
			return k
		}
	}
	return nil
}

// keyID derives a stable identifier from the public key, so the same key always
// has the same kid without anything needing to be stored alongside it.
func keyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func split(token string) (header, payload, sig string, ok bool) {
	first := -1
	second := -1
	for i := 0; i < len(token); i++ {
		if token[i] != '.' {
			continue
		}
		if first < 0 {
			first = i
		} else if second < 0 {
			second = i
		} else {
			return "", "", "", false
		}
	}
	if first < 0 || second < 0 {
		return "", "", "", false
	}
	return token[:first], token[first+1 : second], token[second+1:], true
}
