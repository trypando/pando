//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// providerSession signs in an account from an identity provider, as a
// provider sign-in would leave it: a session for an account whose adapter is
// not the local one.
func (i *install) providerSession() (*session, string) {
	i.t.Helper()
	ctx := context.Background()
	_, err := i.db.Exec(ctx, `
		INSERT INTO identity_adapters (id, kind, name, config) VALUES ('idp_okta', 'oidc', 'Okta', '{}')
		ON CONFLICT DO NOTHING`)
	require.NoError(i.t, err)
	u, err := state.NewUsers(i.db).Create(ctx, "idp_okta", "okta-sub", "", "Okta Person", "", false)
	require.NoError(i.t, err)
	sess, err := state.NewSessions(i.db, testTokenKey).Create(ctx, u.ID, "idp_okta", time.Hour, "", "")
	require.NoError(i.t, err)
	return &session{cookie: sess.Cookie.Reveal()}, u.ID
}

// TestR405_AProviderAccountHasNoTwoFactorHere asserts that an account from an
// identity provider is never asked for, and cannot set up, Pando's second
// factor (R-044): its provider authenticates it. An administrator has nothing
// to reset on it either.
func TestR405_AProviderAccountHasNoTwoFactorHere(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	s, userID := i.providerSession()

	_, tf := meTwoFactor(t, i, s)
	require.Nil(t, tf, "GET /me says nothing about a second factor")

	for _, path := range []string{"/me/two-factor/totp", "/me/two-factor/skip"} {
		got := i.do(s, http.MethodPost, path, map[string]any{})
		require.Equal(t, http.StatusBadRequest, got.Code, "%s: %s", path, got.String())
		require.Contains(t, got.String(), "identity provider")
	}

	// Under the policy too: the provider decides, so its sessions are not
	// held back.
	i.requireLocalTwoFactor(true)
	require.NotEqual(t, http.StatusForbidden, i.do(s, http.MethodGet, "/me/apps", nil).Code)
	i.requireLocalTwoFactor(false)

	reset := i.do(i.admin(), http.MethodDelete, "/users/"+userID+"/two-factor", nil)
	require.Equal(t, http.StatusBadRequest, reset.Code, reset.String())
	require.Contains(t, reset.String(), "identity provider")
}

func TestR405_TwoFactorRoutesRefuseWhoCannotHaveOne(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	anon := i.anon(http.MethodPost, "/me/two-factor/totp", map[string]any{})
	require.Equal(t, http.StatusUnauthorized, anon.Code, anon.String())

	created := i.do(i.admin(), http.MethodPost, "/tokens/service", map[string]any{"name": "CI"})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var issued struct {
		Secret string `json:"secret"`
	}
	created.JSON(t, &issued)
	agent := &session{token: issued.Secret}
	got := i.do(agent, http.MethodPost, "/me/two-factor/totp", map[string]any{})
	require.Equal(t, http.StatusBadRequest, got.Code, got.String())
	require.Contains(t, got.String(), "token has no account")

	gone := i.do(i.admin(), http.MethodDelete, "/users/usr_01JNOSUCHACCOUNT0000000000/two-factor", nil)
	require.Equal(t, http.StatusNotFound, gone.Code, gone.String())
}

func TestR405_BadRequestsToTheTwoFactorRoutes(t *testing.T) {
	t.Parallel()
	i, _ := signInLimited(t)
	dana := i.user("dana")

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/me/two-factor/totp"},
		{http.MethodPost, "/me/two-factor/totp/confirm"},
		{http.MethodPost, "/me/two-factor/recovery-codes"},
		{http.MethodDelete, "/me/two-factor"},
	} {
		got := i.do(dana, route.method, route.path, "not an object")
		require.Equal(t, http.StatusBadRequest, got.Code, "%s %s: %s", route.method, route.path, got.String())
	}

	// Nothing being set up: nothing to confirm.
	nothing := i.do(dana, http.MethodPost, "/me/two-factor/totp/confirm", map[string]string{"code": "123456"})
	require.Equal(t, http.StatusConflict, nothing.Code, nothing.String())

	// Without two-factor there are no recovery codes to replace.
	none := i.do(dana, http.MethodPost, "/me/two-factor/recovery-codes", map[string]any{})
	require.Equal(t, http.StatusConflict, none.Code, none.String())

	// Turning off what is not on is not an event.
	off := i.do(dana, http.MethodDelete, "/me/two-factor", nil)
	require.Equal(t, http.StatusNoContent, off.Code, off.String())
}

// TestR405_WrongConfirmingCodesAreLimited asserts the confirming code is held
// to the same count as a sign-in's: six digits are guessed nowhere for free.
func TestR405_WrongConfirmingCodesAreLimited(t *testing.T) {
	t.Parallel()
	i, _ := signInLimited(t)
	dana := i.user("dana")
	begun := i.do(dana, http.MethodPost, "/me/two-factor/totp", map[string]any{})
	require.Equal(t, http.StatusOK, begun.Code, begun.String())
	var e struct {
		Secret string `json:"secret"`
	}
	begun.JSON(t, &e)

	for range 5 {
		got := i.do(dana, http.MethodPost, "/me/two-factor/totp/confirm", map[string]string{"code": "000000"})
		require.Equal(t, http.StatusUnauthorized, got.Code, got.String())
	}
	limited := i.do(dana, http.MethodPost, "/me/two-factor/totp/confirm",
		map[string]string{"code": totpAt(t, e.Secret, time.Now())})
	require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.String())
}

// TestR405_TheCurrentPasswordIsHeldToTheSignInCounts asserts that guessing
// the password to change a second factor runs out like sign-in does (R-404).
func TestR405_TheCurrentPasswordIsHeldToTheSignInCounts(t *testing.T) {
	t.Parallel()
	i, _ := signInLimited(t)
	dana := i.user("dana")
	userID, _ := meTwoFactor(t, i, dana)
	_, err := i.db.Exec(context.Background(),
		`UPDATE sessions SET issued_at = now() - interval '1 hour' WHERE user_id = $1`, userID)
	require.NoError(t, err)

	for range 10 {
		got := i.do(dana, http.MethodPost, "/me/two-factor/totp", map[string]string{"current_password": "wrong-password-here"})
		require.Equal(t, http.StatusUnauthorized, got.Code, got.String())
	}
	limited := i.do(dana, http.MethodPost, "/me/two-factor/totp", map[string]string{"current_password": danaPassword})
	require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.String())
}

// A server built without two-factor authentication says so on its routes.
func TestTwoFactorRoutesWithoutTheService(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	dana := i.user("dana")
	admin := i.admin()
	userID, _ := meTwoFactor(t, i, dana)
	i.Server.TwoFactor = nil

	got := i.do(dana, http.MethodPost, "/me/two-factor/totp", map[string]any{})
	require.Equal(t, http.StatusConflict, got.Code, got.String())
	reset := i.do(admin, http.MethodDelete, "/users/"+userID+"/two-factor", nil)
	require.Equal(t, http.StatusConflict, reset.Code, reset.String())
	_, tf := meTwoFactor(t, i, dana)
	require.Nil(t, tf)
}
