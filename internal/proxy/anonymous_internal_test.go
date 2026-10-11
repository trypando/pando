package proxy

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/clientaddr"
)

// TestR075_AnIdleAnonymousBucketIsForgotten asserts the limiter does not grow
// without bound: a bucket unused for longer than it takes to refill is
// dropped at the next sweep, and nothing in use is.
func TestR075_AnIdleAnonymousBucketIsForgotten(t *testing.T) {
	l := newAnonymousLimiter()
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	require.True(t, l.allow("app_a|198.51.100.7", start))
	require.True(t, l.allow("app_a|203.0.113.9", start.Add(50*time.Second)))

	require.True(t, l.allow("app_a|203.0.113.9", start.Add(2*anonymousIdle)))
	require.NotContains(t, l.buckets, "app_a|198.51.100.7", "idle past a refill, so forgetting it gives nothing away")
	require.Contains(t, l.buckets, "app_a|203.0.113.9")
}

// TestR075_TheAnonymousLimitFallsBackToThePeer asserts which address a
// bucket is kept per when the request carries no resolved one: the TCP peer,
// or the whole RemoteAddr when it has no port.
func TestR075_TheAnonymousLimitFallsBackToThePeer(t *testing.T) {
	r := httptest.NewRequest("GET", "http://notes.test/", nil)
	r.RemoteAddr = "198.51.100.7:40000"
	require.Equal(t, "198.51.100.7", sourceAddr(r))

	r.RemoteAddr = "pipe"
	require.Equal(t, "pipe", sourceAddr(r))

	r = r.WithContext(clientaddr.With(r.Context(), clientaddr.Request{SourceIP: "203.0.113.9"}))
	require.Equal(t, "203.0.113.9", sourceAddr(r), "the audit log's address when there is one")
}
