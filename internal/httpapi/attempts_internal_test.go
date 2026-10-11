package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/clientaddr"
)

// TestR404_TheInMemoryCountLimitsAndSaysWhen asserts the count a Server uses
// when no shared store is wired: it reaches the limit, says how long until the
// oldest failure leaves the window, forgets on Clear, and is one per Server so
// that two in one process do not share a count.
func TestR404_TheInMemoryCountLimitsAndSaysWhen(t *testing.T) {
	ctx := context.Background()
	s := &Server{}
	f := s.failedAttempts()
	require.Same(t, f, s.failedAttempts(), "one count per Server")
	require.NotSame(t, f, (&Server{}).failedAttempts(), "and not one per process")

	limit := attemptLimit{key: "signin|user|dana", max: 3, window: time.Minute}
	wait, err := limit.exceeded(ctx, f)
	require.NoError(t, err)
	require.Zero(t, wait)
	gone, err := f.Wait(ctx, limit.key, limit.window)
	require.NoError(t, err)
	require.Zero(t, gone, "nothing to wait for with no failures")

	for range 3 {
		require.NoError(t, f.Record(ctx, limit.key, limit.window))
	}
	wait, err = limit.exceeded(ctx, f)
	require.NoError(t, err)
	require.Greater(t, wait, 55*time.Second)
	require.LessOrEqual(t, wait, time.Minute)

	require.NoError(t, f.Clear(ctx, limit.key))
	n, err := f.Recent(ctx, limit.key, limit.window)
	require.NoError(t, err)
	require.Zero(t, n)
}

// waitBroken counts past any limit and cannot say for how long.
type waitBroken struct{ *memoryFailedAttempts }

func (*waitBroken) Recent(context.Context, string, time.Duration) (int, error) { return 99, nil }
func (*waitBroken) Wait(context.Context, string, time.Duration) (time.Duration, error) {
	return 0, errors.New("database away")
}

// TestR404_AnUnreadableWaitIsAnError asserts the limit fails closed when it
// knows a key is over but cannot say until when.
func TestR404_AnUnreadableWaitIsAnError(t *testing.T) {
	_, err := attemptLimit{key: "k", max: 1, window: time.Minute}.exceeded(context.Background(), &waitBroken{})
	require.Error(t, err)
}

// TestR404_TheWaitIsSaidInWords asserts how a refusal says how long to wait
// (R-105: self-contained, no undefined units).
func TestR404_TheWaitIsSaidInWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Second:                  "1 second",
		1500 * time.Millisecond:      "2 seconds",
		30 * time.Second:             "30 seconds",
		time.Minute:                  "1 minute",
		11*time.Minute + time.Second: "12 minutes",
		15 * time.Minute:             "15 minutes",
	} {
		require.Equal(t, want, waitPhrase(d), d.String())
	}
}

// TestR404_TheLimitAddressIsTheAuditedOne asserts which address the limits
// are kept per: the one the audit log records, or the peer without it.
func TestR404_TheLimitAddressIsTheAuditedOne(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/v1/sessions", nil)
	r.RemoteAddr = "10.0.0.5:40000"
	require.Equal(t, "10.0.0.5", limitAddr(r))

	r = r.WithContext(clientaddr.With(r.Context(), clientaddr.Request{SourceIP: "203.0.113.9", PeerIP: "10.0.0.5"}))
	require.Equal(t, "203.0.113.9", limitAddr(r))
}
