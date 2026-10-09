//go:build integration

package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// TestR079_DataFactsAnswerWhatTheSeparateReadsAnswered asserts that folding
// the proxy's reads into one query (issue #93) changed how many round trips a
// request makes and nothing else: for every kind of caller and every way an
// app can be shared, DataFacts says what IsOwner, HasDataGrant,
// AnonymousAccess and PasscodeUnlocked say. Group membership is still read
// live (R-079): removing it changes the answer at once.
func TestR079_DataFactsAnswerWhatTheSeparateReadsAnswered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAuthzStore(db)
	grants := state.NewGrants(db)

	alice := seedUser(t, db, "alice") // owns every app
	bob := seedUser(t, db, "bob")     // a direct data grant on private
	carol := seedUser(t, db, "carol") // in a group with a data grant on private
	dave := seedUser(t, db, "dave")   // nothing

	private := seedApp(t, db, alice.ID)
	public := seedApp(t, db, alice.ID)
	locked := seedApp(t, db, alice.ID)

	grant(t, db, private, "data", "user", bob.ID, nil)
	groupID := id.New(id.Group)
	_, err := db.Exec(ctx, `INSERT INTO groups (id, name) VALUES ($1, 'engineering')`, groupID)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, groupID, carol.ID)
	require.NoError(t, err)
	grant(t, db, private, "data", "group", groupID, nil)
	tokenID := id.New(id.Token)
	grant(t, db, private, "data", "token", tokenID, nil)

	grant(t, db, public, "data", "anonymous", nil, nil)
	grant(t, db, locked, "data", "anonymous", nil, nil)
	var lockedGrant string
	require.NoError(t, db.QueryRow(ctx,
		`SELECT id FROM grants WHERE app_id = $1 AND principal_kind = 'anonymous'`, locked).Scan(&lockedGrant))
	require.NoError(t, grants.SetPasscode(ctx, locked, lockedGrant, "passcode-digest"))
	unlock, _, err := grants.Unlock(ctx, locked, lockedGrant)
	require.NoError(t, err)

	user := func(u state.User) authz.Principal {
		return authz.Principal{Kind: authz.KindUser, ID: u.ID, UserID: u.ID, Status: "active"}
	}
	callers := map[string]authz.Principal{
		"owner":         user(alice),
		"direct grant":  user(bob),
		"group grant":   user(carol),
		"no grant":      user(dave),
		"account token": {Kind: authz.KindToken, ID: tokenID, TokenID: tokenID},
		"anonymous":     authz.Anonymous(),
	}
	apps := map[string]string{"private": private, "public": public, "locked": locked}
	tokens := map[string]string{"no unlock": "", "the unlock": unlock, "another unlock": "not-the-unlock"}

	separate := func(appID string, p authz.Principal, token string) authz.DataFacts {
		var f authz.DataFacts
		if p.UserID != "" {
			f.Owner, err = store.IsOwner(ctx, appID, p.UserID)
			require.NoError(t, err)
		}
		f.Grant, err = store.HasDataGrant(ctx, appID, p)
		require.NoError(t, err)
		f.Anonymous, f.Passcode, err = store.AnonymousAccess(ctx, appID)
		require.NoError(t, err)
		if token != "" {
			f.Unlocked, err = store.PasscodeUnlocked(ctx, appID, token)
			require.NoError(t, err)
		}
		return f
	}

	for appName, appID := range apps {
		for callerName, p := range callers {
			for tokenName, token := range tokens {
				got, err := store.DataFacts(ctx, appID, p, token)
				require.NoError(t, err)
				require.Equal(t, separate(appID, p, token), got, "%s app, %s, %s", appName, callerName, tokenName)
			}
		}
	}

	_, err = db.Exec(ctx, `DELETE FROM group_members WHERE user_id = $1`, carol.ID)
	require.NoError(t, err)
	got, err := store.DataFacts(ctx, private, user(carol), "")
	require.NoError(t, err)
	require.False(t, got.Grant, "membership is read live, so removing it revokes access")
}

// TestR079_ASessionIsReadWithItsAccountAndGroupsInOneQuery asserts the other
// fold (issue #93): ActiveWithUser answers what Sessions.Active, Users.ByID
// and GroupsForUser answered, groups read live, and nothing for a session
// that is revoked.
func TestR079_ASessionIsReadWithItsAccountAndGroupsInOneQuery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	sessions := state.NewSessions(db)
	users := state.NewUsers(db)
	store := state.NewAuthzStore(db)

	carol := seedUser(t, db, "carol")
	groupID := id.New(id.Group)
	_, err := db.Exec(ctx, `INSERT INTO groups (id, name) VALUES ($1, 'engineering')`, groupID)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, groupID, carol.ID)
	require.NoError(t, err)
	sess, err := sessions.Create(ctx, carol.ID, carol.AdapterID, time.Hour, "test", "")
	require.NoError(t, err)

	wantSess, found, err := sessions.Active(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, found)
	wantUser, _, err := users.ByID(ctx, carol.ID)
	require.NoError(t, err)
	wantGroups, err := store.GroupsForUser(ctx, carol.ID)
	require.NoError(t, err)

	gotSess, gotUser, gotGroups, found, err := sessions.ActiveWithUser(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, wantSess.ID, gotSess.ID)
	require.Equal(t, wantSess.UserID, gotSess.UserID)
	require.WithinDuration(t, wantSess.ExpiresAt, gotSess.ExpiresAt, time.Millisecond)
	require.Equal(t, wantUser, gotUser)
	require.ElementsMatch(t, wantGroups, gotGroups)

	require.NoError(t, sessions.Revoke(ctx, sess.ID))
	_, _, _, found, err = sessions.ActiveWithUser(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, found, "a revoked session is no session")
}
