//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/update"
	"github.com/trypando/pando/internal/httpapi"
)

type fakeUpdates struct{ st update.Status }

func (f fakeUpdates) Status(context.Context) (update.Status, error) { return f.st, nil }

// GET /updates is what the console, CLI and MCP all read (R-351), behind
// install.view like the rest of the installation's state.
func TestR351_UpdatesAreServedToWhoeverMayViewTheInstallation(t *testing.T) {
	i := newInstall(t)
	i.Server.Version = "0.3.1"
	i.Server.Updates = fakeUpdates{st: update.Status{
		Current: "0.3.1", Latest: "0.4.0", Available: true, Enabled: true, Channel: "stable",
		Releases: []update.Release{{Version: "0.4.0", Breaking: true, Notes: "### Security\n\nNo new advisories.\n"}},
	}}

	r := i.anon(http.MethodGet, "/updates", nil)
	require.Equal(t, http.StatusUnauthorized, r.Code, r.String())

	r = i.do(i.admin(), http.MethodGet, "/updates", nil)
	require.Equal(t, http.StatusOK, r.Code, r.String())
	var st update.Status
	r.JSON(t, &st)
	require.True(t, st.Available)
	require.Equal(t, "0.4.0", st.Latest)
	require.True(t, st.Releases[0].Breaking)

	someone := i.user("viewer")
	r = i.do(someone, http.MethodGet, "/updates", nil)
	require.Equal(t, http.StatusForbidden, r.Code, "a user holding nothing install-wide is not told")
}

// TestR353_TheServerVersionGoesOnlyToSignedInCallers asserts the server half
// of R-353: the CLI compares against Pando-Version, which an anonymous caller
// never sees.
func TestR353_TheServerVersionGoesOnlyToSignedInCallers(t *testing.T) {
	i := newInstall(t)
	i.Server.Version = "v0.3.1"

	r := i.do(i.admin(), http.MethodGet, "/me", nil)
	require.Equal(t, http.StatusOK, r.Code, r.String())
	require.Equal(t, "0.3.1", r.Hdr.Get(httpapi.VersionHeader))

	r = i.anon(http.MethodGet, "/me", nil)
	require.Empty(t, r.Hdr.Get(httpapi.VersionHeader))
}

// Both settings are ordinary host policy: validated on save, and settable at
// startup like every other field (R-271).
func TestR350_AnUpdateChannelThatIsNotOneIsRefused(t *testing.T) {
	i := newInstall(t)
	admin := i.admin()
	var doc map[string]any
	i.do(admin, http.MethodGet, "/policy", nil).JSON(t, &doc)

	doc["update_channel"] = "nightly"
	r := i.do(admin, http.MethodPut, "/policy", doc)
	require.Equal(t, http.StatusBadRequest, r.Code, r.String())
	require.Contains(t, r.String(), "stable or prerelease")

	doc["update_channel"] = "prerelease"
	doc["disable_update_check"] = true
	r = i.do(admin, http.MethodPut, "/policy", doc)
	require.Equal(t, http.StatusOK, r.Code, r.String())
}
