//go:build integration

package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// TestR063_ASessionStoreNeedsTheWholeKey asserts that the session store,
// like the token store, refuses a key of the wrong size rather than storing
// digests under it (R-063).
func TestR063_ASessionStoreNeedsTheWholeKey(t *testing.T) {
	t.Parallel()
	db := connected(t)
	require.Panics(t, func() { state.NewSessions(db, []byte("short")) })
}

// TestR063_AMalformedSessionCookieFindsNothing asserts that a cookie not
// shaped ses_<id>.<secret> is no session, without reading the table (R-063).
func TestR063_AMalformedSessionCookieFindsNothing(t *testing.T) {
	t.Parallel()
	db := connected(t)
	sessions := state.NewSessions(db, testTokenKey)
	db.Close() // a malformed cookie must not need the database at all

	for _, cookie := range []string{"", "ses_x", id.New(id.Session), id.New(id.Session) + ".", "tok_abc.secret", ".secret"} {
		_, ok, err := sessions.Active(context.Background(), cookie)
		require.NoError(t, err, cookie)
		require.False(t, ok, cookie)
		_, _, _, ok, err = sessions.ActiveWithUser(context.Background(), cookie)
		require.NoError(t, err, cookie)
		require.False(t, ok, cookie)
	}
}

// TestR105_ASessionTheDatabaseCannotReachIsAServerFault asserts that a
// session read or write the database refuses is an internal error, never a
// signed-out answer that looks like a wrong cookie.
func TestR105_ASessionTheDatabaseCannotReachIsAServerFault(t *testing.T) {
	t.Parallel()
	db := connected(t)
	sessions := state.NewSessions(db, testTokenKey)
	db.Close()

	ctx := context.Background()
	cookie := id.New(id.Session) + ".c2VjcmV0"
	_, err := sessions.Create(ctx, "usr_x", state.LocalAdapterID, time.Hour, "", "")
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	_, _, err = sessions.Active(ctx, cookie)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	_, _, _, _, err = sessions.ActiveWithUser(ctx, cookie)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Equal(t, errs.Internal, errs.CodeOf(sessions.Revoke(ctx, "ses_x")))
	require.Equal(t, errs.Internal, errs.CodeOf(sessions.RevokeOthersForUser(ctx, "usr_x", "ses_x")))
	require.Equal(t, errs.Internal, errs.CodeOf(sessions.RevokeAllForUser(ctx, "usr_x")))
}
