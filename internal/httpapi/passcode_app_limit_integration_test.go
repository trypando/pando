//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// TestR075a_WrongPasscodesAreLimitedPerAppFromEveryAddress asserts R-075a's
// "limited per app and visitor" for the app half: guesses spread over many
// addresses, each under its own limit, still run out for the app as a whole.
// A right passcode does not clear that count — whoever knows it is not the one
// guessing — and a passcode shorter than six characters is refused.
func TestR075a_WrongPasscodesAreLimitedPerAppFromEveryAddress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i := newInstall(t)
	failures := state.NewFailedAttempts(i.db)
	i.Server.FailedAttempts = failures
	appID := passcodeApp(t, i, "open-sesame")

	short := i.do(i.admin(), http.MethodPost, "/apps/"+appID+"/grants",
		map[string]any{"plane": "data", "principal_kind": "anonymous", "passcode": "abcde"})
	require.Equal(t, http.StatusBadRequest, short.Code, short.String())
	require.Contains(t, short.String(), "at least 6 characters")

	guess := func(passcode string) reply {
		return i.do(nil, http.MethodPost, "/apps/"+appID+"/passcode", map[string]string{"passcode": passcode})
	}
	require.Equal(t, http.StatusUnauthorized, guess("guess").Code)

	appKey := "passcode|app|" + appID
	n, err := failures.Recent(ctx, appKey, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the app's own count, beside the visitor's")

	unlock(t, i, appID, "open-sesame")
	n, err = failures.Recent(ctx, appKey, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n, "a right passcode clears the visitor's count, not the app's")

	// Ninety-nine more from other addresses: this visitor has guessed once,
	// and is still refused, the right passcode included.
	for range 99 {
		require.NoError(t, failures.Record(ctx, appKey, time.Hour))
	}
	limited := guess("open-sesame")
	require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.String())
	require.Contains(t, limited.String(), "have been tried recently, from anywhere")
}
