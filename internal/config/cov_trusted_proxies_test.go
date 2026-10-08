package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR380_TrustedProxiesAreCheckedAtStartup asserts R-380: the proxies whose
// X-Forwarded-For names the audited client are read from
// PANDO_SERVER_TRUSTED_PROXIES, and a list Pando cannot read, or one that
// trusts most of the internet, stops startup with a message naming the
// variable rather than recording forged addresses.
func TestR380_TrustedProxiesAreCheckedAtStartup(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")

	cfg, err := Load("")
	require.NoError(t, err)
	trusted, err := cfg.Server.Trusted()
	require.NoError(t, err)
	require.True(t, trusted.Empty(), "by default nobody's X-Forwarded-For is believed")

	t.Setenv("PANDO_SERVER_TRUSTED_PROXIES", "10.0.0.5, 10.1.0.0/24")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, "10.0.0.5, 10.1.0.0/24", cfg.Server.TrustedProxies)
	trusted, err = cfg.Server.Trusted()
	require.NoError(t, err)
	require.False(t, trusted.Empty())

	for value, want := range map[string]string{
		"proxy.internal": "not an address or a CIDR range",
		"0.0.0.0/0":      "covers most of the internet",
	} {
		t.Setenv("PANDO_SERVER_TRUSTED_PROXIES", value)
		_, err = Load("")
		require.ErrorContains(t, err, "PANDO_SERVER_TRUSTED_PROXIES", value)
		require.ErrorContains(t, err, want, value)
	}
}
