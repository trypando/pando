// Package twofactor is the second factor of a local account (R-405): an
// authenticator app's time-based codes (TOTP, RFC 6238) and recovery codes.
//
// Local accounts only. An account from an identity provider gets its second
// factor from the provider, and Pando never asks it for one (R-044: the
// provider authenticates).
package twofactor

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: RFC 6238's default, and what every authenticator app computes.
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The parameters every authenticator app assumes when a QR code names none:
// HMAC-SHA1, six digits, thirty seconds. Anything else is a setting some apps
// ignore, and a code that silently disagrees is worse than the default.
const (
	digits    = 6
	period    = 30 * time.Second
	secretLen = 20 // 160 bits, RFC 4226's recommendation for HMAC-SHA1

	// skew is how many steps either side of now a code is accepted for: a
	// phone's clock a little out, or a code typed as it changed.
	skew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newSecret is a fresh authenticator secret, base32 as an app is given it.
func newSecret() (string, error) {
	raw := make([]byte, secretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return b32.EncodeToString(raw), nil
}

// step is the time step t falls in.
func step(t time.Time) int64 { return t.Unix() / int64(period/time.Second) }

// code is the code for one step (RFC 4226 §5.3).
func code(key []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter)) //nolint:gosec // G115: a step since 1970 is never negative.
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, bin%1_000_000)
}

// match is the step a code was made for, within skew of now and after
// lastStep, or ok=false. After lastStep, so a code that was accepted once is
// not accepted again, nor one older than it.
func match(secret, presented string, now time.Time, lastStep int64) (int64, bool) {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil || len(presented) != digits {
		return 0, false
	}
	current := step(now)
	for s := current - skew; s <= current+skew; s++ {
		if s <= lastStep {
			continue
		}
		if hmac.Equal([]byte(code(key, s)), []byte(presented)) {
			return s, true
		}
	}
	return 0, false
}

// otpauthURI is what the QR code holds: the key URI format every
// authenticator app reads. The issuer appears twice, in the label and as a
// parameter, because some apps read only one of them.
func otpauthURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// IsAuthenticatorCode says whether what someone typed is shaped like an
// authenticator code rather than a recovery code, spaces aside.
func IsAuthenticatorCode(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) != digits {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
