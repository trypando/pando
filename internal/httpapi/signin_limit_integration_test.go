//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/state"
)

// signInLimited is an install whose sign-in limits are counted in the store
// every replica shares.
func signInLimited(t *testing.T) (*install, *state.FailedAttempts) {
	t.Helper()
	i := newInstall(t)
	failures := state.NewFailedAttempts(i.db)
	i.Server.FailedAttempts = failures
	return i, failures
}

// deniedFor counts session.denied events for username with reason.
func deniedFor(t *testing.T, i *install, username, reason string) int {
	t.Helper()
	var n int
	require.NoError(t, i.db.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		WHERE action = 'session.denied' AND detail->>'username' = $1
		  AND coalesce(detail->>'reason', '') = $2`, username, reason).Scan(&n))
	return n
}

// TestR404_WrongPasswordsAreLimitedPerUsername asserts R-404: after ten wrong
// passwords for one username, sign-in to it is refused for the rest of the
// window — the right password included — with a message and a Retry-After
// that say when to try again, and each failure and refusal is audited.
// Another username is not held to that count.
func TestR404_WrongPasswordsAreLimitedPerUsername(t *testing.T) {
	t.Parallel()
	i, _ := signInLimited(t)
	i.user("dana")
	admin := bootstrap.AdminUsername

	for range 10 {
		_, got := i.signIn(admin, "not-the-password")
		require.Equal(t, http.StatusUnauthorized, got.Code, got.String())
	}
	require.Equal(t, 10, deniedFor(t, i, admin, ""), "every wrong password is audited")

	_, limited := i.signIn(admin, i.adminPassword)
	require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.String())
	require.Equal(t, "RATE_LIMITED", limited.ErrorCode())
	require.Contains(t, limited.String(), "Too many wrong passwords for this username. Try again in 15 minutes.")
	require.Equal(t, "900", limited.Hdr.Get("Retry-After"))
	require.Equal(t, 1, deniedFor(t, i, admin, "rate_limited"), "a refusal is audited, with why")

	_, other := i.signIn("dana", "not-the-password")
	require.Equal(t, http.StatusUnauthorized, other.Code, "another username has its own count")
}

// TestR404_WrongPasswordsAreLimitedPerAddress asserts R-404's second limit:
// one address that has tried many wrong passwords — across any usernames — is
// refused, even for an account it has not guessed at.
func TestR404_WrongPasswordsAreLimitedPerAddress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i, failures := signInLimited(t)

	_, got := i.signIn("nobody", "not-the-password")
	require.Equal(t, http.StatusUnauthorized, got.Code, "a username that does not exist is counted the same")

	// The request's client address, as the handler keys it.
	var key string
	require.NoError(t, i.db.QueryRow(ctx,
		`SELECT key FROM failed_attempts WHERE key LIKE 'signin|addr|%'`).Scan(&key))
	for range 49 {
		require.NoError(t, failures.Record(ctx, key, time.Hour))
	}

	_, limited := i.signIn(bootstrap.AdminUsername, i.adminPassword)
	require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.String())
	require.Contains(t, limited.String(), "Too many wrong passwords from your network address.")
}

// TestR404_ARightPasswordClearsOnlyTheUsernamesCount asserts that signing in
// forgets the username's failures but not the address's: signing in to an
// account of your own must not reset the count for guessing at others.
func TestR404_ARightPasswordClearsOnlyTheUsernamesCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i, _ := signInLimited(t)
	admin := bootstrap.AdminUsername

	for range 9 {
		_, got := i.signIn(admin, "not-the-password")
		require.Equal(t, http.StatusUnauthorized, got.Code)
	}
	i.admin()
	_, got := i.signIn(admin, "not-the-password")
	require.Equal(t, http.StatusUnauthorized, got.Code, "the username's count started again")

	var addr int
	require.NoError(t, i.db.QueryRow(ctx,
		`SELECT count(*) FROM failed_attempts WHERE key LIKE 'signin|addr|%'`).Scan(&addr))
	require.Equal(t, 10, addr, "the address's count is kept")
}

// brokenAttempts is a failed-attempt store whose database is away.
type brokenAttempts struct{ recentErr, recordErr error }

func (b brokenAttempts) Recent(context.Context, string, time.Duration) (int, error) {
	return 0, b.recentErr
}
func (b brokenAttempts) Wait(context.Context, string, time.Duration) (time.Duration, error) {
	return 0, b.recentErr
}
func (b brokenAttempts) Record(context.Context, string, time.Duration) error { return b.recordErr }
func (b brokenAttempts) Clear(context.Context, string) error                 { return errors.New("database away") }

// TestR404_AnUncountableSignInIsRefused asserts the sign-in limit fails
// closed: an attempt whose count cannot be read, or a wrong password that
// cannot be counted, is refused rather than let through uncounted.
func TestR404_AnUncountableSignInIsRefused(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	i.Server.FailedAttempts = brokenAttempts{recentErr: errors.New("database away")}
	_, got := i.signIn(bootstrap.AdminUsername, i.adminPassword)
	require.Equal(t, http.StatusInternalServerError, got.Code, "an unreadable count is not a free attempt")

	i.Server.FailedAttempts = brokenAttempts{recordErr: errors.New("database away")}
	_, got = i.signIn(bootstrap.AdminUsername, "not-the-password")
	require.Equal(t, http.StatusInternalServerError, got.Code, "a wrong password that cannot be counted is refused")

	// A right password signs in even if clearing the count fails: the count
	// only ever errs toward stricter.
	i.admin()
}

// TestR404_ThePasswordChangeIsHeldToTheSignInLimits asserts that the current
// password asked for by POST /me/password is counted like a sign-in. A stolen
// session or token could otherwise guess the owner's password there without
// limit, then set a new one and end every other session.
func TestR404_ThePasswordChangeIsHeldToTheSignInLimits(t *testing.T) {
	t.Parallel()
	i, _ := signInLimited(t)
	admin := i.admin()

	for range 10 {
		got := i.do(admin, http.MethodPost, "/me/password", map[string]any{
			"current_password": "not-the-password",
			"new_password":     "an-entirely-different-password",
		})
		require.Equal(t, http.StatusUnauthorized, got.Code, got.String())
	}

	limited := i.do(admin, http.MethodPost, "/me/password", map[string]any{
		"current_password": i.adminPassword,
		"new_password":     "an-entirely-different-password",
	})
	require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.String())
	require.Contains(t, limited.String(), "Too many wrong passwords for this username.")

	_, signIn := i.signIn(bootstrap.AdminUsername, i.adminPassword)
	require.Equal(t, http.StatusTooManyRequests, signIn.Code,
		"the count is the username's, shared with sign-in, so neither door gives a second allowance")
}
