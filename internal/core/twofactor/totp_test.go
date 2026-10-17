package twofactor

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// rfcSecret is RFC 6238 Appendix B's SHA-1 key, "12345678901234567890".
var rfcSecret = b32.EncodeToString([]byte("12345678901234567890"))

// TestR405_CodesMatchRFC6238 asserts R-405: the codes are the ones every
// authenticator app computes. RFC 6238's eight-digit values, last six digits.
func TestR405_CodesMatchRFC6238(t *testing.T) {
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	} {
		if got := code(key, step(time.Unix(unix, 0))); got != want {
			t.Errorf("at %d: code = %s, want %s", unix, got, want)
		}
	}
}

func TestR405_MatchAcceptsOneStepEitherSide(t *testing.T) {
	now := time.Unix(1234567890, 0)
	key, _ := b32.DecodeString(rfcSecret)
	current := step(now)
	for _, s := range []int64{current - 1, current, current + 1} {
		got, ok := match(rfcSecret, code(key, s), now, 0)
		if !ok || got != s {
			t.Errorf("step %d: match = %d, %v; want it accepted", s, got, ok)
		}
	}
	for _, s := range []int64{current - 2, current + 2} {
		if _, ok := match(rfcSecret, code(key, s), now, 0); ok {
			t.Errorf("step %d accepted, two steps from now", s)
		}
	}
}

// TestR405_ACodeIsGoodOnce asserts R-405: a code accepted once, or one older
// than it, is refused.
func TestR405_ACodeIsGoodOnce(t *testing.T) {
	now := time.Unix(1234567890, 0)
	key, _ := b32.DecodeString(rfcSecret)
	current := step(now)
	if _, ok := match(rfcSecret, code(key, current), now, current); ok {
		t.Error("the code for the last accepted step was accepted again")
	}
	if _, ok := match(rfcSecret, code(key, current-1), now, current); ok {
		t.Error("a code older than the last accepted step was accepted")
	}
	if _, ok := match(rfcSecret, code(key, current+1), now, current); !ok {
		t.Error("the next step's code was refused")
	}
}

func TestMatchRefusesMalformed(t *testing.T) {
	now := time.Unix(1234567890, 0)
	for _, c := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := match(rfcSecret, c, now, 0); ok {
			t.Errorf("%q accepted", c)
		}
	}
	if _, ok := match("not base32!", "123456", now, 0); ok {
		t.Error("a code was accepted against an unreadable secret")
	}
}

func TestNewSecretIs160Bits(t *testing.T) {
	a, err := newSecret()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := b32.DecodeString(a)
	if err != nil || len(raw) != secretLen {
		t.Fatalf("secret %q decodes to %d bytes (%v), want %d", a, len(raw), err, secretLen)
	}
	if b, _ := newSecret(); a == b {
		t.Error("two secrets were the same")
	}
}

func TestOTPAuthURI(t *testing.T) {
	got := otpauthURI("Pando (pando.example.com)", "ben", "ABC")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Errorf("uri = %s", got)
	}
	if !strings.HasPrefix(u.Path, "/Pando (pando.example.com):ben") {
		t.Errorf("label = %q", u.Path)
	}
	if u.Query().Get("secret") != "ABC" || u.Query().Get("issuer") != "Pando (pando.example.com)" {
		t.Errorf("query = %v", u.Query())
	}
}

func TestIsAuthenticatorCode(t *testing.T) {
	for s, want := range map[string]bool{
		"123456": true, "123 456": true, "12345": false, "1234567": false,
		"abcd-efgh-jkmn-pqrs": false, "12345a": false,
	} {
		if got := IsAuthenticatorCode(s); got != want {
			t.Errorf("IsAuthenticatorCode(%q) = %v, want %v", s, got, want)
		}
	}
}
