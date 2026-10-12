package docker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR105_AnImageTheDaemonWouldNotLoadSaysWhy asserts R-105 for loading a
// built image: the error the daemon put in its load output is read out of it,
// so the message names the cause rather than only "could not be loaded"
// (issue #90).
func TestR105_AnImageTheDaemonWouldNotLoadSaysWhy(t *testing.T) {
	body := `{"stream":"Loading layer\n"}` + "\n" +
		`{"errorDetail":{"message":"write /var/lib/docker/tmp/x: no space left on device"},"error":"write /var/lib/docker/tmp/x: no space left on device"}`
	require.Equal(t, "write /var/lib/docker/tmp/x: no space left on device", parseLoadError(body))
	require.Equal(t, "", parseLoadedRef(body), "a failed load names no image")

	require.Equal(t, "", parseLoadError(`{"stream":"Loaded image: pando/app:latest\n"}`))
	require.Equal(t, "unexpected EOF", parseLoadError(`{"error":"unexpected\n EOF"}`))

	require.LessOrEqual(t, len(daemonReason(strings.Repeat("y", 4000))), 510)
}
