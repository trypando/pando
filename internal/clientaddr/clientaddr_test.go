package clientaddr

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestR380_ClientAddressIsThePeerUnlessThePeerIsTrusted asserts R-380.
func TestR380_ClientAddressIsThePeerUnlessThePeerIsTrusted(t *testing.T) {
	trusted, err := Parse("10.0.0.0/24, 192.168.1.7")
	require.NoError(t, err)

	cases := []struct {
		name, remote, xff, want string
		trust                   Trusted
	}{
		{name: "no list: the peer, whatever the header says", remote: "203.0.113.9:4000", xff: "1.2.3.4", want: "203.0.113.9"},
		{name: "untrusted peer: header ignored", remote: "203.0.113.9:4000", xff: "1.2.3.4", want: "203.0.113.9", trust: trusted},
		{name: "trusted peer: the client it names", remote: "10.0.0.5:4000", xff: "198.51.100.2", want: "198.51.100.2", trust: trusted},
		{name: "forged left of the chain is ignored", remote: "10.0.0.5:4000", xff: "6.6.6.6, 198.51.100.2", want: "198.51.100.2", trust: trusted},
		{name: "two trusted hops", remote: "10.0.0.5:4000", xff: "198.51.100.2, 192.168.1.7", want: "198.51.100.2", trust: trusted},
		{name: "only trusted hops: the left-most", remote: "10.0.0.5:4000", xff: "10.0.0.9", want: "10.0.0.9", trust: trusted},
		{name: "unreadable hop ends the chain", remote: "10.0.0.5:4000", xff: "6.6.6.6, garbage", want: "10.0.0.5", trust: trusted},
		{name: "trusted peer, no header", remote: "10.0.0.5:4000", want: "10.0.0.5", trust: trusted},
		{name: "IPv6 peer", remote: "[2001:db8::1]:4000", xff: "1.2.3.4", want: "2001:db8::1", trust: trusted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			source, peer := c.trust.Resolve(r)
			assert.Equal(t, c.want, source)
			assert.Equal(t, hostOf(c.remote), peer, "the peer is always recorded as it connected")
		})
	}
}

// TestR380_CatchAllTrustedProxyIsRefused asserts R-380's refusal at startup.
func TestR380_CatchAllTrustedProxyIsRefused(t *testing.T) {
	for _, bad := range []string{"0.0.0.0/0", "::/0", "10.0.0.0/7", "2001::/15", "not-an-address"} {
		_, err := Parse(bad)
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "PANDO_SERVER_TRUSTED_PROXIES")
	}
	for _, ok := range []string{"", "10.0.0.0/8", "2001:db8::/16", "127.0.0.1", "::1"} {
		_, err := Parse(ok)
		assert.NoError(t, err, ok)
	}
}

func TestUserAgentIsBoundedAndCarried(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("User-Agent", strings.Repeat("a", 5000))
	assert.Len(t, Trusted{}.Of(r).UserAgent, maxUserAgent)

	got, ok := From(With(context.Background(), Request{SourceIP: "1.2.3.4"}))
	assert.True(t, ok)
	assert.Equal(t, "1.2.3.4", got.SourceIP)
}
