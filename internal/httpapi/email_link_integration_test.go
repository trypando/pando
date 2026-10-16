//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// TestR045_AnEmailYouSetYourselfIsNotOneAProviderCanLinkTo asserts R-045's
// "an email address that exactly one active account has" against an account
// holder who sets their own.
//
// Anyone may change their own email, so an address an account's holder typed
// in vouches for nothing: dana could set a colleague's, and a provider that
// links by email would hand dana that colleague's first sign-in — or, with two
// accounts holding it, refuse the colleague. So a self-set email is never
// matched for linking, and no two accounts in use may share one.
func TestR045_AnEmailYouSetYourselfIsNotOneAProviderCanLinkTo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i := newInstall(t)
	identities := state.NewIdentities(i.db)
	dana, erin := i.user("dana"), i.user("erin")
	idOf := func(s *session) string {
		var me struct {
			UserID string `json:"user_id"`
		}
		i.do(s, http.MethodGet, "/me", nil).JSON(t, &me)
		return me.UserID
	}
	danaID, erinID := idOf(dana), idOf(erin)

	got := i.do(dana, http.MethodPatch, "/users/"+danaID, map[string]any{"email": "carol@example.com"})
	require.Equal(t, http.StatusNoContent, got.Code, got.String())
	_, linkable, err := identities.ByVerifiedEmail(ctx, "carol@example.com")
	require.NoError(t, err)
	require.False(t, linkable, "an email dana set for herself is not one a provider may link carol to")

	taken := i.do(erin, http.MethodPatch, "/users/"+erinID, map[string]any{"email": "CAROL@example.com"})
	require.Equal(t, http.StatusBadRequest, taken.Code, taken.String())
	require.Contains(t, taken.String(), "Another Pando account already has this email address")

	// An administrator's word is the installation's, and links.
	got = i.do(i.admin(), http.MethodPatch, "/users/"+erinID, map[string]any{"email": "erin@example.com"})
	require.Equal(t, http.StatusNoContent, got.Code, got.String())
	linked, linkable, err := identities.ByVerifiedEmail(ctx, "erin@example.com")
	require.NoError(t, err)
	require.True(t, linkable)
	require.Equal(t, erinID, linked.ID)

	// Setting it again yourself takes that back.
	got = i.do(erin, http.MethodPatch, "/users/"+erinID, map[string]any{"email": "erin@example.com"})
	require.Equal(t, http.StatusNoContent, got.Code, got.String())
	_, linkable, err = identities.ByVerifiedEmail(ctx, "erin@example.com")
	require.NoError(t, err)
	require.False(t, linkable)
}
