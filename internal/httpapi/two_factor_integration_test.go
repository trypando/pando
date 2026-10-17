//go:build integration

package httpapi_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // G505: what RFC 6238 and authenticator apps compute.
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/bootstrap"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/httpapi"
)

// totpAt is what an authenticator app shows for secret at t (RFC 6238).
func totpAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	require.NoError(t, err)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix()/30)) //nolint:gosec // G115: never negative.
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff)%1_000_000)
}

// enrollTOTP sets up an authenticator on s's account, on a session that has
// just signed in, and returns its secret and recovery codes.
func (i *install) enrollTOTP(s *session) (string, []string) {
	i.t.Helper()
	begun := i.do(s, http.MethodPost, "/me/two-factor/totp", map[string]any{})
	require.Equal(i.t, http.StatusOK, begun.Code, begun.String())
	var e struct {
		Secret string `json:"secret"`
		URI    string `json:"uri"`
		QRSVG  string `json:"qr_svg"`
	}
	begun.JSON(i.t, &e)
	require.NotEmpty(i.t, e.Secret)
	require.Contains(i.t, e.URI, "otpauth://totp/")
	require.True(i.t, strings.HasPrefix(e.QRSVG, "<svg"), "a QR code comes with it")

	confirmed := i.do(s, http.MethodPost, "/me/two-factor/totp/confirm",
		map[string]string{"code": totpAt(i.t, e.Secret, time.Now())})
	require.Equal(i.t, http.StatusOK, confirmed.Code, confirmed.String())
	var c struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	confirmed.JSON(i.t, &c)
	require.Len(i.t, c.RecoveryCodes, 10)
	return e.Secret, c.RecoveryCodes
}

func (i *install) signInWithCode(username, password, code string) (*session, reply) {
	i.t.Helper()
	got := i.do(nil, http.MethodPost, "/sessions",
		map[string]string{"username": username, "password": password, "code": code})
	s := &session{}
	for _, c := range (&http.Response{Header: got.Hdr}).Cookies() {
		if c.Name == httpapi.SessionCookie {
			s.cookie = c.Value
		}
	}
	return s, got
}

func (i *install) requireLocalTwoFactor(on bool) {
	i.t.Helper()
	require.NoError(i.t, state.NewPolicy(i.db).Save(context.Background(),
		corepolicy.Document{RequireLocalTwoFactor: on}, "test"))
}

func auditCount(t *testing.T, i *install, action, target string) int {
	t.Helper()
	var n int
	require.NoError(t, i.db.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = $1 AND target_id = $2`, action, target).Scan(&n))
	return n
}

const danaPassword = "another-correct-horse-staple"

func meTwoFactor(t *testing.T, i *install, s *session) (string, map[string]any) {
	t.Helper()
	got := i.do(s, http.MethodGet, "/me", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var me struct {
		UserID    string         `json:"user_id"`
		TwoFactor map[string]any `json:"two_factor"`
	}
	got.JSON(t, &me)
	return me.UserID, me.TwoFactor
}

// TestR405_SignInWithTwoFactorNeedsACode asserts R-405: an account with an
// authenticator signs in with its password and a code, or a recovery code
// once, and a wrong code is refused and audited.
func TestR405_SignInWithTwoFactorNeedsACode(t *testing.T) {
	t.Parallel()
	i, _ := signInLimited(t)
	dana := i.user("dana")
	userID, tf := meTwoFactor(t, i, dana)
	require.Equal(t, true, tf["offer"], "asked once, at first")
	require.Equal(t, false, tf["enabled"])

	secret, codes := i.enrollTOTP(dana)
	require.Equal(t, 1, auditCount(t, i, "user.two_factor.enable", userID))
	_, tf = meTwoFactor(t, i, dana)
	require.Equal(t, true, tf["enabled"])
	require.Equal(t, false, tf["offer"])

	s, got := i.signIn("dana", danaPassword)
	require.Equal(t, http.StatusUnauthorized, got.Code, got.String())
	require.Equal(t, "AUTH_TWO_FACTOR_REQUIRED", got.ErrorCode())
	require.Empty(t, s.cookie, "no session on the password alone")

	_, got = i.signInWithCode("dana", danaPassword, "000000")
	require.Equal(t, http.StatusUnauthorized, got.Code, got.String())
	require.Equal(t, "AUTH_INVALID", got.ErrorCode())
	require.Equal(t, 1, deniedFor(t, i, "dana", "wrong_code"))

	_, got = i.signInWithCode("not-the-password-either", "x", totpAt(t, secret, time.Now()))
	require.Equal(t, http.StatusUnauthorized, got.Code, "a code does not stand in for the password")

	// The confirming step's code is spent, so the next step's.
	s, got = i.signInWithCode("dana", danaPassword, totpAt(t, secret, time.Now().Add(30*time.Second)))
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.NotEmpty(t, s.cookie)

	s, got = i.signInWithCode("dana", danaPassword, strings.ToUpper(codes[0]))
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.NotEmpty(t, s.cookie)
	require.Equal(t, 1, auditCount(t, i, "user.two_factor.recovery_code_used", userID))
	_, got = i.signInWithCode("dana", danaPassword, codes[0])
	require.Equal(t, http.StatusUnauthorized, got.Code, "a recovery code works once")
	_, tf = meTwoFactor(t, i, s)
	require.EqualValues(t, 9, tf["recovery_codes_remaining"])
}

// TestR405_WrongCodesAreLimited asserts R-405 holds codes to R-404's kind of
// count: after five wrong codes, even the right one waits.
func TestR405_WrongCodesAreLimited(t *testing.T) {
	t.Parallel()
	i, _ := signInLimited(t)
	dana := i.user("dana")
	secret, _ := i.enrollTOTP(dana)

	for range 5 {
		_, got := i.signInWithCode("dana", danaPassword, "000000")
		require.Equal(t, http.StatusUnauthorized, got.Code, got.String())
	}
	_, got := i.signInWithCode("dana", danaPassword, totpAt(t, secret, time.Now().Add(30*time.Second)))
	require.Equal(t, http.StatusTooManyRequests, got.Code, got.String())
	require.Contains(t, got.String(), "Too many wrong codes for this account.")
}

// TestR405_PolicyHoldsBackASessionUntilItSetsUpTwoFactor asserts R-405: with
// the policy on, a local account without a second factor — signed in before
// or after — can do nothing but set one up, cannot decline, and once it has,
// cannot turn it off.
func TestR405_PolicyHoldsBackASessionUntilItSetsUpTwoFactor(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	before := i.user("dana")
	i.requireLocalTwoFactor(true)

	after, got := i.signIn("dana", danaPassword)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var signedIn struct {
		SetupRequired bool `json:"two_factor_setup_required"`
	}
	got.JSON(t, &signedIn)
	require.True(t, signedIn.SetupRequired)

	for name, s := range map[string]*session{"signed in before": before, "signed in after": after} {
		_, tf := meTwoFactor(t, i, s)
		require.Equal(t, true, tf["setup_required"], name)
		require.Equal(t, false, tf["offer"], name)

		for _, path := range []string{"/me/apps", "/apps", "/users"} {
			held := i.do(s, http.MethodGet, path, nil)
			require.Equal(t, http.StatusForbidden, held.Code, "%s: %s %s", name, path, held.String())
			require.Equal(t, "POLICY_TWO_FACTOR_REQUIRED", held.ErrorCode())
		}
		minted := i.do(s, http.MethodPost, "/tokens", map[string]any{"name": "sneaky"})
		require.Equal(t, http.StatusForbidden, minted.Code, "%s: no token around it", name)
	}
	skipped := i.do(after, http.MethodPost, "/me/two-factor/skip", nil)
	require.Equal(t, http.StatusForbidden, skipped.Code, skipped.String())

	// The session that was held back can set it up without the password
	// again, however old: it has nothing to lose to a borrowed session.
	_, err := i.db.Exec(context.Background(), `UPDATE sessions SET issued_at = now() - interval '1 hour'`)
	require.NoError(t, err)
	i.enrollTOTP(before)
	require.Equal(t, http.StatusOK, i.do(before, http.MethodGet, "/me/apps", nil).Code, "free once set up")
	require.Equal(t, http.StatusOK, i.do(after, http.MethodGet, "/me/apps", nil).Code,
		"every session of the account, not only the one that set it up")

	off := i.do(before, http.MethodDelete, "/me/two-factor", map[string]string{"current_password": danaPassword})
	require.Equal(t, http.StatusForbidden, off.Code, off.String())
	require.Equal(t, "POLICY_TWO_FACTOR_REQUIRED", off.ErrorCode())
}

// TestR405_ChangingItOnAnOldSessionNeedsThePassword asserts that a session
// more than a few minutes old, or a token, gives the password to change its
// second factor, held to R-404's counts.
func TestR405_ChangingItOnAnOldSessionNeedsThePassword(t *testing.T) {
	t.Parallel()
	i, _ := signInLimited(t)
	dana := i.user("dana")
	userID, _ := meTwoFactor(t, i, dana)
	i.enrollTOTP(dana)
	_, err := i.db.Exec(context.Background(),
		`UPDATE sessions SET issued_at = now() - interval '1 hour' WHERE user_id = $1`, userID)
	require.NoError(t, err)

	_, tf := meTwoFactor(t, i, dana)
	require.Equal(t, true, tf["password_required"])

	for _, path := range []string{"/me/two-factor/totp", "/me/two-factor/recovery-codes"} {
		got := i.do(dana, http.MethodPost, path, map[string]any{})
		require.Equal(t, http.StatusUnauthorized, got.Code, "%s: %s", path, got.String())
		got = i.do(dana, http.MethodPost, path, map[string]string{"current_password": "wrong-password-here"})
		require.Equal(t, http.StatusUnauthorized, got.Code, "%s: %s", path, got.String())
	}
	require.Equal(t, 2, auditCount(t, i, "user.two_factor.denied", userID))

	fresh := i.do(dana, http.MethodPost, "/me/two-factor/recovery-codes", map[string]string{"current_password": danaPassword})
	require.Equal(t, http.StatusOK, fresh.Code, fresh.String())
	require.Equal(t, 1, auditCount(t, i, "user.two_factor.recovery_codes", userID))

	off := i.do(dana, http.MethodDelete, "/me/two-factor", map[string]string{"current_password": danaPassword})
	require.Equal(t, http.StatusNoContent, off.Code, off.String())
	require.Equal(t, 1, auditCount(t, i, "user.two_factor.disable", userID))
	s, got := i.signIn("dana", danaPassword)
	require.Equal(t, http.StatusOK, got.Code, "the password alone again: %s", got.String())
	require.NotEmpty(t, s.cookie)
}

// TestR405_AnAdministratorResetsTwoFactor asserts R-405: an administrator
// removes someone's second factor, which signs them out everywhere; they sign
// in with their password and are asked again. Nobody resets their own here,
// and someone without install.users.manage resets nobody's.
func TestR405_AnAdministratorResetsTwoFactor(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	dana := i.user("dana")
	userID, _ := meTwoFactor(t, i, dana)
	i.enrollTOTP(dana)

	erin := i.user("erin")
	require.Equal(t, http.StatusForbidden,
		i.do(erin, http.MethodDelete, "/users/"+userID+"/two-factor", nil).Code)

	adminID, _ := meTwoFactor(t, i, admin)
	self := i.do(admin, http.MethodDelete, "/users/"+adminID+"/two-factor", nil)
	require.Equal(t, http.StatusBadRequest, self.Code, self.String())

	listed := i.do(admin, http.MethodGet, "/users/"+userID, nil)
	require.Contains(t, listed.String(), `"two_factor_enabled":true`)

	reset := i.do(admin, http.MethodDelete, "/users/"+userID+"/two-factor", nil)
	require.Equal(t, http.StatusNoContent, reset.Code, reset.String())
	require.Equal(t, 1, auditCount(t, i, "user.two_factor.reset", userID))
	require.Equal(t, http.StatusUnauthorized, i.do(dana, http.MethodGet, "/me", nil).Code, "signed out everywhere")

	again, got := i.signIn("dana", danaPassword)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	_, tf := meTwoFactor(t, i, again)
	require.Equal(t, false, tf["enabled"])
	require.Equal(t, true, tf["offer"], "asked again")

	// With the policy on, the reset account sets it up before anything else.
	i.requireLocalTwoFactor(true)
	held := i.do(again, http.MethodGet, "/me/apps", nil)
	require.Equal(t, http.StatusForbidden, held.Code, held.String())
}

// TestR405_DecliningIsAskedOnce asserts the ask-once rule end to end.
func TestR405_DecliningIsAskedOnce(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	dana := i.user("dana")
	userID, _ := meTwoFactor(t, i, dana)
	got := i.do(dana, http.MethodPost, "/me/two-factor/skip", nil)
	require.Equal(t, http.StatusNoContent, got.Code, got.String())
	_, tf := meTwoFactor(t, i, dana)
	require.Equal(t, false, tf["offer"])
	require.Equal(t, 1, auditCount(t, i, "user.two_factor.decline", userID))

	// A token is not a sign-in: no account of its own to set up on, and
	// changing a delegated one's needs the password.
	token := i.tokenFor(dana)
	begun := i.do(token, http.MethodPost, "/me/two-factor/totp", map[string]any{})
	require.Equal(t, http.StatusUnauthorized, begun.Code, begun.String())

	// The first-run administrator is local too.
	_, tf = meTwoFactor(t, i, i.admin())
	require.NotNil(t, tf, "%s has two-factor settings", bootstrap.AdminUsername)
}
