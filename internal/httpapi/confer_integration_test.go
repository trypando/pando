//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/errs"
)

// holding signs in a new account holding a custom installation role made of
// verbs, and returns it with its account ID.
func (i *install) holding(name string, verbs ...authz.Verb) (*session, string) {
	i.t.Helper()
	admin := i.admin()
	role := i.do(admin, http.MethodPost, "/roles", map[string]any{"name": name + " role", "scope": "install", "verbs": verbs})
	require.Equal(i.t, http.StatusCreated, role.Code, role.String())
	var r struct {
		ID string `json:"id"`
	}
	role.JSON(i.t, &r)

	s := i.user(name)
	id := i.idOf(s)
	granted := i.do(admin, http.MethodPut, "/users/"+id+"/role", map[string]any{"role_id": r.ID})
	require.Equal(i.t, http.StatusOK, granted.Code, granted.String())
	return s, id
}

func (i *install) idOf(s *session) string {
	i.t.Helper()
	var me struct {
		UserID string `json:"user_id"`
	}
	i.do(s, http.MethodGet, "/me", nil).JSON(i.t, &me)
	require.NotEmpty(i.t, me.UserID)
	return me.UserID
}

func requireMissingVerbs(t *testing.T, got reply) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, got.Code, got.String())
	require.Equal(t, string(errs.PermVerbRequired), got.ErrorCode(), got.String())
	require.Contains(t, got.String(), "missing_verbs")
}

// TestR080a_AnAccountManagerCannotBecomeAnAdministrator asserts R-080a for
// finding 7 of the 2026-10-16 assessment: a custom role holding
// install.users.manage alone could grant itself Administrator, compose a role
// with more, join a group that holds it, or take over an administrator's
// account. Each is refused, and what it holds it may still do.
func TestR080a_AnAccountManagerCannotBecomeAnAdministrator(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	desk, deskID := i.holding("desk", authz.InstallUsersManage)

	// Granting itself, or anyone, more than it holds.
	requireMissingVerbs(t, i.do(desk, http.MethodPut, "/users/"+deskID+"/role", map[string]any{"role_id": authz.RoleAdministrator}))
	requireMissingVerbs(t, i.do(desk, http.MethodPost, "/roles", map[string]any{
		"name": "more", "scope": "install", "verbs": []authz.Verb{authz.InstallUsersManage, authz.InstallAdaptersManage},
	}))

	// Joining a group that holds it, or giving its own group the role.
	group := i.do(admin, http.MethodPost, "/groups", map[string]any{"name": "admins"})
	require.Equal(t, http.StatusCreated, group.Code, group.String())
	var g struct {
		ID string `json:"id"`
	}
	group.JSON(t, &g)
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPut, "/groups/"+g.ID+"/role", map[string]any{"role_id": authz.RoleAdministrator}).Code)
	requireMissingVerbs(t, i.do(desk, http.MethodPut, "/groups/"+g.ID+"/members/"+deskID, nil))
	requireMissingVerbs(t, i.do(desk, http.MethodPut, "/groups/"+g.ID+"/members", map[string]any{"members": []string{deskID}}))
	requireMissingVerbs(t, i.do(desk, http.MethodDelete, "/groups/"+g.ID+"/role", nil))
	requireMissingVerbs(t, i.do(desk, http.MethodDelete, "/groups/"+g.ID, nil))
	requireMissingVerbs(t, i.do(desk, http.MethodDelete, "/roles/"+authz.RoleAdministrator, nil))

	// Taking over, locking out, or removing the administrator.
	requireMissingVerbs(t, i.do(desk, http.MethodPost, "/users/"+i.AdminID+"/password",
		map[string]any{"password": "a-long-enough-new-password"}))
	requireMissingVerbs(t, i.do(desk, http.MethodPatch, "/users/"+i.AdminID, map[string]any{"status": "suspended"}))
	requireMissingVerbs(t, i.do(desk, http.MethodPatch, "/users/"+i.AdminID, map[string]any{"email": "desk@example.com"}))
	requireMissingVerbs(t, i.do(desk, http.MethodDelete, "/users/"+i.AdminID+"/role", nil))
	requireMissingVerbs(t, i.do(desk, http.MethodDelete, "/users/"+i.AdminID, nil))

	// The administrator is untouched and still signs in.
	_, back := i.signIn(bootstrap.AdminUsername, i.adminPassword)
	require.Equal(t, http.StatusOK, back.Code, back.String())

	// What it holds, it may still do: manage an account holding nothing, and
	// hand on its own role.
	plain := i.user("plain")
	plainID := i.idOf(plain)
	require.Equal(t, http.StatusNoContent, i.do(desk, http.MethodPost, "/users/"+plainID+"/password",
		map[string]any{"password": "a-long-enough-new-password"}).Code)
	var deskRole struct {
		Roles []struct {
			RoleID string `json:"role_id"`
		} `json:"install_roles"`
	}
	i.do(admin, http.MethodGet, "/users/"+deskID, nil).JSON(t, &deskRole)
	require.NotEmpty(t, deskRole.Roles)
	shared := i.do(desk, http.MethodPut, "/users/"+plainID+"/role", map[string]any{"role_id": deskRole.Roles[0].RoleID})
	require.Equal(t, http.StatusOK, shared.Code, shared.String())

	// And an administrator may do all of it.
	require.Equal(t, http.StatusNoContent, i.do(admin, http.MethodPut, "/groups/"+g.ID+"/members/"+deskID, nil).Code)
}

// TestR080a_AnAdapterManagerCannotConfigureWhoSignsIn asserts R-080a for
// findings 4 and 5 of the 2026-10-16 assessment: install.adapters.manage
// alone could register an identity provider it ran, or point an AI adapter's
// key variable at Pando's own environment and its base URL at itself.
func TestR080a_AnAdapterManagerCannotConfigureWhoSignsIn(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	ops, _ := i.holding("ops", authz.InstallAdaptersManage)

	provider := map[string]any{
		"kind": "oidc", "name": "Mine", "enabled": true, "link_by_email": true, "jit_provisioning": true,
		"config": map[string]any{"issuer": "https://idp.attacker.example", "client_id": "x"},
	}
	refused := i.do(ops, http.MethodPost, "/identity-providers", provider)
	requireMissingVerbs(t, refused)
	require.Contains(t, refused.String(), "decides who can sign in as which account")

	id := oidcProvider(t, i, admin, "Okta", nil)
	requireMissingVerbs(t, i.do(ops, http.MethodPatch, "/identity-providers/"+id, map[string]any{"link_by_email": true}))
	requireMissingVerbs(t, i.do(ops, http.MethodPost, "/identity-providers/"+id+"/scim-token", nil))

	// Its own work it still does: an AI adapter with a stored key.
	for name, cfg := range map[string]map[string]any{
		"another variable": {"api_key_env": "PANDO_DATABASE_URL"},
		"another address":  {"base_url": "https://attacker.example"},
	} {
		got := i.do(ops, http.MethodPost, "/adapters", map[string]any{
			"id": "ai_x", "category": "ai", "kind": "anthropic", "config": cfg,
		})
		require.Equal(t, http.StatusBadRequest, got.Code, "%s: %s", name, got.String())
		require.Contains(t, got.String(), "environment", name)
	}
	ok := i.do(ops, http.MethodPost, "/adapters", map[string]any{
		"id": "ai_x", "category": "ai", "kind": "anthropic", "enabled": false,
		"config":      map[string]any{"base_url": "https://gateway.example", "api_key_env": "ANTHROPIC_API_KEY"},
		"credentials": map[string]string{"api_key": "sk-ant-test"},
	})
	require.Equal(t, http.StatusCreated, ok.Code, ok.String())
}

// TestR080a_RestoringTheInstallationNeedsEveryInstallVerb asserts R-080a: a
// restore replaces every grant with the backup's, so install.backup.manage
// alone could restore one from when it held more.
func TestR080a_RestoringTheInstallationNeedsEveryInstallVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	keeper, _ := i.holding("keeper", authz.InstallBackupManage)
	got := i.do(keeper, http.MethodPost, "/backups/bak_any/restore", map[string]any{"passphrase": "x", "confirm": true})
	requireMissingVerbs(t, got)
	require.Contains(t, got.String(), "replaces every permission in the installation")
}

// TestR081_TheAdministratorRoleHoldsEveryInstallVerb asserts R-081, on which
// R-080a's "every install verb" checks stand: if the seeded role lacked one,
// administrators could not configure identity providers.
func TestR081_TheAdministratorRoleHoldsEveryInstallVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	var verbs []string
	require.NoError(t, i.db.QueryRow(t.Context(),
		`SELECT verbs FROM roles WHERE id = $1`, authz.RoleAdministrator).Scan(&verbs))
	for _, v := range authz.Verbs {
		if authz.InstallScoped(v) {
			require.Contains(t, verbs, string(v))
		}
	}
}
