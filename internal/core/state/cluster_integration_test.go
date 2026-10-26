//go:build integration

package state_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/tokenkey"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

func clusterStore(t *testing.T) (*state.Cluster, *state.DB) {
	t.Helper()
	db := connected(t)
	return state.NewCluster(db, bytes.Repeat([]byte{7}, tokenkey.Size)), db
}

// TestR415_AJoinTokenIsStoredOnlyAsAKeyedDigest asserts R-415: what the
// database holds cannot be used to join.
func TestR415_AJoinTokenIsStoredOnlyAsAKeyedDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, db := clusterStore(t)

	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	_, plaintext, _ := strings.Cut(issued.Secret.Reveal(), ".")

	var stored string
	require.NoError(t, db.QueryRow(ctx, `SELECT hash FROM join_tokens WHERE id = $1`, issued.Token.ID).Scan(&stored))
	require.True(t, strings.HasPrefix(stored, "hmac-sha256:"))
	require.NotContains(t, stored, plaintext)

	other := state.NewCluster(db, bytes.Repeat([]byte{8}, tokenkey.Size))
	_, err = other.RedeemJoinToken(ctx, issued.Secret)
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err), "a different key must not verify it")
}

// TestR415_ASingleUseJoinTokenJoinsOneHost asserts R-415: two hosts redeeming a
// single-use token at once, only one succeeds.
func TestR415_ASingleUseJoinTokenJoinsOneHost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, _ := clusterStore(t)

	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)

	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.RedeemJoinToken(ctx, issued.Secret); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, succeeded)
}

// TestR415_ARevokedExpiredOrWrongJoinTokenIsRefusedTheSameWay asserts R-415:
// every refusal reads alike, so a guess learns nothing about which part failed.
func TestR415_ARevokedExpiredOrWrongJoinTokenIsRefusedTheSameWay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, db := clusterStore(t)

	revoked, err := c.CreateJoinToken(ctx, []string{"app"}, 3, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	_, err = c.RevokeJoinToken(ctx, revoked.Token.ID)
	require.NoError(t, err)
	again, err := c.RevokeJoinToken(ctx, revoked.Token.ID)
	require.NoError(t, err, "revoking twice is not an error")
	require.NotNil(t, again.RevokedAt)

	expired, err := c.CreateJoinToken(ctx, []string{"app"}, 3, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	_, err = db.Exec(ctx, `UPDATE join_tokens SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE id = $1`, expired.Token.ID)
	require.NoError(t, err)

	live, err := c.CreateJoinToken(ctx, []string{"control", "app"}, 3, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	liveID, _, _ := strings.Cut(live.Secret.Reveal(), ".")

	var messages []string
	for _, presented := range []secret.Value{
		revoked.Secret, expired.Secret,
		secret.New(liveID + ".wrong"), secret.New("jt_nobody.x"), secret.New("nonsense"),
	} {
		_, err := c.RedeemJoinToken(ctx, presented)
		require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
		messages = append(messages, err.Error())
	}
	for _, m := range messages {
		require.Equal(t, messages[0], m)
	}

	got, err := c.RedeemJoinToken(ctx, live.Secret)
	require.NoError(t, err)
	require.Equal(t, 1, got.Uses)
	require.Equal(t, []string{"control", "app"}, got.Roles)

	_, err = c.RevokeJoinToken(ctx, "jt_nobody")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	list, err := c.JoinTokens(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	require.Equal(t, live.Token.ID, list[0].ID, "newest first")
}

// TestR414_HostsAreRecordedListedAndRemoved asserts R-414's membership store,
// and that a host's roles are only control and app.
func TestR414_HostsAreRecordedListedAndRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, db := clusterStore(t)

	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	h, err := c.AddHost(ctx, state.Host{
		Name: "app-1", Roles: []string{"app"}, Address: "10.0.0.2:7443",
		CertSHA256: strings.Repeat("a", 64), Platform: "linux/amd64", Version: "0.16.0", JoinedWith: issued.Token.ID,
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(h.ID, "host_"))

	_, err = c.AddHost(ctx, state.Host{Name: "app-1", Roles: []string{"app"}, Address: "x", CertSHA256: strings.Repeat("b", 64)})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))

	_, err = c.AddHost(ctx, state.Host{Name: "sched", Roles: []string{"scheduler"}, Address: "x", CertSHA256: strings.Repeat("c", 64)})
	require.Error(t, err, "a role other than control or app is refused by the database")

	require.NoError(t, c.TouchHost(ctx, h.ID))
	got, err := c.Host(ctx, h.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastSeenAt)
	require.Equal(t, issued.Token.ID, got.JoinedWith)

	list, err := c.Hosts(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	_, err = c.RemoveHost(ctx, h.ID)
	require.NoError(t, err)
	_, err = c.Host(ctx, h.ID)
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	_, err = c.RemoveHost(ctx, h.ID)
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	var n int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM roles WHERE id = 'role_administrator' AND 'install.cluster.manage' = ANY(verbs)`).Scan(&n))
	require.Equal(t, 1, n, "the Administrator holds install.cluster.manage")
}
