package cloudflare

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A console hostname Cloudflare's included certificate does not cover saves,
// with a warning saying what it needs; one it covers saves without one.
func TestWarnsOfAConsoleHostnameTheIncludedCertificateDoesNotCover(t *testing.T) {
	require.Empty(t, warn([]byte(`{"zone":"example.com","console_hostname":"pando.example.com"}`)))
	require.Empty(t, warn([]byte(`{"zone":"example.com"}`)))
	require.Empty(t, warn([]byte(`not json`)))
	// In a secondary zone, measured from that zone, the longest that holds it.
	require.Empty(t, warn([]byte(`{"zone":"example.com","other_zones":"dev.example.com","console_hostname":"pando.dev.example.com"}`)))

	got := warn([]byte(`{"zone":"example.com","other_zones":"example.org, example.net","console_hostname":"Pando.A.Example.org."}`))
	require.Len(t, got, 1)
	require.Contains(t, got[0], "pando.a.example.org is more than one level below example.org")
	require.Contains(t, got[0], "Advanced Certificate Manager")
}
