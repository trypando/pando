package twofactor

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// RecoveryCodeCount is how many recovery codes a set holds.
const RecoveryCodeCount = 10

// recoveryAlphabet is base32 in lower case without the letters most easily
// misread on paper (i, l, o, u), so every character is 5 bits and a code
// copied by hand comes back as it was.
const recoveryAlphabet = "abcdefghjkmnpqrstvwxyz0123456789"

// newRecoveryCodes is a fresh set: each sixteen characters, 80 random bits,
// in four groups of four.
func newRecoveryCodes() ([]string, error) {
	out := make([]string, RecoveryCodeCount)
	raw := make([]byte, 16)
	for i := range out {
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		var b strings.Builder
		for j, c := range raw {
			if j > 0 && j%4 == 0 {
				b.WriteByte('-')
			}
			b.WriteByte(recoveryAlphabet[int(c)%len(recoveryAlphabet)])
		}
		out[i] = b.String()
	}
	return out, nil
}

// normalizeRecoveryCode is a code as stored: lower case, without the
// separators and spaces a person may or may not have typed.
func normalizeRecoveryCode(s string) string {
	s = strings.ToLower(s)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, s)
}

// recoveryDigest is what is kept of a recovery code. SHA-256 rather than
// argon2id, as with a passcode unlock: the code is 80 random bits, so there is
// nothing small to search and a slow hash would only slow sign-in.
func recoveryDigest(code string) string {
	sum := sha256.Sum256([]byte(normalizeRecoveryCode(code))) // codeql[go/weak-sensitive-data-hashing]
	return hex.EncodeToString(sum[:])
}
