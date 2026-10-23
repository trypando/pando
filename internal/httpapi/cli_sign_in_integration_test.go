//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/clisignin"
)

const cliVerifier = "a-verifier-that-is-at-least-forty-three-characters-long"

// cliUser is an account that has chosen its own password, signed in.
func cliUser(i *install, username string) *session {
	i.t.Helper()
	created := i.do(i.admin(), http.MethodPost, "/users", map[string]any{
		"username": username, "display_name": username, "password": danaPassword,
		"must_change_password": false,
	})
	require.Equal(i.t, http.StatusCreated, created.Code, created.String())
	s, got := i.signIn(username, danaPassword)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	return s
}

func approveCLI(i *install, s *session, port int) reply {
	i.t.Helper()
	return i.do(s, http.MethodPost, "/cli-sign-ins", map[string]any{
		"code_challenge": clisignin.Challenge(cliVerifier), "code_challenge_method": "S256",
		"name": "laptop CLI", "port": port, "state": "state-0123456789abcdef",
	})
}

// TestR406_TheCLISignsInThroughTheBrowserEndToEnd asserts R-406 over the API:
// a person signed in to the console approves, the browser is sent to the
// CLI's loopback address with a code, and the CLI trades it with its verifier
// for a token that acts as that person — once.
func TestR406_TheCLISignsInThroughTheBrowserEndToEnd(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	dana := cliUser(i, "dana")
	danaID, _ := meTwoFactor(t, i, dana)

	approved := approveCLI(i, dana, 49152)
	require.Equal(t, http.StatusCreated, approved.Code, approved.String())
	require.Equal(t, "no-store", approved.Hdr.Get("Cache-Control"))
	var a struct {
		Code        string `json:"code"`
		RedirectURL string `json:"redirect_url"`
	}
	approved.JSON(t, &a)
	to, err := url.Parse(a.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:49152", to.Host)
	require.Equal(t, a.Code, to.Query().Get("code"))
	require.Equal(t, 1, auditCount(t, i, "cli_sign_in.approve", danaID))

	wrong := i.anon(http.MethodPost, "/cli-sign-ins/token",
		map[string]string{"code": "not-a-code", "code_verifier": cliVerifier})
	require.Equal(t, http.StatusUnauthorized, wrong.Code, wrong.String())
	require.Equal(t, "AUTH_INVALID", wrong.ErrorCode())

	got := i.anon(http.MethodPost, "/cli-sign-ins/token",
		map[string]string{"code": a.Code, "code_verifier": cliVerifier})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
	var issued struct {
		Secret   string `json:"secret"`
		Username string `json:"username"`
		Token    struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"token"`
	}
	got.JSON(t, &issued)
	require.Equal(t, "dana", issued.Username)
	require.Equal(t, "laptop CLI", issued.Token.Name)
	require.Equal(t, 1, auditCount(t, i, "token.create", issued.Token.ID))

	cli := &session{token: issued.Secret}
	asCLI, _ := meTwoFactor(t, i, cli)
	require.Equal(t, danaID, asCLI, "the token acts as the person who approved (R-058)")

	again := i.anon(http.MethodPost, "/cli-sign-ins/token",
		map[string]string{"code": a.Code, "code_verifier": cliVerifier})
	require.Equal(t, http.StatusUnauthorized, again.Code, "a code works once")

	// Guesses and spent codes leave no audit row: the route is public, and a
	// row per request would be a row anybody can add.
	var denied int
	require.NoError(t, i.db.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = 'cli_sign_in.denied'`).Scan(&denied))
	require.Equal(t, 0, denied)

	// A wrong verifier on a real approval spends it, and is recorded against
	// the account that approved it.
	second := approveCLI(i, dana, 0)
	require.Equal(t, http.StatusCreated, second.Code, second.String())
	var b struct {
		Code string `json:"code"`
	}
	second.JSON(t, &b)
	stolen := i.anon(http.MethodPost, "/cli-sign-ins/token",
		map[string]string{"code": b.Code, "code_verifier": cliVerifier + "-not-the-one"})
	require.Equal(t, http.StatusUnauthorized, stolen.Code, stolen.String())
	require.Equal(t, 1, auditCount(t, i, "cli_sign_in.denied", danaID))
}

// TestR406_OnlyASignedInPersonApprovesACLISignIn asserts R-406: nobody signed
// out, and no token, can approve — a token approving would be a token minting
// a token.
func TestR406_OnlyASignedInPersonApprovesACLISignIn(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	dana := cliUser(i, "dana")

	anon := approveCLI(i, nil, 0)
	require.Equal(t, http.StatusUnauthorized, anon.Code, anon.String())

	byToken := approveCLI(i, i.tokenFor(dana), 0)
	require.Equal(t, http.StatusForbidden, byToken.Code, byToken.String())

	bad := i.do(dana, http.MethodPost, "/cli-sign-ins", map[string]any{
		"code_challenge": "short", "code_challenge_method": "S256", "name": "x",
	})
	require.Equal(t, http.StatusBadRequest, bad.Code, bad.String())

	pasted := approveCLI(i, dana, 0)
	require.Equal(t, http.StatusCreated, pasted.Code, pasted.String())
	var a struct {
		RedirectURL string `json:"redirect_url"`
	}
	pasted.JSON(t, &a)
	require.Empty(t, a.RedirectURL, "a pasted code goes nowhere")
}

// TestR406_ARequiredSecondFactorComesFirst asserts R-405 holds for R-406: a
// session that must still set up two-factor authentication cannot approve a
// CLI sign-in, so a token cannot be made around the requirement.
func TestR406_ARequiredSecondFactorComesFirst(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	dana := cliUser(i, "dana")
	i.requireLocalTwoFactor(true)

	got := approveCLI(i, dana, 0)
	require.Equal(t, http.StatusForbidden, got.Code, got.String())
	require.Equal(t, "POLICY_TWO_FACTOR_REQUIRED", got.ErrorCode())
}

// TestR406_AGeneratedPasswordIsChangedFirst asserts R-046 holds for R-406: an
// account still on the password an administrator generated for it cannot
// approve a CLI sign-in, as it cannot sign the CLI in with that password.
func TestR406_AGeneratedPasswordIsChangedFirst(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	got := approveCLI(i, i.userMustChange("erin"), 0)
	require.Equal(t, http.StatusForbidden, got.Code, got.String())
	require.Equal(t, "PERM_PASSWORD_CHANGE_REQUIRED", got.ErrorCode())
}
