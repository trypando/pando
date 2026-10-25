//go:build integration

package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// TestR413_AnMCPClientsSignInIsTakenOnceAndUnusedClientsAreRemoved asserts
// R-413's storage: a client is read back as registered, an approval is taken
// by its code's digest exactly once and marks its client used, and the
// retention job removes approvals nobody collected and clients nobody approved
// within a day.
func TestR413_AnMCPClientsSignInIsTakenOnceAndUnusedClientsAreRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	clients, codes := state.NewOAuthClients(db), state.NewOAuthCodes(db)
	ada := seedUser(t, db, "ada")
	now := time.Now().UTC()

	used := state.OAuthClient{ID: id.New(id.OAuthClient), Name: "Claude", RedirectURIs: []string{"https://claude.ai/cb"}}
	require.NoError(t, clients.Create(ctx, used))
	got, found, err := clients.ByID(ctx, used.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, used.RedirectURIs, got.RedirectURIs)
	require.Nil(t, got.UsedAt)
	_, found, err = clients.ByID(ctx, "oac_nobody")
	require.NoError(t, err)
	require.False(t, found)

	require.Error(t, clients.Create(ctx, state.OAuthClient{ID: "tok_wrong", Name: "x", RedirectURIs: []string{"https://a/b"}}),
		"the schema refuses an ID that is not a client's")

	live := state.OAuthCode{CodeHash: "live", ClientID: used.ID, UserID: ada.ID, ApprovedBy: ada.ID,
		CodeChallenge: strings.Repeat("a", 43), RedirectURI: "https://claude.ai/cb", ExpiresAt: now.Add(5 * time.Minute)}
	require.NoError(t, codes.Create(ctx, live))
	got, _, err = clients.ByID(ctx, used.ID)
	require.NoError(t, err)
	require.NotNil(t, got.UsedAt, "approving marks the client used")

	taken, found, err := codes.Take(ctx, "live")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "https://claude.ai/cb", taken.RedirectURI)
	_, found, err = codes.Take(ctx, "live")
	require.NoError(t, err)
	require.False(t, found, "taken once")

	stale := live
	stale.CodeHash, stale.ExpiresAt = "stale", now.Add(-time.Minute)
	require.NoError(t, codes.Create(ctx, stale))

	abandoned := state.OAuthClient{ID: id.New(id.OAuthClient), Name: "Nobody", RedirectURIs: []string{"https://x.example/cb"}}
	require.NoError(t, clients.Create(ctx, abandoned))
	fresh := state.OAuthClient{ID: id.New(id.OAuthClient), Name: "Just now", RedirectURIs: []string{"https://y.example/cb"}}
	require.NoError(t, clients.Create(ctx, fresh))

	// A day and a minute later.
	later := now.Add(state.UnusedOAuthClientLifetime + time.Minute)
	_, err = db.Exec(ctx, `UPDATE oauth_clients SET created_at = $2 WHERE id = $1`, fresh.ID, later)
	require.NoError(t, err)
	removed, err := state.NewRetention(db).SSOFlows(ctx, later.Add(-24*time.Hour), later, 100)
	require.NoError(t, err)
	require.GreaterOrEqual(t, removed, int64(2))

	_, found, err = codes.Take(ctx, "stale")
	require.NoError(t, err)
	require.False(t, found, "expired and never collected: removed")
	_, found, err = clients.ByID(ctx, abandoned.ID)
	require.NoError(t, err)
	require.False(t, found, "registered a day ago and never approved: removed")
	_, found, err = clients.ByID(ctx, used.ID)
	require.NoError(t, err)
	require.True(t, found, "approved once: kept")
	_, found, err = clients.ByID(ctx, fresh.ID)
	require.NoError(t, err)
	require.True(t, found, "registered within the day: kept")
}
