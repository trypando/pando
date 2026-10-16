package cloudflare

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR165_TheTunnelsConsoleHostnameIsReserved asserts that a Cloudflare
// tunnel declares where it routes the console — the zone itself unless
// another hostname is set — so no app is given it.
func TestR165_TheTunnelsConsoleHostnameIsReserved(t *testing.T) {
	caps, err := (&Adapter{config: Config{Zone: "bemeek.io", ConsoleHostname: "bemeek.io"}}).Capabilities(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"bemeek.io"}, caps.ReservedHostnames)

	caps, err = (&Adapter{}).Capabilities(context.Background())
	require.NoError(t, err)
	require.Empty(t, caps.ReservedHostnames)
}
