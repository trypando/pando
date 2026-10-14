//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type heldRole struct {
	RoleID   string `json:"role_id"`
	RoleName string `json:"role_name"`
	Via      struct {
		Kind       string `json:"kind"`
		GroupID    string `json:"group_id"`
		Name       string `json:"name"`
		SourceName string `json:"source_name"`
	} `json:"via"`
}

type accountRoles struct {
	ID            string     `json:"id"`
	InstallRoleID string     `json:"install_role_id"`
	InstallRoles  []heldRole `json:"install_roles"`
}

// TestR078_AnAccountListsEveryInstallationRoleItHolds asserts that the
// accounts list, an account's own page and a group's members show every
// installation role an account holds and where it comes from — directly,
// through a Pando group, through a provider group linked to one, and through
// a provider group holding the role itself — which is what authorization
// evaluates (issue #221).
func TestR078_AnAccountListsEveryInstallationRoleItHolds(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	_, okta := scimProvider(t, i, admin, "Okta", nil)
	auditor := i.customRole(admin, "audit readers", "install", "install.audit.read")

	group := func(name string) string {
		var g struct {
			ID string `json:"id"`
		}
		created := i.do(admin, http.MethodPost, "/groups", map[string]any{"name": name})
		require.Equal(t, http.StatusCreated, created.Code, created.String())
		created.JSON(t, &g)
		return g.ID
	}
	setRole := func(groupID, roleID string) {
		set := i.do(admin, http.MethodPut, "/groups/"+groupID+"/role", map[string]string{"role_id": roleID})
		require.Equal(t, http.StatusOK, set.Code, set.String())
	}
	join := func(groupID, userID string) {
		require.Equal(t, http.StatusNoContent,
			i.do(admin, http.MethodPut, "/groups/"+groupID+"/members/"+userID, nil).Code)
	}

	// Gwen holds a role only through a Pando group.
	auditors := group("Auditors")
	setRole(auditors, auditor)
	gwen := i.user("gwen")
	gwenID := i.userID(gwen)
	join(auditors, gwenID)

	// Dana holds one directly and one through the same group.
	dana := i.user("dana")
	danaID := i.userID(dana)
	require.Equal(t, http.StatusOK, i.do(admin, http.MethodPut, "/users/"+danaID+"/role",
		map[string]string{"role_id": auditor}).Code)
	join(auditors, danaID)

	// Eve is in two Okta groups: Engineering, linked to the Pando group Ops,
	// and pando-administrator, which holds Administrator itself.
	var eve struct {
		ID string `json:"id"`
	}
	okta.do(http.MethodPost, "/Users", map[string]any{"userName": "eve", "externalId": "00ueve", "active": true}).JSON(t, &eve)
	var eng, admins struct {
		ID string `json:"id"`
	}
	okta.do(http.MethodPost, "/Groups", map[string]any{"displayName": "Engineering",
		"members": []map[string]string{{"value": eve.ID}}}).JSON(t, &eng)
	okta.do(http.MethodPost, "/Groups", map[string]any{"displayName": "pando-administrator",
		"members": []map[string]string{{"value": eve.ID}}}).JSON(t, &admins)
	ops := group("Ops")
	setRole(ops, auditor)
	require.Equal(t, http.StatusNoContent, i.do(admin, http.MethodPut, "/groups/"+ops+"/links/"+eng.ID, nil).Code)
	setRole(admins.ID, "role_administrator")

	nobody := i.user("nobody")
	nobodyID := i.userID(nobody)

	var list struct {
		Users []accountRoles `json:"users"`
	}
	listed := i.do(admin, http.MethodGet, "/users", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.String())
	listed.JSON(t, &list)
	byID := map[string]accountRoles{}
	for _, u := range list.Users {
		byID[u.ID] = u
	}

	check := func(a accountRoles, want ...string) {
		t.Helper()
		got := []string{}
		for _, h := range a.InstallRoles {
			got = append(got, h.RoleID+" "+h.Via.Kind+" "+h.Via.Name+" "+h.Via.SourceName)
		}
		require.Equal(t, append([]string{}, want...), got, a.ID)
	}

	check(byID[gwenID], auditor+" group Auditors ")
	require.Equal(t, auditors, byID[gwenID].InstallRoles[0].Via.GroupID)
	require.Equal(t, "audit readers", byID[gwenID].InstallRoles[0].RoleName,
		"the role's name travels with it: your own account needs no install.view, and GET /roles does")
	require.Empty(t, byID[gwenID].InstallRoleID, "install_role_id is the direct grant, and Gwen has none")

	check(byID[danaID], auditor+" direct  ", auditor+" group Auditors ")
	require.Equal(t, auditor, byID[danaID].InstallRoleID)

	check(byID[eve.ID], auditor+" group Ops ", "role_administrator group pando-administrator Okta")
	require.Equal(t, ops, byID[eve.ID].InstallRoles[0].Via.GroupID)

	check(byID[nobodyID])
	require.NotNil(t, byID[nobodyID].InstallRoles, "an empty list, not null")

	// The account's own page and the group's members say the same.
	var one accountRoles
	i.do(admin, http.MethodGet, "/users/"+eve.ID, nil).JSON(t, &one)
	require.Equal(t, byID[eve.ID], one)
	i.do(gwen, http.MethodGet, "/users/"+gwenID, nil).JSON(t, &one)
	require.Equal(t, byID[gwenID], one, "your own account, without install.view")

	var members struct {
		Members []accountRoles `json:"members"`
	}
	i.do(admin, http.MethodGet, "/groups/"+auditors+"/members", nil).JSON(t, &members)
	require.Len(t, members.Members, 2)
	for _, m := range members.Members {
		require.Equal(t, byID[m.ID], m)
	}

	// The same answer authorization gives.
	require.Equal(t, http.StatusOK, i.do(gwen, http.MethodGet, "/audit", nil).Code)
	require.Equal(t, http.StatusForbidden, i.do(nobody, http.MethodGet, "/audit", nil).Code)

	// Leaving the group takes the role off the list, live (R-079).
	require.Equal(t, http.StatusNoContent,
		i.do(admin, http.MethodDelete, "/groups/"+auditors+"/members/"+gwenID, nil).Code)
	i.do(admin, http.MethodGet, "/users/"+gwenID, nil).JSON(t, &one)
	check(one)
}
