package mcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR405_TwoFactorAsTools asserts R-405 with R-261: an agent reads your own
// two-factor status from GET /me and an administrator's reset goes to the
// same endpoint the console's does. Setting one up has no tool.
func TestR405_TwoFactorAsTools(t *testing.T) {
	srv, s := newSession()
	s.run(t, srv, call(1, "pando_get_two_factor", `{}`))
	require.Len(t, s.calls, 1)
	require.Equal(t, "GET", s.calls[0].method)
	require.Equal(t, "/me", s.calls[0].path)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_reset_user_two_factor", `{"user_id":"usr_1"}`))
	require.Len(t, s.calls, 1)
	require.Equal(t, "DELETE", s.calls[0].method)
	require.Equal(t, "/users/usr_1/two-factor", s.calls[0].path)

	srv, s = newSession()
	s.run(t, srv, call(1, "pando_reset_user_two_factor", `{}`))
	require.Empty(t, s.calls, "no user_id, no call")
}
