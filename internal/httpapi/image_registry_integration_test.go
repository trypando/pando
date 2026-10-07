//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR194_TheImageRegistryPasswordIsSetHereAndNeverReadBack asserts R-194 and
// R-190 through the API (issue #72): an administrator sets the install
// registry with its password, every response says only that a password is
// set, and the audit event names the fields changed and not their values.
// Changing it needs install.adapters.manage; reading it, install.view.
func TestR194_TheImageRegistryPasswordIsSetHereAndNeverReadBack(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	set := i.do(admin, http.MethodPut, "/image-registry", map[string]any{
		"url": "https://registry.internal:5000", "username": "pando", "password": "registry-do-not-leak",
	})
	require.Equal(t, http.StatusOK, set.Code, set.String())
	require.NotContains(t, set.String(), "registry-do-not-leak")
	var view struct {
		Configured  bool   `json:"configured"`
		URL         string `json:"url"`
		PasswordSet bool   `json:"password_set"`
	}
	set.JSON(t, &view)
	require.True(t, view.Configured)
	require.True(t, view.PasswordSet)

	got := i.do(admin, http.MethodGet, "/image-registry", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.NotContains(t, got.String(), "registry-do-not-leak")
	require.Contains(t, got.String(), `"password_set":true`)

	audit := i.do(admin, http.MethodGet, "/audit?action=install.registry.update", nil)
	require.Equal(t, http.StatusOK, audit.Code, audit.String())
	require.Contains(t, audit.String(), "install.registry.update")
	require.NotContains(t, audit.String(), "registry-do-not-leak")

	// A malformed body is refused without quoting it back.
	bad := i.do(admin, http.MethodPut, "/image-registry", `{"password": registry-do-not-leak`)
	require.Equal(t, http.StatusBadRequest, bad.Code, bad.String())
	require.NotContains(t, bad.String(), "registry-do-not-leak")

	// An ordinary account holds no install verb.
	user := i.user("crewmate")
	denied := i.do(user, http.MethodPut, "/image-registry", map[string]any{"url": "https://evil.example"})
	require.Equal(t, http.StatusForbidden, denied.Code, denied.String())

	cleared := i.do(admin, http.MethodDelete, "/image-registry", nil)
	require.Equal(t, http.StatusNoContent, cleared.Code, cleared.String())
	got = i.do(admin, http.MethodGet, "/image-registry", nil)
	got.JSON(t, &view)
	require.False(t, view.Configured)
	require.False(t, view.PasswordSet)
}
