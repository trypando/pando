package docker

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// Podman that cannot be reached, or answers what is not its info, is
// unavailable — and a rootful Podman missing a controller says which, in
// Podman's words (issue #179).
func TestPodmanThatCannotSayWhichLimitsItAppliesIsUnavailable(t *testing.T) {
	ctx := context.Background()

	f, a := podmanDaemon(t)
	f.on("GET /libpod/info", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) })
	_, err := a.Capabilities(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err), "an answer that is not its info")

	gone := New()
	require.NoError(t, gone.Configure(ctx, []byte(`{"host":"tcp://127.0.0.1:1"}`)))
	_, err = gone.podmanSupport(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err), "no answer at all")

	f, a = podmanDaemon(t)
	f.on("GET /libpod/info", respond(http.StatusOK, libpodInfoBody("v2", false, "cpu")))
	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.False(t, caps.SupportsResourceLimits)
	require.Contains(t, caps.ResourceLimitsRemedy, "memory controller")
	require.Contains(t, caps.ResourceLimitsRemedy, "Podman reports")
}
