//go:build integration

package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/idp"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/httpapi"
	"github.com/trypando/pando/internal/id"
)

// ssoSignIn finishes a sign-in through the SSO completion route, as the
// callback leaves it: a flow that has authenticated userID and is waiting for
// its one-time code in the browser holding the binding.
func (i *install) ssoSignIn(userID string) string {
	i.t.Helper()
	ctx := i.t.Context()
	flows := state.NewSSOFlows(i.db)
	flowID := id.New(id.Session) + "-flow"
	bind, code := "bind-"+flowID, "code-"+flowID
	require.NoError(i.t, flows.Create(ctx, state.SSOFlow{
		ID: flowID, AdapterID: state.LocalAdapterID, Purpose: state.FlowSignIn, BindHash: idp.Digest(bind),
		ReturnOrigin: "http://example.com", NextPath: "/", CallbackURL: "http://example.com/callback",
		ExpiresAt: time.Now().Add(time.Minute),
	}))
	require.NoError(i.t, flows.Authenticated(ctx, flowID, userID, idp.Digest(code), nil))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/complete?code="+code, nil)
	req.Header.Set("Cookie", httpapi.SSOBindCookie+"="+bind)
	rec := httptest.NewRecorder()
	i.handler.ServeHTTP(rec, req)
	require.Equal(i.t, http.StatusFound, rec.Code, rec.Body.String())
	for _, c := range rec.Result().Cookies() {
		if c.Name == httpapi.SessionCookie && c.Value != "" {
			return c.Value
		}
	}
	i.t.Fatal("the SSO sign-in set no session cookie")
	return ""
}

// auditText is every audit event, each row as JSON text.
func (i *install) auditText() string {
	i.t.Helper()
	rows, err := i.db.Query(i.t.Context(), `SELECT row_to_json(a)::text FROM audit_events a`)
	require.NoError(i.t, err)
	defer rows.Close()
	var all strings.Builder
	for rows.Next() {
		var row string
		require.NoError(i.t, rows.Scan(&row))
		all.WriteString(row)
		all.WriteByte('\n')
	}
	require.NoError(i.t, rows.Err())
	return all.String()
}

func signedIn(i *install, cookie string) bool {
	return i.do(&session{cookie: cookie}, http.MethodGet, "/me", nil).Code == http.StatusOK
}

// TestR063_ASessionCookieIsNeverInTheAuditLog asserts that a session's cookie
// is a secret the audit log does not hold: sign-in by password and through a
// provider, sign-out and revocation record the session's public ID, and the
// cookie's secret appears in no event (R-063, R-194).
func TestR063_ASessionCookieIsNeverInTheAuditLog(t *testing.T) {
	i := newInstall(t)
	byPassword := i.admin()
	viaSSO := i.ssoSignIn(i.AdminID)
	signedOut, _ := i.signIn(bootstrap.AdminUsername, i.adminPassword)
	require.True(t, signedIn(i, signedOut.cookie))
	out := i.do(signedOut, http.MethodDelete, "/sessions", nil)
	require.Equal(t, http.StatusNoContent, out.Code, out.String())
	require.False(t, signedIn(i, signedOut.cookie), "a signed-out session no longer signs anyone in")

	log := i.auditText()
	for _, cookie := range []string{byPassword.cookie, viaSSO, signedOut.cookie} {
		sessionID, secret, ok := strings.Cut(cookie, ".")
		require.True(t, ok, "a session cookie is ses_<id>.<secret>: %q", cookie)
		require.True(t, id.Is(id.Session, sessionID))
		require.GreaterOrEqual(t, len(secret), 43, "the secret is 256 bits")
		require.Contains(t, log, sessionID, "the audit log names the session by its public ID")
		require.NotContains(t, log, secret, "the cookie's secret is never audited")
	}
	require.Contains(t, log, `"session.revoke"`)

	var stored string
	sessionID, secret, _ := strings.Cut(byPassword.cookie, ".")
	require.NoError(t, i.db.QueryRow(t.Context(),
		`SELECT secret_hash FROM sessions WHERE id = $1`, sessionID).Scan(&stored))
	require.True(t, strings.HasPrefix(stored, "hmac-sha256:"), "only a keyed digest is stored")
	require.NotContains(t, stored, secret)
}

// TestR063_ACookieBuiltFromAnAuditedIDIsRefused asserts that the public ID a
// session is audited under signs nobody in, alone or with a wrong or tampered
// secret, and that signing out with such a cookie ends nothing (R-063).
func TestR063_ACookieBuiltFromAnAuditedIDIsRefused(t *testing.T) {
	i := newInstall(t)
	victim := i.admin()
	require.True(t, signedIn(i, victim.cookie))
	sessionID, secret, ok := strings.Cut(victim.cookie, ".")
	require.True(t, ok)
	require.Contains(t, i.auditText(), sessionID)

	last := secret[len(secret)-1]
	flipped := byte('A')
	if last == 'A' {
		flipped = 'B'
	}
	other := i.user("mallory")
	_, otherSecret, _ := strings.Cut(other.cookie, ".")
	for name, forged := range map[string]string{
		"the bare ID":                 sessionID,
		"the ID with a dot":           sessionID + ".",
		"a tampered secret":           sessionID + "." + secret[:len(secret)-1] + string(flipped),
		"another session's secret":    sessionID + "." + otherSecret,
		"the secret without its ID":   secret,
		"the secret under another ID": id.New(id.Session) + "." + secret,
	} {
		require.False(t, signedIn(i, forged), "%s must not sign anyone in", name)
		gone := i.do(&session{cookie: forged}, http.MethodDelete, "/sessions", nil)
		require.Equal(t, http.StatusNoContent, gone.Code, gone.String())
	}
	require.True(t, signedIn(i, victim.cookie), "signing out with a forged cookie ends nobody's session")
	require.True(t, signedIn(i, other.cookie))
}
