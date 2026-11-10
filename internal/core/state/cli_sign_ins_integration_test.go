//go:build integration

package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// TestR406_ACLISignInIsTakenOnceAndExpiredOnesAreRemoved asserts R-406's
// storage: an approval is taken by its code's digest exactly once, the
// schema refuses a challenge that is not an S256 digest, and the retention
// job removes approvals nobody collected.
func TestR406_ACLISignInIsTakenOnceAndExpiredOnesAreRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewCLISignIns(db)
	ada := seedUser(t, db, "ada")
	now := time.Now().UTC()
	challenge := strings.Repeat("a", 43)

	live := state.CLISignIn{CodeHash: "live", UserID: ada.ID, ApprovedBy: ada.ID,
		CodeChallenge: challenge, TokenName: "laptop CLI", ExpiresAt: now.Add(5 * time.Minute)}
	require.NoError(t, store.Create(ctx, live))

	got, found, err := store.Take(ctx, "live")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ada.ID, got.UserID)
	require.Equal(t, challenge, got.CodeChallenge)
	require.Equal(t, "laptop CLI", got.TokenName)
	require.Equal(t, state.TokenDelegated, got.TokenKind, "delegated unless asked otherwise")

	_, found, err = store.Take(ctx, "live")
	require.NoError(t, err)
	require.False(t, found, "taken once")

	bad := live
	bad.CodeHash, bad.CodeChallenge = "bad", "not-a-digest"
	require.Error(t, store.Create(ctx, bad), "the schema refuses a challenge that is not S256")

	stale := live
	stale.CodeHash, stale.ExpiresAt = "stale", now.Add(-time.Minute)
	require.NoError(t, store.Create(ctx, stale))
	kept := live
	kept.CodeHash = "kept"
	require.NoError(t, store.Create(ctx, kept))

	removed, err := state.NewRetention(db).SSOFlows(ctx, now.Add(-24*time.Hour), now, 100)
	require.NoError(t, err)
	require.GreaterOrEqual(t, removed, int64(1))

	_, found, err = store.Take(ctx, "stale")
	require.NoError(t, err)
	require.False(t, found, "expired and never collected: removed")
	_, found, err = store.Take(ctx, "kept")
	require.NoError(t, err)
	require.True(t, found, "still in its 5 minutes: kept")
}

// TestR060_ACLISignInKeepsTheKindOfTokenItMakes asserts the storage behind
// pando token create --service (issue #396): an approval for a service token
// says so when it is taken, and the schema refuses any other kind.
func TestR060_ACLISignInKeepsTheKindOfTokenItMakes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewCLISignIns(db)
	ada := seedUser(t, db, "ada")
	approval := state.CLISignIn{CodeHash: "service", UserID: ada.ID, ApprovedBy: ada.ID,
		CodeChallenge: strings.Repeat("b", 43), TokenName: "nightly agent",
		TokenKind: state.TokenAccount, ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}
	require.NoError(t, store.Create(ctx, approval))

	got, found, err := store.Take(ctx, "service")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, state.TokenAccount, got.TokenKind)

	approval.CodeHash, approval.TokenKind = "admin", "admin"
	require.Error(t, store.Create(ctx, approval), "the schema refuses an unknown kind")
}
