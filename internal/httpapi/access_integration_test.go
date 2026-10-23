//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/bootstrap"
)

// --- accounts --------------------------------------------------------------

// O-17: reading or changing your own account needs nothing administrative;
// doing either to someone else needs an install-scoped verb. Before those verbs
// existed these were gated only by being signed in, which meant any account
// could suspend the administrator.
func TestO17_ManagingSomeoneElsesAccountNeedsAnInstallVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	other := i.user("ordinary")

	// Their own is fine.
	own := i.do(other, http.MethodGet, "/me", nil)
	require.Equal(t, http.StatusOK, own.Code, own.String())

	// The administrator's is not.
	denied := i.do(other, http.MethodGet, "/users/"+i.AdminID, nil)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())

	// And the administrator can read it.
	allowed := i.do(admin, http.MethodGet, "/users/"+i.AdminID, nil)
	require.Equal(t, http.StatusOK, allowed.Code, allowed.String())
}

// R-080: an ordinary account must not be able to suspend the administrator.
func TestR080_AnOrdinaryUserCannotSuspendTheAdministrator(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	other := i.user("ordinary")

	got := i.do(other, http.MethodPatch, "/users/"+i.AdminID, map[string]any{"status": "suspended"})
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, got.Code, got.String())

	// The administrator can still sign in.
	_, after := i.signIn(bootstrap.AdminUsername, i.adminPassword)
	require.Equal(t, http.StatusOK, after.Code, after.String())
}

func TestCreatingAnAccountIsAdministration(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	other := i.user("ordinary")

	denied := i.do(other, http.MethodPost, "/users", map[string]any{
		"username": "sneaky", "password": "a-long-enough-password", "display_name": "Sneaky",
	})
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())

	listed := i.do(i.admin(), http.MethodGet, "/users", nil)
	require.Equal(t, http.StatusOK, listed.Code)
	require.NotContains(t, listed.String(), "sneaky")
}

// R-194: a password digest is never rendered, wherever a user is returned.
func TestR194_AUserIsNeverReturnedWithAnythingCredentialShaped(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	i.user("ordinary")

	for _, path := range []string{"/users", "/users/" + i.AdminID, "/me"} {
		got := i.do(admin, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, got.Code, path)
		// must_change_password is a flag, not a credential. What must never
		// appear is the stored digest or anything it is made of.
		for _, forbidden := range []string{"password_hash", "digest", "argon2", "$2a$", "$argon2"} {
			require.NotContains(t, got.String(), forbidden, "%s leaked %q", path, forbidden)
		}
	}
}

// R-049: suspension is not deletion, and neither is reachable by mistyping the
// other — they are separate routes.
func TestR049_SuspendingAnAccountStopsItSigningInWithoutDeletingIt(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	created := i.do(admin, http.MethodPost, "/users", map[string]any{
		"username": "temp", "password": "a-long-enough-password", "display_name": "Temp",
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var user struct {
		ID string `json:"id"`
	}
	created.JSON(t, &user)

	_, before := i.signIn("temp", "a-long-enough-password")
	require.Equal(t, http.StatusOK, before.Code)

	suspended := i.do(admin, http.MethodPatch, "/users/"+user.ID, map[string]any{"status": "suspended"})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, suspended.Code, suspended.String())

	_, after := i.signIn("temp", "a-long-enough-password")
	require.GreaterOrEqual(t, after.Code, 400, "a suspended account cannot sign in")

	// Still there: suspension is not deletion.
	still := i.do(admin, http.MethodGet, "/users/"+user.ID, nil)
	require.Equal(t, http.StatusOK, still.Code, still.String())
}

// Changing your own password is self only, and needs no verb.
func TestChangingYourOwnPassword(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	got := i.do(admin, http.MethodPost, "/me/password", map[string]any{
		"current_password": i.adminPassword,
		"new_password":     "an-entirely-different-password",
	})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, got.Code, got.String())

	_, old := i.signIn(bootstrap.AdminUsername, i.adminPassword)
	require.GreaterOrEqual(t, old.Code, 400, "the old password stops working")

	_, fresh := i.signIn(bootstrap.AdminUsername, "an-entirely-different-password")
	require.Equal(t, http.StatusOK, fresh.Code, fresh.String())
}

func TestChangingYourPasswordNeedsTheCurrentOne(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	got := i.do(i.admin(), http.MethodPost, "/me/password", map[string]any{
		"current_password": "not the password",
		"new_password":     "an-entirely-different-password",
	})
	require.GreaterOrEqual(t, got.Code, 400, got.String())
	require.NotEqual(t, http.StatusInternalServerError, got.Code)
}

// R-088: the last administrator cannot be removed, or the install is left with
// nobody who can administer it.
func TestR088_TheLastAdministratorCannotBeRemoved(t *testing.T) {
	t.Parallel()
	// Every route that would leave the installation with nobody who can manage
	// accounts. An install in that state cannot be repaired through the API —
	// the only way back is `pando admin` against the database, which needs
	// shell access to the host.
	//
	// Suspension is on this list because it was not, and nothing stopped an
	// administrator suspending themselves: the same lockout as revoking the
	// role, in one request, reached by a different route.
	for _, route := range []struct {
		name, method, path string
		body               any
	}{
		{"delete the account", http.MethodDelete, "/users/%s", nil},
		{"revoke the role", http.MethodDelete, "/users/%s/role", nil},
		{"suspend the account", http.MethodPatch, "/users/%s", map[string]any{"status": "suspended"}},
	} {
		t.Run(route.name, func(t *testing.T) {
			i := newInstall(t)
			admin := i.admin()

			got := i.do(admin, route.method, fmt.Sprintf(route.path, i.AdminID), route.body)
			require.GreaterOrEqual(t, got.Code, 400, got.String())
			require.NotEqual(t, http.StatusInternalServerError, got.Code, got.String())

			var env struct {
				Message string `json:"message"`
				Remedy  string `json:"remedy"`
			}
			got.JSON(t, &env)
			require.NotEmpty(t, env.Message)

			// And the install still has an administrator who can sign in.
			_, after := i.signIn(bootstrap.AdminUsername, i.adminPassword)
			require.Equal(t, http.StatusOK, after.Code, after.String())
			require.Equal(t, http.StatusOK, i.do(i.admin(), http.MethodGet, "/users", nil).Code)
		})
	}
}

// Suspension is still available once somebody else can administer the install,
// which is the difference between a guard and a prohibition.
func TestSuspendingAnAdministratorIsAllowedWhenAnotherRemains(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	created := i.do(admin, http.MethodPost, "/users", map[string]any{
		"username": "second-admin", "password": "a-long-enough-password", "display_name": "Second",
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var second struct {
		ID string `json:"id"`
	}
	created.JSON(t, &second)

	promoted := i.do(admin, http.MethodPut, "/users/"+second.ID+"/role",
		map[string]any{"role_id": "role_administrator"})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent, http.StatusCreated},
		promoted.Code, promoted.String())

	// The second administrator can still get in, which is what makes it safe,
	// and suspends the first: nobody changes their own status (R-048).
	deputy, in := i.signIn("second-admin", "a-long-enough-password")
	require.Equal(t, http.StatusOK, in.Code, in.String())
	i.keepPassword(deputy, in, "a-long-enough-password")

	suspended := i.do(deputy, http.MethodPatch, "/users/"+i.AdminID,
		map[string]any{"status": "suspended"})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, suspended.Code, suspended.String())
}

// --- groups and roles ------------------------------------------------------

// R-078: group names cross the identity boundary; what a group can do is
// Pando's.
func TestR078_GroupsAreCreatedAndListedByAnAdministrator(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	created := i.do(admin, http.MethodPost, "/groups", map[string]any{
		"name": "platform", "members": []string{i.AdminID},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())

	var group struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	created.JSON(t, &group)
	require.Equal(t, "platform", group.Name)
	require.NotEmpty(t, group.ID)

	listed := i.do(admin, http.MethodGet, "/groups", nil)
	require.Equal(t, http.StatusOK, listed.Code)
	require.Contains(t, listed.String(), "platform")

	members := i.do(admin, http.MethodPut, "/groups/"+group.ID+"/members",
		map[string]any{"members": []string{}})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, members.Code, members.String())

	deleted := i.do(admin, http.MethodDelete, "/groups/"+group.ID, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, deleted.Code, deleted.String())
}

func TestGroupsAreAdministrationRatherThanSelfService(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	other := i.user("ordinary")

	for _, got := range []reply{
		i.do(other, http.MethodGet, "/groups", nil),
		i.do(other, http.MethodPost, "/groups", map[string]any{"name": "mine"}),
	} {
		require.GreaterOrEqual(t, got.Code, 400, got.String())
		require.NotEqual(t, http.StatusInternalServerError, got.Code)
	}
}

// The verb catalog, for composing a custom role (R-082).
func TestTheVerbCatalogIsReadable(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	got := i.do(i.admin(), http.MethodGet, "/verbs", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	var body struct {
		Verbs []map[string]any `json:"verbs"`
	}
	got.JSON(t, &body)
	require.NotEmpty(t, body.Verbs)

	// Both planes and both scopes are described, because a role is composed
	// from them and a catalog missing half is a catalog that misleads.
	require.Contains(t, got.String(), "app.")
	require.Contains(t, got.String(), "install.")
}

// R-081: built-in roles are immutable, and new verbs arrive by migration only.
func TestR081_ABuiltInRoleCannotBeDeleted(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	listed := i.do(admin, http.MethodGet, "/roles", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.String())

	var body struct {
		Roles []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			BuiltIn bool   `json:"built_in"`
		} `json:"roles"`
	}
	listed.JSON(t, &body)
	require.NotEmpty(t, body.Roles, "an install ships with roles")

	for _, role := range body.Roles {
		if !role.BuiltIn {
			continue
		}
		got := i.do(admin, http.MethodDelete, "/roles/"+role.ID, nil)
		require.GreaterOrEqual(t, got.Code, 400, "%s was deletable", role.Name)
		require.NotEqual(t, http.StatusInternalServerError, got.Code, got.String())
	}
}

// R-082: a custom role is composed from the verb catalog.
func TestR082_ACustomRoleIsCreatedFromVerbsAndCanBeRemoved(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	created := i.do(admin, http.MethodPost, "/roles", map[string]any{
		"name":  "log-reader",
		"scope": "app",
		"verbs": []string{"app.view", "app.logs.read"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())

	var role struct {
		ID string `json:"id"`
	}
	created.JSON(t, &role)
	require.NotEmpty(t, role.ID)

	deleted := i.do(admin, http.MethodDelete, "/roles/"+role.ID, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, deleted.Code, deleted.String())
}

// R-080: an install-scoped grant carries install verbs and no app, and vice
// versa. A role for one scope cannot be granted in the other.
func TestR080_AScopeMismatchIsRefusedRatherThanEvaluated(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	got := i.do(admin, http.MethodPost, "/roles", map[string]any{
		"name": "confused", "scope": "install", "verbs": []string{"app.view"},
	})
	require.GreaterOrEqual(t, got.Code, 400, got.String())
	require.NotEqual(t, http.StatusInternalServerError, got.Code)
}

// --- grants ----------------------------------------------------------------

// Sharing an app normally means letting someone use it, not letting them
// redeploy it: the two planes are never conflated (R-029, R-070/071).
func TestR029_SharingAnAppOnTheDataPlaneDoesNotLetSomeoneManageIt(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	other := i.user("ordinary")

	id := i.createApp(admin, "notes")

	var recipient struct {
		Users []struct {
			ID string `json:"id"`
			// The local adapter's external_id is the username (R-044: the
			// adapter owns the namespace, Pando owns the ID).
			Username string `json:"external_id"`
		} `json:"users"`
	}
	i.do(admin, http.MethodGet, "/users", nil).JSON(t, &recipient)

	var otherID string
	for _, u := range recipient.Users {
		if u.Username == "ordinary" {
			otherID = u.ID
		}
	}
	require.NotEmpty(t, otherID)

	granted := i.do(admin, http.MethodPost, "/apps/"+id+"/grants", map[string]any{
		"plane": "data", "principal_kind": "user", "principal_id": otherID,
	})
	require.Equal(t, http.StatusCreated, granted.Code, granted.String())

	// A data-plane grant is permission to use the app, not to manage it.
	denied := i.do(other, http.MethodPost, "/apps/"+id+"/deployments", map[string]any{})
	require.GreaterOrEqual(t, denied.Code, 400, denied.String())
	require.NotEqual(t, http.StatusInternalServerError, denied.Code)

	_ = other
}

func TestGrantsAreListedAndRevoked(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	listed := i.do(admin, http.MethodGet, "/apps/"+id+"/grants", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.String())
	require.Contains(t, listed.String(), "grants")

	// Removing a grant that is not there is not an error: revoking access
	// somebody already does not have has the outcome the caller asked for.
	gone := i.do(admin, http.MethodDelete, "/apps/"+id+"/grants/gr_nonexistent", nil)
	require.Contains(t, []int{http.StatusNoContent, http.StatusOK, http.StatusNotFound},
		gone.Code, gone.String())
}

// Only someone who can manage the app's access may change it (R-083-adjacent):
// a grant is the thing that decides who else gets in.
func TestOnlyTheAppsOwnerCanShareIt(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	other := i.user("ordinary")
	id := i.createApp(admin, "notes")

	got := i.do(other, http.MethodPost, "/apps/"+id+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": "usr_whoever",
	})
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, got.Code, got.String())
}
