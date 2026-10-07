// Package hash derives and verifies password and token hashes with argon2id.
//
// Used for local user passwords (R-042). API token secrets were hashed here
// too until issue #93; they are now SHA-256 digests (state.apiTokenDigest),
// and Verify still reads a token stored before that, once, before it is
// rewritten. Kept out of core so adapters may use it without importing
// anything the R-027 boundary forbids.
package hash

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/trypando/pando/internal/secret"
)

// Parameters. The cost — time, memory, parallelism — is in cost.go. Encoded
// into every hash, so raising any of them later does not invalidate existing
// credentials — a hash carries the parameters it was made with.
const (
	saltLength = 16
	keyLength  = 32
)

// MinPasswordLength is the only rule a password has to satisfy.
//
// Here rather than beside any one of its callers because there are three — the
// API's self-service change, first-run bootstrap, and the reset command — and
// three copies of a number is three chances for one of them to drift low. The
// number itself is [P], and the reasoning is in design 04 §2.7: no composition
// classes, because a class requirement produces `Passw0rd!`, which has less
// real entropy than three words and is the password the rule reliably produces.
//
// A DR bundle's passphrase is longer (sixteen, design 07 D) and that difference
// is deliberate: a password is guessed against a server that rate-limits and
// can lock the account, and a passphrase protects a file an attacker already
// holds and can grind offline as fast as their hardware allows.
const MinPasswordLength = 10

// New derives an encoded hash of v in the standard argon2 string format.
func New(v secret.Value) (string, error) {
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}

	key := argon2.IDKey([]byte(v.Reveal()), salt, timeCost, memoryCost, parallelism, keyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryCost, timeCost, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether v matches the encoded hash.
//
// The comparison is constant time. A malformed hash returns false with an
// error rather than panicking, because a corrupted row must deny access, not
// crash the login path.
func Verify(v secret.Value, encoded string) (bool, error) {
	params, salt, key, err := decode(encoded)
	if err != nil {
		return false, err
	}

	candidate := argon2.IDKey([]byte(v.Reveal()), salt,
		//nolint:gosec // G115: decode bounds len(key) to maxKeyLength.
		params.time, params.memory, params.parallelism, uint32(len(key)))

	return subtle.ConstantTimeCompare(key, candidate) == 1, nil
}

type params struct {
	memory      uint32
	time        uint32
	parallelism uint8
}

func decode(encoded string) (params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return params{}, nil, nil, fmt.Errorf("malformed argon2id hash")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return params{}, nil, nil, fmt.Errorf("malformed argon2id version: %w", err)
	}
	if version != argon2.Version {
		return params{}, nil, nil, fmt.Errorf("unsupported argon2 version %d", version)
	}

	var p params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.parallelism); err != nil {
		return params{}, nil, nil, fmt.Errorf("malformed argon2id parameters: %w", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return params{}, nil, nil, fmt.Errorf("malformed argon2id salt: %w", err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return params{}, nil, nil, fmt.Errorf("malformed argon2id key: %w", err)
	}

	// Everything above parses; this rejects what parses but is not usable.
	// Found by FuzzVerifyEncodedHash, and both halves are worth stating:
	//
	// argon2.IDKey *panics* on t=0 or p=0 rather than returning an error, so
	// without this a stored hash carrying either takes the process down from
	// inside the sign-in path — the exact outcome Verify's doc comment
	// promises it will not have.
	//
	// An empty key is worse than a panic. subtle.ConstantTimeCompare reports a
	// match for two zero-length slices, so a hash ending in an empty key field
	// verifies true against every password. Writing one takes database access,
	// which is why this is a robustness rule and not an advisory — but a rule
	// whose absence turns a corrupted row into an authentication bypass is one
	// worth having.
	switch {
	case p.time < 1:
		return params{}, nil, nil, fmt.Errorf("argon2id time cost must be at least 1, got %d", p.time)
	case p.parallelism < 1:
		return params{}, nil, nil, fmt.Errorf("argon2id parallelism must be at least 1, got %d", p.parallelism)
	case p.memory < 8*uint32(p.parallelism):
		// argon2 silently raises memory to this floor, which would derive a
		// different key than the hash claims to hold. Refuse instead.
		return params{}, nil, nil, fmt.Errorf("argon2id memory cost %d is below the floor for parallelism %d",
			p.memory, p.parallelism)
	case len(salt) < minSaltLength:
		return params{}, nil, nil, fmt.Errorf("argon2id salt is %d bytes, minimum %d", len(salt), minSaltLength)
	case len(key) < minKeyLength || len(key) > maxKeyLength:
		return params{}, nil, nil, fmt.Errorf("argon2id key is %d bytes, expected %d to %d",
			len(key), minKeyLength, maxKeyLength)
	}

	return p, salt, key, nil
}

// Bounds on what decode will accept from a stored hash.
//
// A range rather than the exact keyLength this package writes, because the key
// length is the one parameter the encoding does not record: Verify derives its
// candidate at whatever length the stored key is, so a hash written by a
// version that used a different length still verifies. The range is what keeps
// that flexibility from including zero.
const (
	minSaltLength = 8 // RFC 9106 §4
	minKeyLength  = 16
	maxKeyLength  = 64
)
