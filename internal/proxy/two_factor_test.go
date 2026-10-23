package proxy_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/proxy"
)

// pendingAuth is a session whose account must still set up two-factor
// authentication (R-405).
type pendingAuth struct{}

func (pendingAuth) Authenticate(*http.Request) (authz.Principal, error) {
	p := activeUser("usr_alice")
	p.TwoFactorSetupPending = true
	return p, nil
}

// TestR405_ASessionThatMustSetUpTwoFactorIsAnonymousToApps asserts R-405 in
// front of an app: a session that has given only its password, where policy
// requires a second factor, reaches an app as nobody. A private app sends it to
// sign in, where it sets one up; a public one serves it as anonymous.
func TestR405_ASessionThatMustSetUpTwoFactorIsAnonymousToApps(t *testing.T) {
	front, _, _, _ := harnessWith(t, pendingAuth{}, func(s *store) { s.owner[appID] = "usr_alice" },
		func(url string) proxy.Upstreams { return fixedUpstream{addr: url} })
	resp := send(t, front, true)
	require.NotEqual(t, http.StatusOK, resp.StatusCode, "the owner's own app is not theirs to use yet")
	require.Contains(t, resp.Location, "/.pando", "sent to sign in, where the setup is")

	public, _, _, got := harnessWith(t, pendingAuth{}, func(s *store) {
		s.owner[appID] = "usr_alice"
		s.anonymous[appID] = true
	}, func(url string) proxy.Upstreams { return fixedUpstream{addr: url} })
	resp = send(t, public, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "anonymous", got.header.Get(proxy.HeaderUser))
}

// mustChangeAuth is a session whose password somebody else set (R-046).
type mustChangeAuth struct{}

func (mustChangeAuth) Authenticate(*http.Request) (authz.Principal, error) {
	p := activeUser("usr_alice")
	p.PasswordChangePending = true
	return p, nil
}

// TestR046_ASessionThatMustChangeItsPasswordIsAnonymousToApps asserts R-046
// in front of an app: until the password is changed the session reaches an
// app as nobody, its owner's own included.
func TestR046_ASessionThatMustChangeItsPasswordIsAnonymousToApps(t *testing.T) {
	front, _, _, _ := harnessWith(t, mustChangeAuth{}, func(s *store) { s.owner[appID] = "usr_alice" },
		func(url string) proxy.Upstreams { return fixedUpstream{addr: url} })
	resp := send(t, front, true)
	require.NotEqual(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Location, "/.pando", "sent to sign in, where the change is")
}
