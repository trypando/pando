//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func (i *install) userID(s *session) string {
	i.t.Helper()
	var me struct {
		UserID string `json:"user_id"`
	}
	i.do(s, http.MethodGet, "/me", nil).JSON(i.t, &me)
	require.NotEmpty(i.t, me.UserID)
	return me.UserID
}

func (i *install) customRole(s *session, name, scope string, verbs ...string) string {
	i.t.Helper()
	got := i.do(s, http.MethodPost, "/roles", map[string]any{"name": name, "scope": scope, "verbs": verbs})
	require.Equal(i.t, http.StatusCreated, got.Code, got.String())
	var role struct {
		ID string `json:"id"`
	}
	got.JSON(i.t, &role)
	return role.ID
}

// TestR082_ACustomRoleSomeoneHoldsCanBeDeleted asserts that deleting a custom
// role takes its grants with it: the role in use could not be deleted at all,
// because the grants referenced it. Whoever held it loses what it allowed.
func TestR082_ACustomRoleSomeoneHoldsCanBeDeleted(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")

	helper := i.user("helper")
	roleID := i.customRole(admin, "log reader", "app", "app.view", "app.logs.read")
	granted := i.do(admin, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": i.userID(helper), "role_id": roleID,
	})
	require.Equal(t, http.StatusCreated, granted.Code, granted.String())
	require.Equal(t, http.StatusOK, i.do(helper, http.MethodGet, "/apps/"+appID, nil).Code)

	deleted := i.do(admin, http.MethodDelete, "/roles/"+roleID, nil)
	require.Equal(t, http.StatusNoContent, deleted.Code, deleted.String())

	require.Equal(t, http.StatusNotFound, i.do(helper, http.MethodGet, "/apps/"+appID, nil).Code,
		"the role's grant went with it")
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodDelete, "/roles/"+roleID, nil).Code)

	// Built-ins still cannot be.
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodDelete, "/roles/role_creator", nil).Code)
}

// TestR088_DeletingTheRoleThatIsTheOnlyWayToManageAccountsIsRefused asserts
// R-088 through a route that did not check it: a custom role holding
// install.users.manage, when it is the last grant that can manage accounts.
func TestR088_DeletingTheRoleThatIsTheOnlyWayToManageAccountsIsRefused(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	keeperRole := i.customRole(admin, "keeper", "install", "install.users.manage", "install.view")
	keeper := i.user("keeper")
	promoted := i.do(admin, http.MethodPut, "/users/"+i.userID(keeper)+"/role", map[string]any{"role_id": keeperRole})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated, http.StatusNoContent}, promoted.Code, promoted.String())

	// The first administrator steps down, leaving the custom role as the only
	// way anyone manages accounts. The keeper could not do it for them: the
	// administrator holds verbs the keeper does not (R-080a).
	usurped := i.do(keeper, http.MethodDelete, "/users/"+i.AdminID+"/role", nil)
	require.Equal(t, http.StatusForbidden, usurped.Code, usurped.String())
	demoted := i.do(admin, http.MethodDelete, "/users/"+i.AdminID+"/role", nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, demoted.Code, demoted.String())

	refused := i.do(keeper, http.MethodDelete, "/roles/"+keeperRole, nil)
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	require.Contains(t, refused.String(), "only administrators")

	// Nothing was taken: the keeper can still manage accounts.
	require.Equal(t, http.StatusOK, i.do(keeper, http.MethodGet, "/users", nil).Code)
}

// TestR078_DeletingAGroupTakesWhatWasSharedWithIt asserts that a group's
// members lose what the group gave them, and keep what was given to them.
func TestR078_DeletingAGroupTakesWhatWasSharedWithIt(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	viaGroup := i.createApp(admin, "wiki")
	direct := i.createApp(admin, "notes")

	member := i.user("member")
	memberID := i.userID(member)

	created := i.do(admin, http.MethodPost, "/groups", map[string]any{"name": "team", "members": []string{memberID}})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var group struct {
		ID string `json:"id"`
	}
	created.JSON(t, &group)

	for _, g := range []map[string]any{
		{"app": viaGroup, "principal_kind": "group", "principal_id": group.ID},
		{"app": direct, "principal_kind": "user", "principal_id": memberID},
	} {
		got := i.do(admin, http.MethodPost, "/apps/"+g["app"].(string)+"/grants", map[string]any{
			"plane": "data", "principal_kind": g["principal_kind"], "principal_id": g["principal_id"],
		})
		require.Equal(t, http.StatusCreated, got.Code, got.String())
	}
	before, _ := i.myLauncher(member)
	require.Len(t, before.Apps, 2)

	deleted := i.do(admin, http.MethodDelete, "/groups/"+group.ID, nil)
	require.Equal(t, http.StatusNoContent, deleted.Code, deleted.String())

	after, placed := i.myLauncher(member)
	require.Len(t, after.Apps, 1)
	_, stillThere := placed[direct]
	require.True(t, stillThere, "access given directly is kept")

	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodDelete, "/groups/"+group.ID, nil).Code)
}

// TestR081_EveryBuiltInRoleIsListed asserts the list of every role has all
// five of Pando's: the screen that says what each role is showed two, because
// it asked only for the ones granted across the installation.
func TestR081_EveryBuiltInRoleIsListed(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	i.customRole(admin, "log reader", "app", "app.view", "app.logs.read")

	list := func(scope string) []string {
		var out struct {
			Roles []struct {
				ID      string `json:"id"`
				Builtin bool   `json:"builtin"`
			} `json:"roles"`
		}
		got := i.do(admin, http.MethodGet, "/roles"+scope, nil)
		require.Equal(t, http.StatusOK, got.Code, got.String())
		got.JSON(t, &out)
		ids := []string{}
		for _, r := range out.Roles {
			if r.Builtin {
				ids = append(ids, r.ID)
			}
		}
		return ids
	}

	require.Equal(t,
		[]string{"role_administrator", "role_app_manager", "role_auditor", "role_app_viewer", "role_creator",
			"role_owner", "role_operator", "role_viewer"},
		list("?scope=all"), "installation roles first, broadest first")
	require.Equal(t, []string{"role_administrator", "role_app_manager", "role_app_viewer", "role_auditor", "role_creator"},
		list(""), "the default is unchanged: installation roles, by name")
	require.Equal(t, []string{"role_owner", "role_operator", "role_viewer"}, list("?scope=app"))
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodGet, "/roles?scope=everything", nil).Code)
}
