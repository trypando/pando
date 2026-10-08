//go:build integration

package state_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// Keyset pagination of the lists that grow with the organization (issue #72),
// and the launcher query rewritten as a union of indexed lookups.

func grant(t *testing.T, db *state.DB, appID, plane, kind string, principal any, role any) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'test')`, id.New(id.Grant), appID, plane, kind, principal, role)
	require.NoError(t, err)
}

func userPrincipal(u state.User) authz.Principal {
	return authz.Principal{Kind: authz.KindUser, ID: u.ID, UserID: u.ID, Status: "active"}
}

// TestR264_TheLauncherListsEveryAppItsUserCanOpenOnce asserts R-264 over the
// launcher's union query: owned, a direct data grant, a group's data grant and
// an anonymous grant each put an app on the launcher, exactly once however
// many of them apply; a control grant, a deleted app and somebody else's
// grant do not.
func TestR264_TheLauncherListsEveryAppItsUserCanOpenOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	bob := seedUser(t, db, "bob")
	apps := state.NewApps(db)

	owned := seedApp(t, db, bob.ID)
	direct := seedApp(t, db, alice.ID)
	grant(t, db, direct, "data", "user", bob.ID, nil)
	viaGroup := seedApp(t, db, alice.ID)
	team, err := state.NewGroups(db).Create(ctx, "team")
	require.NoError(t, err)
	require.NoError(t, state.NewGroups(db).AddMember(ctx, team.ID, bob.ID))
	grant(t, db, viaGroup, "data", "group", team.ID, nil)
	grant(t, db, viaGroup, "data", "user", bob.ID, nil) // and directly: still once
	open := seedApp(t, db, alice.ID)
	grant(t, db, open, "data", "anonymous", nil, nil)

	controlOnly := seedApp(t, db, alice.ID)
	grant(t, db, controlOnly, "control", "user", bob.ID, authz.RoleOperator)
	deleted := seedApp(t, db, alice.ID)
	grant(t, db, deleted, "data", "user", bob.ID, nil)
	_, err = db.Exec(ctx, `UPDATE apps SET deleted_at = now() WHERE id = $1`, deleted)
	require.NoError(t, err)
	_ = seedApp(t, db, alice.ID) // alice's alone

	usable, _, err := apps.ListForUse(ctx, userPrincipal(bob), state.Page{}, false)
	require.NoError(t, err)
	var got []string
	for _, a := range usable {
		got = append(got, a.ID)
	}
	require.ElementsMatch(t, []string{owned, direct, viaGroup, open}, got)
}

// TestTheAccountsListPagesWithoutGapsOrRepeats walks GET /users's pages.
func TestTheAccountsListPagesWithoutGapsOrRepeats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	var want []string
	for i := 0; i < 7; i++ {
		want = append(want, seedUser(t, db, fmt.Sprintf("person%d", i)).ID)
	}
	users := state.NewUsers(db)

	var got []string
	cursor := ""
	pages := 0
	for {
		page, next, total, err := users.ListPage(ctx, state.Page{Limit: 3, Cursor: cursor})
		require.NoError(t, err)
		require.Equal(t, 7, total)
		require.LessOrEqual(t, len(page), 3)
		for _, u := range page {
			got = append(got, u.ID)
		}
		pages++
		if next == "" {
			break
		}
		cursor = next
	}
	require.Equal(t, 3, pages)
	require.ElementsMatch(t, want, got)
	for i := 1; i < len(got); i++ {
		require.Greater(t, got[i-1], got[i], "newest first")
	}

	found, next, total, err := users.ListPage(ctx, state.Page{Query: "PERSON3"})
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, 1, total)
	require.Empty(t, next)

	named, _, total, err := users.ListPage(ctx, state.Page{IDs: []string{want[1], want[4]}})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, named, 2)

	_, _, _, err = users.ListPage(ctx, state.Page{Cursor: "not a cursor"})
	require.ErrorIs(t, err, state.ErrBadCursor)
}

// TestTheGroupsListPagesAndCountsRatherThanListingMembers walks GET /groups's
// pages, its member count and its member filter.
func TestTheGroupsListPagesAndCountsRatherThanListingMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	bob := seedUser(t, db, "bob")
	groups := state.NewGroups(db)
	var names []string
	for _, n := range []string{"Delta", "alpha", "Charlie", "bravo", "echo"} {
		g, err := groups.Create(ctx, n)
		require.NoError(t, err)
		names = append(names, n)
		if n == "bravo" || n == "echo" {
			require.NoError(t, groups.AddMember(ctx, g.ID, alice.ID))
		}
		if n == "bravo" {
			require.NoError(t, groups.AddMember(ctx, g.ID, bob.ID))
		}
	}

	var got []string
	counts := map[string]int{}
	cursor := ""
	for {
		page, next, total, err := groups.ListPage(ctx, state.Page{Limit: 2, Cursor: cursor}, state.GroupFilter{})
		require.NoError(t, err)
		require.Equal(t, 5, total)
		for _, g := range page {
			got = append(got, g.Name)
			require.Empty(t, g.Members, "a page counts members, it does not list them")
			require.NotNil(t, g.MemberCount)
			counts[g.Name] = *g.MemberCount
		}
		if next == "" {
			break
		}
		cursor = next
	}
	require.Equal(t, []string{"alpha", "bravo", "Charlie", "Delta", "echo"}, got, "by name, ignoring case")
	require.Equal(t, 2, counts["bravo"])
	require.Equal(t, 1, counts["echo"])
	require.Equal(t, 0, counts["alpha"])

	mine, _, total, err := groups.ListPage(ctx, state.Page{}, state.GroupFilter{Member: alice.ID})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, mine, 2)
}

// TestTheAppsListPagesOncePerAppAndNarrowsToIDs walks GET /apps's pages for an
// administrator and for somebody granted apps two ways, and its id filter.
func TestTheAppsListPagesOncePerAppAndNarrowsToIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	bob := seedUser(t, db, "bob")
	apps := state.NewApps(db)
	team, err := state.NewGroups(db).Create(ctx, "team")
	require.NoError(t, err)
	require.NoError(t, state.NewGroups(db).AddMember(ctx, team.ID, bob.ID))

	var all, bobs []string
	for i := 0; i < 5; i++ {
		appID := seedApp(t, db, alice.ID)
		all = append(all, appID)
		if i%2 == 0 {
			grant(t, db, appID, "control", "user", bob.ID, authz.RoleViewer)
			grant(t, db, appID, "control", "group", team.ID, authz.RoleOperator)
			bobs = append(bobs, appID)
		}
	}

	walk := func(list func(state.Page) ([]state.App, string, int, error)) ([]string, int) {
		var got []string
		cursor := ""
		var total int
		for {
			page, next, n, err := list(state.Page{Limit: 2, Cursor: cursor})
			require.NoError(t, err)
			total = n
			for _, a := range page {
				got = append(got, a.ID)
			}
			if next == "" {
				return got, total
			}
			cursor = next
		}
	}

	got, total := walk(func(p state.Page) ([]state.App, string, int, error) { return apps.ListAllPage(ctx, p) })
	require.Equal(t, 5, total)
	require.ElementsMatch(t, all, got)
	require.Len(t, got, 5, "no app twice")

	got, total = walk(func(p state.Page) ([]state.App, string, int, error) {
		return apps.ListForPrincipalPage(ctx, userPrincipal(bob), p)
	})
	require.Equal(t, 3, total)
	require.ElementsMatch(t, bobs, got)
	require.Len(t, got, 3, "granted twice, listed once")

	narrowed, _, total, err := apps.ListForPrincipalPage(ctx, userPrincipal(bob), state.Page{IDs: []string{all[0], all[1]}})
	require.NoError(t, err)
	require.Equal(t, 1, total, "narrowing never shows an app the grants do not")
	require.Len(t, narrowed, 1)
	require.Equal(t, all[0], narrowed[0].ID)
}
