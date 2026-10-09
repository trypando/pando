package clientaddr

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestR379_TheMiddlewarePutsTheRequestOnItsContext asserts what the audit
// writer reads: the client, the peer and the user agent, on every request.
func TestR379_TheMiddlewarePutsTheRequestOnItsContext(t *testing.T) {
	trusted, err := Parse("10.0.0.5")
	require.NoError(t, err)
	assert.False(t, trusted.Empty())
	assert.True(t, Trusted{}.Empty())

	var got Request
	var found bool
	h := trusted.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, found = From(r.Context())
	}))
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	r.Header.Set("User-Agent", "siem-collector/1.0")
	h.ServeHTTP(httptest.NewRecorder(), r)

	require.True(t, found)
	assert.Equal(t, Request{SourceIP: "198.51.100.7", PeerIP: "10.0.0.5", UserAgent: "siem-collector/1.0"}, got)

	_, found = From(r.Context())
	assert.False(t, found, "a context the middleware did not see carries nothing")
}

// TestAnAddressWithNoPortIsTheAddress covers a RemoteAddr with no port, as a
// unix socket listener reports it.
func TestAnAddressWithNoPortIsTheAddress(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "@"
	source, peer := Trusted{}.Resolve(r)
	assert.Equal(t, "@", source)
	assert.Equal(t, "@", peer)
}
