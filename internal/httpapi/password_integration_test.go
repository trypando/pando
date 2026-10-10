//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/httpapi"
)

// TestR046_TheSetupTokenHolderSetsUpTheAdministrator asserts first-run setup
// over the API: public while there is no account but only with the setup
// token Pando printed (issue #130), signs the new administrator in, and
// refused from then on.
func TestR046_TheSetupTokenHolderSetsUpTheAdministrator(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	// Set up already, by the harness; nothing to claim.
	require.JSONEq(t, `{"needed":false}`, i.do(nil, http.MethodGet, "/setup", nil).String())
	refused := i.do(nil, http.MethodPost, "/setup", map[string]string{"username": "mallory", "password": "a-long-enough-password"})
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	require.Contains(t, refused.String(), "already set up")

	// With no account at all, it is open to whoever has the setup token.
	require.NoError(t, i.Users.Delete(context.Background(), i.AdminID))
	require.JSONEq(t, `{"needed":true}`, i.do(nil, http.MethodGet, "/setup", nil).String())
	token, err := i.Users.ReplaceSetupToken(context.Background())
	require.NoError(t, err)

	for _, wrong := range []string{"", "not-the-token"} {
		first := i.do(nil, http.MethodPost, "/setup", map[string]string{
			"setup_token": wrong, "username": "mallory", "password": "a-long-enough-password",
		})
		require.Equal(t, http.StatusUnauthorized, first.Code, first.String())
		require.Contains(t, first.String(), "AUTH_INVALID")
		require.Contains(t, first.String(), "setup token")
	}
	require.JSONEq(t, `{"needed":true}`, i.do(nil, http.MethodGet, "/setup", nil).String())

	short := i.do(nil, http.MethodPost, "/setup", map[string]string{"username": "ada", "password": "short"})
	require.Equal(t, http.StatusBadRequest, short.Code, short.String())

	got := i.do(nil, http.MethodPost, "/setup", map[string]string{
		"setup_token": token, "username": "ada", "display_name": "Ada", "password": "a-password-ada-chose",
	})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
	s := &session{}
	for _, c := range (&http.Response{Header: got.Hdr}).Cookies() {
		if c.Name == httpapi.SessionCookie {
			s.cookie = c.Value
		}
	}
	require.NotEmpty(t, s.cookie, "setup signs the administrator in")

	var me struct {
		Verbs []string `json:"verbs"`
	}
	i.do(s, http.MethodGet, "/me", nil).JSON(t, &me)
	require.Contains(t, me.Verbs, "install.users.manage")

	again := i.do(nil, http.MethodPost, "/setup", map[string]string{"setup_token": token, "username": "eve", "password": "a-long-enough-password"})
	require.Equal(t, http.StatusBadRequest, again.Code, again.String())
}

// TestR046_AdministratorsHandOverGeneratedPasswords asserts creating and
// resetting an account with a generated password: must-change by default,
// every session ended on reset, and not for your own account.
func TestR046_AdministratorsHandOverGeneratedPasswords(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	// Only for someone who manages accounts.
	ordinary := i.user("ordinary")
	require.Equal(t, http.StatusForbidden, i.do(ordinary, http.MethodPost, "/passwords/generate", nil).Code)

	var gen struct {
		Password string `json:"password"`
	}
	i.do(admin, http.MethodPost, "/passwords/generate", nil).JSON(t, &gen)
	require.GreaterOrEqual(t, len(gen.Password), 18)

	created := i.do(admin, http.MethodPost, "/users", map[string]any{"username": "dana", "password": gen.Password})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	require.Contains(t, created.String(), `"must_change_password":true`, "handed over, so changed at first sign-in")

	dana, signed := i.signIn("dana", gen.Password)
	require.Equal(t, http.StatusOK, signed.Code, signed.String())
	danaID := i.userID(dana)

	// Reset: the old session ends, the new password works, and the flag is
	// what the administrator chose.
	i.do(admin, http.MethodPost, "/passwords/generate", nil).JSON(t, &gen)
	reset := i.do(admin, http.MethodPost, "/users/"+danaID+"/password",
		map[string]any{"password": gen.Password, "must_change_password": false})
	require.Equal(t, http.StatusNoContent, reset.Code, reset.String())
	require.Equal(t, http.StatusUnauthorized, i.do(dana, http.MethodGet, "/me", nil).Code, "every session ended")
	_, signed = i.signIn("dana", gen.Password)
	require.Equal(t, http.StatusOK, signed.Code, signed.String())
	require.Contains(t, signed.String(), `"must_change_password":false`)

	// Not your own, and not somebody who does not manage accounts.
	self := i.do(admin, http.MethodPost, "/users/"+i.AdminID+"/password", map[string]any{"password": gen.Password})
	require.Equal(t, http.StatusBadRequest, self.Code, self.String())
	require.Equal(t, http.StatusForbidden,
		i.do(ordinary, http.MethodPost, "/users/"+danaID+"/password", map[string]any{"password": gen.Password}).Code)
	missing := i.do(admin, http.MethodPost, "/users/usr_01JZZZZZZZZZZZZZZZZZZZZZZZ/password", map[string]any{"password": gen.Password})
	require.Equal(t, http.StatusNotFound, missing.Code, missing.String())
}
