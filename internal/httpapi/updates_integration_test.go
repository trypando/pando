//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/update"
	"github.com/trypando/pando/internal/core/upgrade"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/httpapi"
)

type fakeUpgrades struct {
	started []upgrade.Request
	by      []authz.Principal
}

func (f *fakeUpgrades) PlanFor(_ context.Context, v string) (upgrade.Plan, error) {
	return upgrade.Plan{Current: "0.3.1", Target: v, Possible: true, Reasons: []string{}, Breaking: []update.Release{}}, nil
}
func (f *fakeUpgrades) Start(_ context.Context, p authz.Principal, req upgrade.Request) (upgrade.Attempt, error) {
	f.started = append(f.started, req)
	f.by = append(f.by, p)
	return upgrade.Attempt{ID: "upg_1", From: "0.3.1", To: req.Version, State: upgrade.StateRunning}, nil
}
func (f *fakeUpgrades) Last(context.Context) (*upgrade.Attempt, error) {
	return &upgrade.Attempt{ID: "upg_1", State: upgrade.StateSucceeded}, nil
}

// TestR356_UpgradingIsInstallUpgradeAndAgentsDoNotHoldIt asserts R-356 at the
// API: an administrator may, someone holding nothing may not, and an agent's
// token may not by default — even its owner's — because policy.Default()
// denies install.upgrade to agents.
func TestR356_UpgradingIsInstallUpgradeAndAgentsDoNotHoldIt(t *testing.T) {
	i := newInstall(t)
	fake := &fakeUpgrades{}
	i.Server.Upgrades = fake
	admin := i.admin()

	r := i.do(admin, http.MethodGet, "/upgrade?version=0.4.0", nil)
	require.Equal(t, http.StatusOK, r.Code, r.String())
	r = i.do(admin, http.MethodGet, "/upgrade", nil)
	require.Equal(t, http.StatusBadRequest, r.Code, "a plan needs a version")
	r = i.do(admin, http.MethodGet, "/upgrade/last", nil)
	require.Equal(t, http.StatusOK, r.Code, r.String())

	r = i.do(admin, http.MethodPost, "/upgrade", map[string]any{"version": "0.4.0", "passphrase": "a long passphrase"})
	require.Equal(t, http.StatusAccepted, r.Code, r.String())
	require.Equal(t, "0.4.0", fake.started[0].Version)
	require.Equal(t, "a long passphrase", fake.started[0].Passphrase.Reveal())
	require.Equal(t, i.AdminID, fake.by[0].ID)

	nobody := i.user("nobody")
	r = i.do(nobody, http.MethodPost, "/upgrade", map[string]any{"version": "0.4.0", "skip_backup": true})
	require.Equal(t, http.StatusForbidden, r.Code, r.String())
	r = i.do(nobody, http.MethodGet, "/upgrade?version=0.4.0", nil)
	require.Equal(t, http.StatusForbidden, r.Code, r.String())

	agent := i.tokenFor(admin)
	r = i.do(agent, http.MethodPost, "/upgrade", map[string]any{"version": "0.4.0", "skip_backup": true})
	require.Equal(t, http.StatusForbidden, r.Code, r.String())
	require.Len(t, fake.started, 1, "only the administrator's request reached the service")

	r = i.do(admin, http.MethodPost, "/upgrade", "not an object")
	require.Equal(t, http.StatusBadRequest, r.Code, r.String())
	require.Contains(t, r.String(), "skip_backup", "the refusal shows the body it wants")

	i.Server.Upgrades = &failingUpgrades{}
	r = i.do(admin, http.MethodGet, "/upgrade?version=0.4.0", nil)
	require.Equal(t, http.StatusBadRequest, r.Code, r.String())
	r = i.do(admin, http.MethodPost, "/upgrade", map[string]any{"version": "0.4.0", "skip_backup": true})
	require.Equal(t, http.StatusBadRequest, r.Code, r.String())
	require.Contains(t, r.String(), "cannot upgrade itself")
	r = i.do(admin, http.MethodGet, "/upgrade/last", nil)
	require.Equal(t, http.StatusInternalServerError, r.Code, r.String())

	i.Server.Upgrades = nil
	r = i.do(admin, http.MethodGet, "/upgrade/last", nil)
	require.Equal(t, http.StatusInternalServerError, r.Code)
}

// failingUpgrades refuses everything, as the service does when an upgrade is
// not possible.
type failingUpgrades struct{}

func (failingUpgrades) PlanFor(context.Context, string) (upgrade.Plan, error) {
	return upgrade.Plan{}, errs.New(errs.ValidInvalid, "Pando cannot plan an upgrade to that version.")
}
func (failingUpgrades) Start(context.Context, authz.Principal, upgrade.Request) (upgrade.Attempt, error) {
	return upgrade.Attempt{}, errs.New(errs.ValidInvalid, "Pando cannot upgrade itself to 0.4.0: in-place upgrades are off.")
}
func (failingUpgrades) Last(context.Context) (*upgrade.Attempt, error) {
	return nil, errs.New(errs.Internal, "The last upgrade's outcome does not parse.")
}

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
