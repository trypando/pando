//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR081_AnAdministratorManagesAnAppSomebodyElseMade asserts the
// install.apps.* verbs: the Administrator sees, and can do everything to, an
// app it holds no grant on — and a custom role holding install.apps.view and
// install.apps.logs.read sees it and can change nothing.
func TestR081_AnAdministratorManagesAnAppSomebodyElseMade(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	carol := i.user("carol")
	carolID := i.userID(carol)
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPut, "/users/"+carolID+"/role", map[string]string{"role_id": "role_creator"}).Code)
	appID := i.createApp(carol, "carols-notes")

	// Listed, and every app verb, without a grant on it.
	var list struct {
		Apps []struct {
			ID string `json:"id"`
		} `json:"apps"`
	}
	i.do(admin, http.MethodGet, "/apps", nil).JSON(t, &list)
	require.True(t, listed(list.Apps, appID), "an administrator lists every app")

	var one struct {
		Verbs []string `json:"verbs"`
	}
	i.do(admin, http.MethodGet, "/apps/"+appID, nil).JSON(t, &one)
	require.Contains(t, one.Verbs, "app.delete")
	require.Contains(t, one.Verbs, "app.grants.manage")
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPatch, "/apps/"+appID, map[string]string{"name": "Carol's notes"}).Code)

	// Carol's own role on it, and the administrator may change it in one step.
	type userApps struct {
		Apps []struct {
			AppID     string `json:"app_id"`
			Owner     bool   `json:"owner"`
			CanManage bool   `json:"can_manage"`
			Control   []struct {
				GrantID string `json:"grant_id"`
				RoleID  string `json:"role_id"`
				Via     string `json:"via"`
			} `json:"control"`
		} `json:"apps"`
	}
	var got userApps
	i.do(admin, http.MethodGet, "/users/"+carolID+"/apps", nil).JSON(t, &got)
	require.Len(t, got.Apps, 1)
	require.True(t, got.Apps[0].Owner)
	require.True(t, got.Apps[0].CanManage)
	require.Len(t, got.Apps[0].Control, 1)
	require.Equal(t, "role_owner", got.Apps[0].Control[0].RoleID)

	grant := got.Apps[0].Control[0].GrantID
	require.Equal(t, http.StatusNoContent,
		i.do(admin, http.MethodPatch, "/apps/"+appID+"/grants/"+grant, map[string]string{"role_id": "role_operator"}).Code)
	i.do(admin, http.MethodGet, "/users/"+carolID+"/apps", nil).JSON(t, &got)
	require.Equal(t, "role_operator", got.Apps[0].Control[0].RoleID)

	// An installation role is not an app role.
	bad := i.do(admin, http.MethodPatch, "/apps/"+appID+"/grants/"+grant, map[string]string{"role_id": "role_administrator"})
	require.Equal(t, http.StatusBadRequest, bad.Code, bad.String())

	// View-only: sees the app and Carol's role on it, and may change neither.
	viewerRole := i.customRole(admin, "app auditor", "install", "install.view", "install.apps.view", "install.apps.logs.read")
	vic := i.user("vic")
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPut, "/users/"+i.userID(vic)+"/role", map[string]string{"role_id": viewerRole}).Code)

	i.do(vic, http.MethodGet, "/apps", nil).JSON(t, &list)
	require.True(t, listed(list.Apps, appID))
	i.do(vic, http.MethodGet, "/apps/"+appID, nil).JSON(t, &one)
	require.ElementsMatch(t, []string{"app.view", "app.logs.read"}, one.Verbs)
	require.Equal(t, http.StatusForbidden,
		i.do(vic, http.MethodPatch, "/apps/"+appID, map[string]string{"name": "Mine now"}).Code)

	i.do(vic, http.MethodGet, "/users/"+carolID+"/apps", nil).JSON(t, &got)
	require.Len(t, got.Apps, 1)
	require.False(t, got.Apps[0].CanManage)
	require.Equal(t, http.StatusForbidden,
		i.do(vic, http.MethodPatch, "/apps/"+appID+"/grants/"+grant, map[string]string{"role_id": "role_owner"}).Code)
}

// TestR080_AnInstallWideVerbReachesAppsMadeLater asserts issue #81: a group
// holding one app verb's install-wide counterpart holds that verb on every
// app, including one made after the grant, and nothing else — not even
// app.view, so the app list does not open up to it — and none of it is use
// (R-087).
func TestR080_AnInstallWideVerbReachesAppsMadeLater(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	var team struct {
		ID string `json:"id"`
	}
	created := i.do(admin, http.MethodPost, "/groups", map[string]any{"name": "Restarters"})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	created.JSON(t, &team)
	role := i.customRole(admin, "restart every app", "install", "install.apps.view", "install.apps.restart")
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPut, "/groups/"+team.ID+"/role", map[string]string{"role_id": role}).Code)

	rita := i.user("rita")
	members := i.do(admin, http.MethodPut, "/groups/"+team.ID+"/members", map[string]any{"members": []string{i.userID(rita)}})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, members.Code, members.String())

	// Made after the grant, by somebody else.
	carol := i.user("carol")
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPut, "/users/"+i.userID(carol)+"/role", map[string]string{"role_id": "role_creator"}).Code)
	appID := i.createApp(carol, "made-later")

	var one struct {
		Verbs []string `json:"verbs"`
	}
	got := i.do(rita, http.MethodGet, "/apps/"+appID, nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	got.JSON(t, &one)
	require.ElementsMatch(t, []string{"app.view", "app.restart"}, one.Verbs)
	require.Equal(t, http.StatusForbidden,
		i.do(rita, http.MethodPatch, "/apps/"+appID, map[string]string{"name": "Mine now"}).Code)

	// The built-in Auditor: every app's view and logs, the audit log, and
	// nothing it could change.
	audrey := i.user("audrey")
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPut, "/users/"+i.userID(audrey)+"/role", map[string]string{"role_id": "role_auditor"}).Code)
	i.do(audrey, http.MethodGet, "/apps/"+appID, nil).JSON(t, &one)
	require.ElementsMatch(t, []string{"app.view", "app.logs.read"}, one.Verbs)
	require.Equal(t, http.StatusOK, i.do(audrey, http.MethodGet, "/audit", nil).Code)
	require.Equal(t, http.StatusForbidden, i.do(audrey, http.MethodGet, "/users", nil).Code,
		"an auditor does not hold install.view")
}

// An account's app list shows only apps the viewer could see anyway: it is not
// a way to learn that an app exists.
func TestAccountAppsListOnlyWhatTheViewerCanSee(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	dana := i.user("dana")
	danaID := i.userID(dana)
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPut, "/users/"+danaID+"/role", map[string]string{"role_id": "role_creator"}).Code)
	i.createApp(dana, "private")

	// install.view alone reads accounts, but not their apps.
	peeker := i.customRole(admin, "directory", "install", "install.view")
	pat := i.user("pat")
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPut, "/users/"+i.userID(pat)+"/role", map[string]string{"role_id": peeker}).Code)

	got := i.do(pat, http.MethodGet, "/users/"+danaID+"/apps", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.JSONEq(t, `{"apps":[],"next_cursor":""}`, got.String())

	// Dana's own, with nothing administrative.
	got = i.do(dana, http.MethodGet, "/users/"+danaID+"/apps", nil)
	require.Contains(t, got.String(), `"can_manage":true`)
}

func listed(apps []struct {
	ID string `json:"id"`
}, id string) bool {
	for _, a := range apps {
		if a.ID == id {
			return true
		}
	}
	return false
}
