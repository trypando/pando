//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// TestR048_ASuspendedAccountCannotReinstateItself asserts that suspension holds
// against the account itself: a token it minted while active cannot lift the
// suspension or read the account, and nobody changes their own status.
func TestR048_ASuspendedAccountCannotReinstateItself(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	sam := i.user("sam")
	samID := i.userID(sam)
	tok := i.tokenFor(sam)

	// Not even while active: status is somebody else's to change.
	own := i.do(sam, http.MethodPatch, "/users/"+samID, map[string]any{"status": "suspended"})
	require.Equal(t, http.StatusBadRequest, own.Code, own.String())
	ownAdmin := i.do(admin, http.MethodPatch, "/users/"+i.AdminID, map[string]any{"status": "suspended"})
	require.Equal(t, http.StatusBadRequest, ownAdmin.Code, ownAdmin.String())

	suspended := i.do(admin, http.MethodPatch, "/users/"+samID, map[string]any{"status": "suspended"})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, suspended.Code, suspended.String())

	back := i.do(tok, http.MethodPatch, "/users/"+samID, map[string]any{"status": "active"})
	require.GreaterOrEqual(t, back.Code, 400, back.String())
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden},
		i.do(tok, http.MethodGet, "/users/"+samID, nil).Code)

	user, found, err := i.Users.ByID(context.Background(), samID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "suspended", user.Status, "still suspended")
}

// TestR048_ChangingAnotherAccountsStatusNeedsAccountManagement asserts that a
// status change on someone else needs install.users.manage held now, not only
// a path to the account.
func TestR048_ChangingAnotherAccountsStatusNeedsAccountManagement(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	viewer, _ := i.holding("viewer", authz.InstallView)
	other := i.userID(i.user("other"))

	got := i.do(viewer, http.MethodPatch, "/users/"+other, map[string]any{"status": "suspended"})
	require.Equal(t, http.StatusForbidden, got.Code, got.String())

	desk, _ := i.holding("desk", authz.InstallUsersManage)
	got = i.do(desk, http.MethodPatch, "/users/"+other, map[string]any{"status": "suspended"})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, got.Code, got.String())
}

// TestR049_ADeletedAccountIsNotBroughtBackByAStatusChange asserts that a
// status change never revives a deleted account, whoever asks.
func TestR049_ADeletedAccountIsNotBroughtBackByAStatusChange(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	gone := i.userID(i.user("gone"))

	deleted := i.do(admin, http.MethodDelete, "/users/"+gone, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, deleted.Code, deleted.String())

	got := i.do(admin, http.MethodPatch, "/users/"+gone, map[string]any{"status": "active"})
	require.GreaterOrEqual(t, got.Code, 400, got.String())

	ctx := context.Background()
	for _, status := range []string{"active", "suspended"} {
		err := i.Users.SetStatus(ctx, gone, status)
		require.Equal(t, errs.NotFound, errs.CodeOf(err), status)
	}
	var revived int
	require.NoError(t, i.db.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE id = $1 AND deleted_at IS NULL`, gone).Scan(&revived))
	require.Zero(t, revived)
}

// TestR155_AnOwnerCannotGrantThemselvesApproval asserts that app.grants.manage
// hands on only the app verbs its holder holds on the app: an Owner cannot
// give themselves app.deploy.approve, which would undo the two-person rule an
// installation keeps by withholding it.
func TestR155_AnOwnerCannotGrantThemselvesApproval(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	owner := i.user("olive")
	ownerID := i.userID(owner)
	i.grantControl(appID, owner, authz.RoleOwner)
	approver := i.customRole(admin, "approver", "app", "app.view", "app.deploy.approve")

	got := i.do(owner, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": ownerID, "role_id": approver,
	})
	requireMissingVerbs(t, got)

	// Nor by changing an existing grant's role.
	viewer := i.user("vic")
	i.grantControl(appID, viewer, authz.RoleViewer)
	var grants struct {
		Grants []struct {
			ID          string `json:"id"`
			PrincipalID string `json:"principal_id"`
		} `json:"grants"`
	}
	i.do(owner, http.MethodGet, "/apps/"+appID+"/grants", nil).JSON(t, &grants)
	var grantID string
	for _, g := range grants.Grants {
		if g.PrincipalID == i.userID(viewer) {
			grantID = g.ID
		}
	}
	require.NotEmpty(t, grantID)
	requireMissingVerbs(t, i.do(owner, http.MethodPatch, "/apps/"+appID+"/grants/"+grantID,
		map[string]any{"role_id": approver}))

	// What the Owner holds it still hands on, and an administrator, who holds
	// install.deploys.approve, may grant approval.
	other := i.user("otto")
	got = i.do(owner, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": i.userID(other), "role_id": authz.RoleViewer,
	})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
	got = i.do(admin, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": i.userID(i.user("abe")), "role_id": approver,
	})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
}

// TestR080a_ManagingEveryAppsGrantsDoesNotConferApproval asserts the same
// install-wide: install.apps.grants.manage hands on on any app only what its
// holder holds there.
func TestR080a_ManagingEveryAppsGrantsDoesNotConferApproval(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	manager, managerID := i.holding("manager", authz.InstallAppsView, authz.InstallAppsGrantsManage)
	approver := i.customRole(admin, "approver", "app", "app.view", "app.deploy.approve")
	viewOnly := i.customRole(admin, "looker", "app", "app.view")

	requireMissingVerbs(t, i.do(manager, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": managerID, "role_id": approver,
	}))
	got := i.do(manager, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": managerID, "role_id": viewOnly,
	})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
}

// TestR154_PinningARevisionOnADeployedAppIsADeploy asserts that pinning a
// revision on an app that has been deployed, which changes what it runs, is
// held to a deploy's permission and approval; on an app that has never run it
// stays a spec edit.
func TestR154_PinningARevisionOnADeployedAppIsADeploy(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	ctx := context.Background()
	appID := a.createApp(admin, "notes")
	editor := a.user("eddie")
	a.grantControl(appID, editor, a.customRole(admin, "editor", "app", "app.view", "app.spec.edit"))
	pin := func(s *session, rev int) reply {
		return a.do(s, http.MethodPost, fmt.Sprintf("/apps/%s/specs/%d/pin", appID, rev), map[string]any{})
	}

	// Never deployed: a spec edit.
	first := a.writeSpec(admin, appID, minimalSpec())
	got := pin(editor, first)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	app, _, err := a.Apps.ByID(ctx, appID)
	require.NoError(t, err)
	ran, err := a.Server.Deployments.Create(ctx, appID, app.PinnedSpecID, state.TriggerManual, a.AdminID)
	require.NoError(t, err)
	require.NoError(t, a.Server.Deployments.Finish(ctx, ran.ID, state.DeploySucceeded, "", ""))
	require.NoError(t, a.Apps.SetState(ctx, appID, state.StateRunning))

	// Running: app.spec.edit alone no longer changes what runs.
	second := a.writeSpec(admin, appID, minimalSpec())
	got = pin(editor, second)
	require.Equal(t, http.StatusForbidden, got.Code, got.String())
	app, _, err = a.Apps.ByID(ctx, appID)
	require.NoError(t, err)
	require.Equal(t, ran.SpecID, app.PinnedSpecID, "the pin did not move")

	// A deploy that would need approval is not taken around it by a pin.
	// A revision that does not plan is not pinned either. The harness
	// configures no runtime, so nothing plans here; past this the plan is
	// not what is asserted.
	got = pin(admin, second)
	require.Equal(t, "PLAN_ADAPTER_NOT_CONFIGURED", got.ErrorCode(), got.String())
	a.Server.Planner = nil
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalRequired = true })
	got = pin(admin, second)
	require.Equal(t, http.StatusConflict, got.Code, got.String())
	require.Contains(t, got.String(), "Deploy revision")

	// A revision that already ran needs none (R-157).
	got = pin(admin, first)
	require.Equal(t, http.StatusOK, got.Code, got.String())
}

// TestR061_ATokenNeverOutlivesTheLifetimeCap asserts that host policy's token
// lifetime caps an expiry asked for, not only a missing one, for delegated and
// account tokens alike.
func TestR061_ATokenNeverOutlivesTheLifetimeCap(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	i.setPolicy(func(d *corepolicy.Document) { d.MaxTokenLifetimeDays = 30 })
	limit := time.Now().UTC().AddDate(0, 0, 30).Add(time.Minute)

	for _, path := range []string{"/tokens", "/tokens/service"} {
		for _, days := range []int{0, 36500} {
			got := i.do(admin, http.MethodPost, path, map[string]any{"name": fmt.Sprintf("t%d", days), "expires_days": days})
			require.Equal(t, http.StatusCreated, got.Code, got.String())
			at := expiresAt(t, got)
			require.NotNil(t, at, "%s %d", path, days)
			require.True(t, at.Before(limit), "%s %d: %s", path, days, at)
		}
	}

	// A shorter one is kept as asked.
	got := i.do(admin, http.MethodPost, "/tokens", map[string]any{"name": "short", "expires_days": 7})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
	at := expiresAt(t, got)
	require.NotNil(t, at)
	require.True(t, at.Before(time.Now().UTC().AddDate(0, 0, 8)))
}

// expiresAt is when the token a creation answered with expires.
func expiresAt(t *testing.T, created reply) *time.Time {
	t.Helper()
	var body struct {
		Token struct {
			ExpiresAt *time.Time `json:"expires_at"`
		} `json:"token"`
	}
	created.JSON(t, &body)
	return body.Token.ExpiresAt
}

// TestR046_APasswordSomebodyElseSetCannotBeTurnedIntoAToken asserts that a
// session that must change its password reaches only its account, the
// password change and signing out: minting a token first is refused.
func TestR046_APasswordSomebodyElseSetCannotBeTurnedIntoAToken(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	erin := i.userMustChange("erin")

	got := i.do(erin, http.MethodPost, "/tokens", map[string]any{"name": "laptop"})
	require.Equal(t, http.StatusForbidden, got.Code, got.String())
	require.Equal(t, string(errs.PermPasswordChangeRequired), got.ErrorCode())
	require.Equal(t, string(errs.PermPasswordChangeRequired), i.do(erin, http.MethodGet, "/apps", nil).ErrorCode())
	require.Equal(t, http.StatusOK, i.do(erin, http.MethodGet, "/me", nil).Code)

	changed := i.do(erin, http.MethodPost, "/me/password", map[string]string{
		"current_password": "another-correct-horse-staple", "new_password": "erin-chose-this-one",
	})
	require.Equal(t, http.StatusNoContent, changed.Code, changed.String())
	got = i.do(erin, http.MethodPost, "/tokens", map[string]any{"name": "laptop"})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
}

// TestR059_AnAdministratorsResetRevokesTheAccountsTokens asserts that an
// administrator's password or two-factor reset ends every token the account
// owns, so nothing minted before it outlasts it.
func TestR059_AnAdministratorsResetRevokesTheAccountsTokens(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	for _, reset := range []struct {
		name, method, path string
		body               any
	}{
		{"password", http.MethodPost, "/password", map[string]any{"password": "a-long-enough-new-password"}},
		{"two-factor", http.MethodDelete, "/two-factor", nil},
	} {
		holder := i.user("holder-" + reset.name)
		id := i.userID(holder)
		tok := i.tokenFor(holder)
		require.Equal(t, http.StatusOK, i.do(tok, http.MethodGet, "/me", nil).Code)

		got := i.do(admin, reset.method, "/users/"+id+reset.path, reset.body)
		require.Equal(t, http.StatusNoContent, got.Code, reset.name+": "+got.String())
		require.Equal(t, http.StatusUnauthorized, i.do(tok, http.MethodGet, "/me", nil).Code, reset.name)
	}
}

// TestR082_AppsAreListedOnlyWithAppView asserts that GET /apps lists an app
// only for someone holding app.view on it: a custom role without it lets its
// holder act on the app by ID and does not list it.
func TestR082_AppsAreListedOnlyWithAppView(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	restarter := i.user("rita")
	i.grantControl(appID, restarter, i.customRole(admin, "restarter", "app", "app.restart"))
	viewer := i.user("val")
	i.grantControl(appID, viewer, authz.RoleViewer)

	listed := func(s *session) bool {
		var page struct {
			Apps []struct {
				ID string `json:"id"`
			} `json:"apps"`
		}
		got := i.do(s, http.MethodGet, "/apps", nil)
		require.Equal(t, http.StatusOK, got.Code, got.String())
		got.JSON(t, &page)
		for _, a := range page.Apps {
			if a.ID == appID {
				return true
			}
		}
		return false
	}
	require.False(t, listed(restarter))
	require.True(t, listed(viewer))
}
