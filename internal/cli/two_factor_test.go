package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// twoStepAPI answers POST /sessions with AUTH_TWO_FACTOR_REQUIRED until a
// code comes with it, as Pando does for an account with two-factor
// authentication (R-405).
type twoStepAPI struct {
	mu       sync.Mutex
	sessions []string
	url      string
}

func newTwoStepAPI(t *testing.T, signedIn map[string]any) *twoStepAPI {
	t.Helper()
	a := &twoStepAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions":
			a.mu.Lock()
			a.sessions = append(a.sessions, string(raw))
			a.mu.Unlock()
			var body map[string]string
			_ = json.Unmarshal(raw, &body)
			if body["code"] == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": "AUTH_TWO_FACTOR_REQUIRED", "message": "This account has two-factor authentication.",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(signedIn)
		case r.URL.Path == "/api/v1/tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"secret": "tok_secret_value"})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

// TestR405_LoginAsksForACodeWhenTheAccountHasTwoFactor asserts R-405 in the
// CLI: the password, then a code, then the same sign-in again with both.
func TestR405_LoginAsksForACodeWhenTheAccountHasTwoFactor(t *testing.T) {
	api := newTwoStepAPI(t, map[string]any{"user_id": "usr_01HQ8"})
	got := run(t, &fakeAPI{url: api.url}, "ben\nhunter2\n123456\n", "login", api.url)
	require.NoError(t, got.err)
	require.Len(t, api.sessions, 2)
	require.JSONEq(t, `{"username":"ben","password":"hunter2","code":"123456"}`, api.sessions[1])
	require.Contains(t, got.out, "Signed in to "+api.url)
}

// TestR405_LoginStopsOnAnAccountThatMustSetUpTwoFactor asserts that an
// account held back by policy cannot mint a token: it sets up two-factor
// authentication in the console first.
func TestR405_LoginStopsOnAnAccountThatMustSetUpTwoFactor(t *testing.T) {
	api := newAPI(t).reply("POST /sessions",
		map[string]any{"user_id": "usr_01HQ8", "two_factor_setup_required": true})
	got := run(t, api, "ben\nhunter2\n", "login")
	require.ErrorContains(t, got.err, "requires two-factor authentication")
	require.False(t, api.sawPath("/tokens"), "no token is minted")
}

func TestR405_TwoFactorStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		me   map[string]any
		want []string
	}{
		"on": {map[string]any{"two_factor": map[string]any{"enabled": true, "required": true, "recovery_codes_remaining": 7}},
			[]string{"is on. 7 recovery codes left", "requires it"}},
		"off":      {map[string]any{"two_factor": map[string]any{"enabled": false}}, []string{"is off", "pando two-factor setup"}},
		"provider": {map[string]any{}, []string{"identity provider"}},
	} {
		t.Run(name, func(t *testing.T) {
			api := newAPI(t).reply("GET /me", tc.me)
			got := run(t, api, "", "two-factor", "status")
			require.NoError(t, got.err)
			for _, w := range tc.want {
				require.Contains(t, got.out, w)
			}
		})
	}
}

// TestR405_TwoFactorSetupShowsAQRCodeAndTheRecoveryCodes asserts the CLI's
// setup: the password, a QR code and the key, the code, then the recovery
// codes with the instruction to save them.
func TestR405_TwoFactorSetupShowsAQRCodeAndTheRecoveryCodes(t *testing.T) {
	api := newAPI(t).
		reply("POST /me/two-factor/totp", map[string]any{
			"secret": "JBSWY3DPEHPK3PXP", "uri": "otpauth://totp/Pando:ben?secret=JBSWY3DPEHPK3PXP&issuer=Pando",
		}).
		reply("POST /me/two-factor/totp/confirm", map[string]any{"recovery_codes": []string{"aaaa-bbbb-cccc-dddd"}})

	got := run(t, api, "hunter2\n123456\n", "two-factor", "setup")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"current_password":"hunter2"}`, api.bodyFor("POST /me/two-factor/totp"))
	require.JSONEq(t, `{"code":"123456"}`, api.bodyFor("POST /me/two-factor/totp/confirm"))
	require.Contains(t, got.out, "█", "a QR code is drawn")
	require.Contains(t, got.out, "JBSW Y3DP EHPK 3PXP", "the key in groups of four")
	require.Contains(t, got.out, "aaaa-bbbb-cccc-dddd")
	require.Contains(t, got.out, "Save them somewhere safe")
}

func TestR405_TwoFactorRecoveryCodesAndDisable(t *testing.T) {
	api := newAPI(t).
		reply("POST /me/two-factor/recovery-codes", map[string]any{"recovery_codes": []string{"eeee-ffff-gggg-hhhh"}})
	got := run(t, api, "hunter2\n", "two-factor", "recovery-codes")
	require.NoError(t, got.err)
	require.Contains(t, got.out, "old recovery codes no longer work")
	require.Contains(t, got.out, "eeee-ffff-gggg-hhhh")

	got = run(t, api, "hunter2\n", "two-factor", "disable")
	require.NoError(t, got.err)
	require.JSONEq(t, `{"current_password":"hunter2"}`, api.bodyFor("DELETE /me/two-factor"))
	require.Contains(t, got.out, "is off")
}

func TestR405_TwoFactorCommandsReportRefusals(t *testing.T) {
	refusal := map[string]any{"code": "POLICY_TWO_FACTOR_REQUIRED", "message": "This installation requires it."}
	api := newAPI(t).
		fail("DELETE /me/two-factor", http.StatusForbidden, refusal).
		fail("POST /me/two-factor/totp", http.StatusUnauthorized, map[string]any{"code": "AUTH_INVALID", "message": "That current password is not right."}).
		fail("POST /me/two-factor/recovery-codes", http.StatusConflict, map[string]any{"code": "STATE_INVALID", "message": "Not on."}).
		fail("GET /me", http.StatusUnauthorized, map[string]any{"code": "AUTH_REQUIRED", "message": "You need to sign in."})
	require.ErrorContains(t, run(t, api, "pw\n", "two-factor", "disable").err, "requires it")
	require.ErrorContains(t, run(t, api, "pw\n", "two-factor", "setup").err, "not right")
	require.ErrorContains(t, run(t, api, "pw\n", "two-factor", "recovery-codes").err, "Not on")
	require.ErrorContains(t, run(t, api, "", "two-factor", "status").err, "sign in")

	confirm := newAPI(t).
		reply("POST /me/two-factor/totp", map[string]any{"secret": "ABCD", "uri": "otpauth://totp/x"}).
		fail("POST /me/two-factor/totp/confirm", http.StatusUnauthorized, map[string]any{"code": "AUTH_INVALID", "message": "That code is not right."})
	require.ErrorContains(t, run(t, confirm, "pw\n000000\n", "two-factor", "setup").err, "code is not right")
}

func TestR405_UserResetTwoFactor(t *testing.T) {
	api := newAPI(t)
	got := run(t, api, "", "user", "reset-two-factor", "usr_01HQ8")
	require.NoError(t, got.err)
	require.True(t, api.sawPath("/users/usr_01HQ8/two-factor"))
	require.True(t, strings.Contains(got.out, "signed out everywhere"))

	refused := newAPI(t).fail("DELETE /users/usr_01HQ8/two-factor", http.StatusForbidden,
		map[string]any{"code": "PERM_VERB_REQUIRED", "message": "You need install.users.manage."})
	require.ErrorContains(t, run(t, refused, "", "user", "reset-two-factor", "usr_01HQ8").err, "install.users.manage")
}
