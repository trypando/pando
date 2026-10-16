package traefik

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR165_TheConsoleHostnameIsReserved asserts that a Traefik edge declares
// the hostname it serves the console on, so no app is given it.
func TestR165_TheConsoleHostnameIsReserved(t *testing.T) {
	caps, err := (&Adapter{config: Config{ConsoleHostname: "pando.example.com"}}).Capabilities(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"pando.example.com"}, caps.ReservedHostnames)

	caps, err = (&Adapter{}).Capabilities(context.Background())
	require.NoError(t, err)
	require.Empty(t, caps.ReservedHostnames)
}
