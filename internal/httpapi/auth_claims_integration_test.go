//go:build integration

package httpapi_test

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/httpapi"
)

// TestR054_ASignedInPrincipalCarriesEmailAndName asserts that somebody signed
// in with a session reaches the proxy with their email and display name, so
// the assertion an app receives carries the email and name claims R-054 lists
// and the X-Pando-Email convenience header is set (R-053). They were left
// empty, and every app saw a subject with no email or name.
func TestR054_ASignedInPrincipalCarriesEmailAndName(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	ctx := t.Context()

	dana, err := i.Users.Create(ctx, state.LocalAdapterID, "dana", "dana@corp.example", "Dana Scully", "", false)
	require.NoError(t, err)
	sess, err := i.Sessions.Create(ctx, dana.ID, state.LocalAdapterID, 3600e9, "test", "")
	require.NoError(t, err)

	authenticator := &httpapi.Authenticator{
		Sessions: i.Sessions, Tokens: i.Tokens, Users: i.Users, Groups: state.NewAuthzStore(i.db),
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", httpapi.SessionCookie+"="+sess.ID)
	p, err := authenticator.Authenticate(r)
	require.NoError(t, err)

	require.Equal(t, authz.KindUser, p.Kind)
	require.Equal(t, dana.ID, p.UserID, "the subject is the stable ID, never the email")
	require.Equal(t, "dana@corp.example", p.Email)
	require.Equal(t, "Dana Scully", p.DisplayName)
}
