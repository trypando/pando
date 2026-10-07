//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// passcodeApp is an app anyone may use with passcode.
func passcodeApp(t *testing.T, i *install, passcode string) string {
	t.Helper()
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	created := i.do(admin, http.MethodPost, "/apps/"+appID+"/grants",
		map[string]any{"plane": "data", "principal_kind": "anonymous", "passcode": passcode})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	return appID
}

// TestR075a_ThePasscodeLimitIsKeptInTheSharedStore asserts that the limit on
// guessing a passcode is counted in the store every replica shares (R-075a,
// issue #72): wrong guesses recorded there lock the visitor out, a right one
// clears them, and a count recorded by another replica counts here.
func TestR075a_ThePasscodeLimitIsKeptInTheSharedStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i := newInstall(t)
	failures := state.NewPasscodeFailures(i.db)
	i.Server.PasscodeFailures = failures
	appID := passcodeApp(t, i, "open-sesame")
	guess := func(passcode string) reply {
		return i.do(nil, http.MethodPost, "/apps/"+appID+"/passcode", map[string]string{"passcode": passcode})
	}

	wrong := guess("guess")
	require.Equal(t, http.StatusUnauthorized, wrong.Code, wrong.String())

	// The request's client address, as the handler keys it.
	var key string
	require.NoError(t, i.db.QueryRow(ctx, `SELECT key FROM passcode_failures LIMIT 1`).Scan(&key))
	require.Contains(t, key, appID+"|", "counted in the shared store, per app and client")

	// A right passcode clears the count.
	unlock(t, i, appID, "open-sesame")
	n, err := failures.Recent(ctx, key, time.Hour)
	require.NoError(t, err)
	require.Zero(t, n)

	// Nine more recorded by other replicas, and one here: the tenth locks the
	// visitor out of every replica, the right passcode included.
	for range 9 {
		require.NoError(t, failures.Record(ctx, key, time.Hour))
	}
	require.Equal(t, http.StatusUnauthorized, guess("guess-again").Code)
	limited := guess("open-sesame")
	require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.String())
	require.Contains(t, limited.String(), "Too many wrong passcodes")
}

// brokenFailures is a passcode store whose database is away.
type brokenFailures struct{ recentErr, recordErr error }

func (b brokenFailures) Recent(context.Context, string, time.Duration) (int, error) {
	return 0, b.recentErr
}
func (b brokenFailures) Record(context.Context, string, time.Duration) error { return b.recordErr }
func (b brokenFailures) Clear(context.Context, string) error                 { return errors.New("database away") }

// TestR075a_AnUncountablePasscodeAttemptIsRefused asserts that the limit
// fails closed: when the shared count cannot be read or written, the attempt
// is refused rather than let through uncounted.
func TestR075a_AnUncountablePasscodeAttemptIsRefused(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	appID := passcodeApp(t, i, "open-sesame")

	i.Server.PasscodeFailures = brokenFailures{recentErr: errors.New("database away")}
	got := i.do(nil, http.MethodPost, "/apps/"+appID+"/passcode", map[string]string{"passcode": "open-sesame"})
	require.Equal(t, http.StatusInternalServerError, got.Code, "an unreadable count is not a free attempt")

	i.Server.PasscodeFailures = brokenFailures{recordErr: errors.New("database away")}
	got = i.do(nil, http.MethodPost, "/apps/"+appID+"/passcode", map[string]string{"passcode": "guess"})
	require.Equal(t, http.StatusInternalServerError, got.Code, "a wrong guess that cannot be counted is refused")

	// A right passcode is let in even if clearing the count fails: the count
	// only ever errs toward stricter.
	unlock(t, i, appID, "open-sesame")
}
