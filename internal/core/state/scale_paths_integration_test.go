//go:build integration

package state_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/id"
)

// Paths that cost what the answer costs, not what the install holds
// (issue #72, O-53). Each test fails against the query it replaced.

func specFor(appID string, image int, autoDeploy bool) *spec.AppSpec {
	return &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		AppID:         appID,
		Source:        spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"},
		Build:         spec.Build{Strategy: spec.BuildPrebuilt},
		Workloads:     []spec.Workload{{Name: "web", Image: fmt.Sprintf("example/app:%d", image), Primary: true, Exposed: true}},
		Routing:       spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
		Runtime:       spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
		Deploy:        spec.Deploy{Strategy: spec.DeployRecreate, AutoDeploy: spec.AutoDeploy{Enabled: autoDeploy}},
		Retention:     spec.Retention{SpecRevisions: 10},
	}
}

func installAdmin(t *testing.T, db *state.DB, kind, principalID string) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		INSERT INTO grants (id, app_id, plane, role_scope, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, NULL, 'control', 'install', $2, $3, $4, 'system')`,
		id.New(id.Grant), kind, principalID, authz.RoleAdministrator)
	require.NoError(t, err)
}

// TestO53_AListTotalIsExactUpToTheCapThenALowerBound asserts O-53: past
// TotalCap matches the count stops, rather than reading every match, and a
// narrower search below the cap is still counted exactly.
func TestO53_AListTotalIsExactUpToTheCapThenALowerBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	_, err := db.Exec(ctx, `
		INSERT INTO groups (id, name)
		SELECT 'grp_bulk' || lpad(i::text, 6, '0'), 'bulk ' || i FROM generate_series(1, $1) i`, state.TotalCap+5)
	require.NoError(t, err)
	groups := state.NewGroups(db)

	page, next, total, err := groups.ListPage(ctx, state.Page{Limit: 10}, state.GroupFilter{})
	require.NoError(t, err)
	require.Len(t, page, 10)
	require.NotEmpty(t, next)
	require.Equal(t, state.TotalCap+1, total, "more than the cap is reported as the cap plus one, not counted")

	_, _, total, err = groups.ListPage(ctx, state.Page{Limit: 10, Query: "bulk 1000"}, state.GroupFilter{})
	require.NoError(t, err)
	require.Equal(t, 7, total, "bulk 1000 and bulk 10000 to 10005, counted exactly")
}

// TestSubstringSearchesAreServedByTrigramIndexes asserts that the searches,
// with their leading wildcard, can be answered from an index (000055) rather
// than only by reading every account, app and group — and that the
// expressions the list queries write are the ones indexed, character for
// character, which is what an expression index needs.
//
// Whether the planner prefers the index on a given table depends on its size
// and contents, so the question is put without the alternatives: no
// sequential or plain index scan, and none of the table's partial indexes,
// any of which could stand in for a scan of every live row. What is left is
// an index that answers the pattern itself, or a failure to plan.
func TestSubstringSearchesAreServedByTrigramIndexes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, ownerURL := statetest.Connect(t)
	ownerConn, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	defer func() { _ = ownerConn.Close(ctx) }()

	for _, c := range []struct{ table, index, query string }{
		{"users", "users_search_trgm_idx", `SELECT id FROM users WHERE deleted_at IS NULL AND alias_of IS NULL
			AND ` + state.SearchUsers + ` ILIKE '%' || $1 || '%'`},
		{"apps", "apps_search_trgm_idx", `SELECT a.id FROM apps a WHERE a.deleted_at IS NULL
			AND ` + state.SearchApps + ` ILIKE '%' || $1 || '%'`},
		{"groups", "groups_name_trgm_idx", `SELECT g.id FROM groups g WHERE g.name ILIKE '%' || $1 || '%'`},
	} {
		func() {
			tx, err := ownerConn.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			rows, err := tx.Query(ctx, `
				SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
				WHERE i.indrelid = $1::regclass AND i.indpred IS NOT NULL AND c.relname <> $2`, c.table, c.index)
			require.NoError(t, err)
			var partial []string
			for rows.Next() {
				var name string
				require.NoError(t, rows.Scan(&name))
				partial = append(partial, name)
			}
			rows.Close()
			for _, name := range partial {
				_, err := tx.Exec(ctx, `DROP INDEX `+pgx.Identifier{name}.Sanitize())
				require.NoError(t, err)
			}
			for _, off := range []string{"enable_seqscan", "enable_indexscan"} {
				_, err := tx.Exec(ctx, `SET LOCAL `+off+` = off`)
				require.NoError(t, err)
			}
			plan := explain(t, tx, c.query, "ada")
			require.Contains(t, plan, c.index, "the search %q is not served by %s:\n%s", c.query, c.index, plan)
			require.Contains(t, plan, "Index Cond", plan)
		}()
	}
}

// explain is the plan Postgres chooses for query, as text.
func explain(t *testing.T, tx pgx.Tx, query string, args ...any) string {
	t.Helper()
	rows, err := tx.Query(context.Background(), `EXPLAIN `+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		lines = append(lines, line)
	}
	require.NoError(t, rows.Err())
	return strings.Join(lines, "\n")
}

// TestR079_AGroupIsReadWithoutItsMembers asserts that reading a group to show
// it, or to learn that it exists, does not load its membership, and that the
// members come a page at a time.
func TestR079_AGroupIsReadWithoutItsMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	groups := state.NewGroups(db)
	team, err := groups.Create(ctx, "team")
	require.NoError(t, err)
	var members []string
	for i := range 5 {
		u := seedUser(t, db, fmt.Sprintf("member%d", i))
		members = append(members, u.ID)
	}
	require.NoError(t, groups.SetMembers(ctx, team.ID, members))

	got, found, err := groups.ByID(ctx, team.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Nil(t, got.Members, "the group is read without its members")
	require.NotNil(t, got.MemberCount)
	require.Equal(t, 5, *got.MemberCount)

	ok, err := groups.Exists(ctx, team.ID)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = groups.Exists(ctx, "grp_missing")
	require.NoError(t, err)
	require.False(t, ok)

	var walked []string
	cursor := ""
	for {
		page, next, total, err := groups.MembersPage(ctx, team.ID, state.Page{Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		require.Equal(t, 5, total)
		require.LessOrEqual(t, len(page), 2)
		for _, u := range page {
			walked = append(walked, u.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	require.ElementsMatch(t, members, walked, "every member once, a page at a time")

	page, _, total, err := groups.MembersPage(ctx, team.ID, state.Page{IDs: []string{members[1], "usr_stranger"}})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, page, 1)
	require.Equal(t, members[1], page[0].ID, "asking about particular people answers which are members")

	page, _, _, err = groups.MembersPage(ctx, team.ID, state.Page{Query: "member3"})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, "member3", page[0].ExternalID)
}

// TestR088_ManagersAreCountedOnlyWhenAChangeCanAffectThem asserts that the
// R-088 count is skipped for a change that cannot change who manages accounts
// — an ordinary person's memberships, a group holding no administrator role —
// and taken for one that can: an administrator, directly or through a group,
// or a provider's group linked to an administrators' group.
func TestR088_ManagersAreCountedOnlyWhenAChangeCanAffectThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	groups := state.NewGroups(db)

	admin := seedUser(t, db, "admin")
	installAdmin(t, db, "user", admin.ID)
	viaGroup := seedUser(t, db, "via-group")
	ordinary := seedUser(t, db, "ordinary")

	admins, err := groups.Create(ctx, "admins")
	require.NoError(t, err)
	installAdmin(t, db, "group", admins.ID)
	require.NoError(t, groups.AddMember(ctx, admins.ID, viaGroup.ID))
	plain, err := groups.Create(ctx, "plain")
	require.NoError(t, err)

	for name, c := range map[string]struct {
		users, groups []string
		want          bool
	}{
		"an ordinary person":               {users: []string{ordinary.ID}, want: false},
		"an administrator":                 {users: []string{admin.ID}, want: true},
		"an administrator through a group": {users: []string{viaGroup.ID}, want: true},
		"a group with no role":             {groups: []string{plain.ID}, want: false},
		"the administrators' group":        {groups: []string{admins.ID}, want: true},
	} {
		got, err := state.ManagerCountNeeded(ctx, db, c.users, c.groups)
		require.NoError(t, err, name)
		require.Equal(t, c.want, got, name)
	}

	// A provider's group feeding the administrators' group can change who
	// manages, through the link.
	_, err = db.Exec(ctx, `INSERT INTO identity_adapters (id, kind, name, config) VALUES ('idp_okta', 'oidc', 'Okta', '{}')`)
	require.NoError(t, err)
	require.NoError(t, groups.SyncMemberships(ctx, "idp_okta", ordinary.ID, []string{"engineering"}))
	var synced string
	require.NoError(t, db.QueryRow(ctx, `SELECT id FROM groups WHERE adapter_id = 'idp_okta'`).Scan(&synced))
	got, err := state.ManagerCountNeeded(ctx, db, nil, []string{synced})
	require.NoError(t, err)
	require.False(t, got, "a provider's group linked to nothing")
	require.NoError(t, groups.Link(ctx, synced, admins.ID, "test"))
	got, err = state.ManagerCountNeeded(ctx, db, nil, []string{synced})
	require.NoError(t, err)
	require.True(t, got, "a provider's group linked to the administrators' group")

	// And the count itself still refuses the lockout when it is taken.
	require.NoError(t, state.NewUsers(db).SetStatus(ctx, viaGroup.ID, "suspended"))
	require.NoError(t, state.NewUsers(db).SetStatus(ctx, ordinary.ID, "suspended"))
	_, err = db.Exec(ctx, `DELETE FROM grants WHERE principal_kind = 'user' AND principal_id = $1`, admin.ID)
	require.NoError(t, err)
	require.NoError(t, groups.AddMember(ctx, admins.ID, admin.ID))
	require.Error(t, groups.RemoveMember(ctx, admins.ID, admin.ID),
		"removing the last active administrator from the administrators' group is refused")
}

// TestR152_PruningGoesAppByAppInBatchesAndKeepsWhatRollbackNeeds asserts
// R-152 under batched pruning: each statement reads a few apps and removes a
// few revisions from each, every app is reached, and a revision that was ever
// pinned or that a deploy refers to is never removed.
func TestR152_PruningGoesAppByAppInBatchesAndKeepsWhatRollbackNeeds(t *testing.T) {
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedUser(t, db, "owner")
	restore := state.SetPruneBatches(2, 2)
	defer restore()

	type made struct {
		id               string
		pinned, deployed string
		revisions        []string
	}
	var all []made
	for range 3 {
		app, err := apps.Create(ctx, "prune", id.New(id.App), owner.ID, owner.ID,
			spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
		require.NoError(t, err)
		m := made{id: app.ID}
		for i := range 15 {
			rev, err := apps.CreateRevision(ctx, app.ID, specFor(app.ID, i, false), spec.OriginEdited, owner.ID)
			require.NoError(t, err)
			m.revisions = append(m.revisions, rev.ID)
		}
		m.pinned, m.deployed = m.revisions[1], m.revisions[2]
		require.NoError(t, apps.Pin(ctx, app.ID, m.pinned, state.StateRunning, owner.ID))
		require.NoError(t, apps.Pin(ctx, app.ID, m.revisions[14], state.StateRunning, owner.ID))
		_, err = db.Exec(ctx, `
			INSERT INTO deployments (id, app_id, spec_id, trigger, status, created_by)
			VALUES ($1, $2, $3, 'manual', 'failed', 'test')`, id.New(id.Deployment), app.ID, m.deployed)
		require.NoError(t, err)
		all = append(all, m)
	}

	// Five revisions per app are outside the newest ten; two are kept for
	// R-152. Three can go, at most two per app per pass.
	pruned, err := apps.PruneSpecRevisions(ctx)
	require.NoError(t, err)
	require.Equal(t, 6, pruned, "every app is reached, two revisions from each")
	pruned, err = apps.PruneSpecRevisions(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, pruned, "the rest of each app's backlog on the next pass")
	pruned, err = apps.PruneSpecRevisions(ctx)
	require.NoError(t, err)
	require.Zero(t, pruned)

	for _, m := range all {
		left, err := apps.ListRevisions(ctx, m.id)
		require.NoError(t, err)
		var ids []string
		for _, r := range left {
			ids = append(ids, r.ID)
		}
		require.Len(t, ids, 12, "the newest ten, the ever-pinned one and the deployed one")
		require.Contains(t, ids, m.pinned, "a revision that was ever pinned is never pruned")
		require.Contains(t, ids, m.deployed, "a revision a deploy refers to is never pruned")
		require.Subset(t, ids, m.revisions[5:])
		require.NotContains(t, ids, m.revisions[0])
	}
}

// TestR141_AutoDeployIsReadFromThePinnedRevision asserts R-141 with the
// column 000056 adds: an app tracks a branch when the revision it has pinned
// says so, the column follows the pin, and nothing else can set it.
func TestR141_AutoDeployIsReadFromThePinnedRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedUser(t, db, "owner")
	app, err := apps.Create(ctx, "tracker", id.New(id.App), owner.ID, owner.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	on, err := apps.CreateRevision(ctx, app.ID, specFor(app.ID, 1, true), spec.OriginEdited, owner.ID)
	require.NoError(t, err)
	off, err := apps.CreateRevision(ctx, app.ID, specFor(app.ID, 2, false), spec.OriginEdited, owner.ID)
	require.NoError(t, err)

	tracking := func() []string {
		list, err := apps.WithAutoDeploy(ctx)
		require.NoError(t, err)
		var ids []string
		for _, a := range list {
			ids = append(ids, a.ID)
		}
		return ids
	}
	require.Empty(t, tracking(), "nothing pinned")
	require.NoError(t, apps.Pin(ctx, app.ID, on.ID, state.StateRunning, owner.ID))
	require.Equal(t, []string{app.ID}, tracking())

	_, err = db.Exec(ctx, `UPDATE apps SET auto_deploy = false WHERE id = $1`, app.ID)
	require.NoError(t, err)
	require.Equal(t, []string{app.ID}, tracking(), "the column cannot be written around the pinned revision")

	require.NoError(t, apps.Pin(ctx, app.ID, off.ID, state.StateRunning, owner.ID))
	require.Empty(t, tracking(), "moving the pin to a revision without auto-deploy stops it")
	_, err = db.Exec(ctx, `UPDATE apps SET auto_deploy = true WHERE id = $1`, app.ID)
	require.NoError(t, err)
	require.Empty(t, tracking())
}

// TestR264_TheLauncherPagesFavoritesFirstAndSaysWhatYouCanManage asserts
// R-264 over the paged launcher: favorites, then filed apps, then the rest,
// each by name; every app once across the pages; can_manage set from the
// control plane alone; and the search narrowing by name.
func TestR264_TheLauncherPagesFavoritesFirstAndSaysWhatYouCanManage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	alice := seedUser(t, db, "alice")
	bob := seedUser(t, db, "bob")

	named := func(name string) string {
		appID := id.New(id.App)
		_, err := db.Exec(ctx, `INSERT INTO apps (id, name, slug, owner_user_id, state) VALUES ($1, $2, $1, $3, 'draft')`,
			appID, name, alice.ID)
		require.NoError(t, err)
		grant(t, db, appID, "data", "anonymous", nil, nil)
		return appID
	}
	zebra, apple, mango, kiwi, fig := named("zebra"), named("apple"), named("mango"), named("kiwi"), named("fig")
	grant(t, db, kiwi, "control", "user", bob.ID, authz.RoleOperator)
	require.NoError(t, apps.SetFavorite(ctx, bob.ID, zebra, true))
	section, err := apps.CreateSection(ctx, bob.ID, "work")
	require.NoError(t, err)
	_, err = apps.PlaceApp(ctx, bob.ID, section.ID, mango)
	require.NoError(t, err)

	var order []string
	manage := map[string]bool{}
	cursor := ""
	for {
		page, next, err := apps.ListForUse(ctx, userPrincipal(bob), state.Page{Limit: 2, Cursor: cursor}, false)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page), 2)
		for _, a := range page {
			order = append(order, a.ID)
			manage[a.ID] = a.CanManage
		}
		if next == "" {
			break
		}
		cursor = next
	}
	require.Equal(t, []string{zebra, mango, apple, fig, kiwi}, order,
		"the favorite, then the filed app, then the rest by name")
	require.Equal(t, map[string]bool{zebra: false, mango: false, apple: false, fig: false, kiwi: true}, manage,
		"can_manage is the control plane's answer for each")

	every, _, err := apps.ListForUse(ctx, userPrincipal(bob), state.Page{}, true)
	require.NoError(t, err)
	for _, a := range every {
		require.True(t, a.CanManage, "install.apps.view manages every app")
	}

	found, _, err := apps.ListForUse(ctx, userPrincipal(bob), state.Page{Query: "ang"}, false)
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, mango, found[0].ID)
}

// TestR071_AnAccountsAppsArePagedByAppWithEveryGrantOnEach asserts that a
// person's apps come a page of apps at a time, with all of an app's grants —
// direct and through a group — on the page that has the app.
func TestR071_AnAccountsAppsArePagedByAppWithEveryGrantOnEach(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	bob := seedUser(t, db, "bob")
	team, err := state.NewGroups(db).Create(ctx, "team")
	require.NoError(t, err)
	require.NoError(t, state.NewGroups(db).AddMember(ctx, team.ID, bob.ID))

	var made []string
	for range 3 {
		appID := seedApp(t, db, alice.ID)
		grant(t, db, appID, "data", "user", bob.ID, nil)
		grant(t, db, appID, "control", "group", team.ID, authz.RoleViewer)
		made = append(made, appID)
	}
	grants := state.NewGrants(db)

	seen := map[string]int{}
	cursor, pages := "", 0
	for {
		page, next, err := grants.ForUser(ctx, bob.ID, state.Page{Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		pages++
		apps := map[string]bool{}
		for _, g := range page {
			apps[g.AppID] = true
			seen[g.AppID]++
		}
		require.LessOrEqual(t, len(apps), 2)
		if next == "" {
			break
		}
		cursor = next
	}
	require.Equal(t, 2, pages)
	require.Len(t, seen, 3)
	for _, appID := range made {
		require.Equal(t, 2, seen[appID], "the direct grant and the group's, together")
	}

	byGroup, next, err := grants.ForGroup(ctx, team.ID, state.Page{Limit: 5})
	require.NoError(t, err)
	require.Empty(t, next)
	require.Len(t, byGroup, 3)
	for _, g := range byGroup {
		require.Equal(t, "group", g.Via)
		require.Equal(t, team.ID, g.GroupID)
	}
}

// TestR075_AnAppsGrantsArePagedByPrincipalWithEveryoneFirst asserts that an
// app's grants come a page at a time, the grant to everyone on the first page
// and one principal's grants on both planes together.
func TestR075_AnAppsGrantsArePagedByPrincipalWithEveryoneFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedUser(t, db, "owner")
	appID := seedApp(t, db, owner.ID)
	grant(t, db, appID, "data", "anonymous", nil, nil)
	var people []string
	for i := range 4 {
		u := seedUser(t, db, fmt.Sprintf("person%d", i))
		grant(t, db, appID, "data", "user", u.ID, nil)
		grant(t, db, appID, "control", "user", u.ID, authz.RoleViewer)
		people = append(people, u.ID)
	}
	grants := state.NewGrants(db)

	first, next, err := grants.ListForApp(ctx, appID, state.Page{Limit: 3})
	require.NoError(t, err)
	require.NotEmpty(t, next)
	require.Equal(t, "anonymous", first[0].PrincipalKind, "the grant to everyone is first")

	all := first
	for next != "" {
		var page []state.GrantRow
		page, next, err = grants.ListForApp(ctx, appID, state.Page{Limit: 3, Cursor: next})
		require.NoError(t, err)
		all = append(all, page...)
	}
	require.Len(t, all, 9)
	for i := 1; i < len(all); i += 2 {
		require.Equal(t, all[i].PrincipalID, all[i+1].PrincipalID, "one principal's grants are adjacent")
		require.NotEqual(t, all[i].Plane, all[i+1].Plane)
	}
}

// TestSubscriptionsAndBackupsArePaged walks both lists a page at a time,
// narrowed and not, and checks the backup attempts are bounded.
func TestSubscriptionsAndBackupsArePaged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedUser(t, db, "owner")
	other := seedUser(t, db, "other")
	appID := seedApp(t, db, owner.ID)
	subs := state.NewSubscriptions(db)
	for i := range 5 {
		who, app := owner.ID, ""
		if i%2 == 1 {
			who = other.ID
		}
		if i < 2 {
			app = appID
		}
		_, err := subs.Create(ctx, state.Subscription{
			ID: id.New(id.Subscription), OwnerID: who, AppID: app, Events: []string{"*"},
			Destination: "webhook", URL: "https://example.test/hook", CreatedBy: who,
		})
		require.NoError(t, err)
	}
	walk := func(f state.SubscriptionFilter) int {
		n, cursor := 0, ""
		for {
			page, next, err := subs.ListPage(ctx, f, state.Page{Limit: 2, Cursor: cursor})
			require.NoError(t, err)
			require.LessOrEqual(t, len(page), 2)
			n += len(page)
			if next == "" {
				return n
			}
			cursor = next
		}
	}
	require.Equal(t, 5, walk(state.SubscriptionFilter{}))
	require.Equal(t, 3, walk(state.SubscriptionFilter{OwnerID: owner.ID}))
	require.Equal(t, 2, walk(state.SubscriptionFilter{AppID: appID}))
	require.Equal(t, 3, walk(state.SubscriptionFilter{InstallOnly: true}))
	all, err := subs.List(ctx, state.SubscriptionFilter{EnabledOnly: true})
	require.NoError(t, err)
	require.Len(t, all, 5)

	backups := state.NewBackups(db)
	for range 3 {
		require.NoError(t, backups.Record(ctx, state.Backup{
			ID: id.New(id.Backup), AppID: appID, Kind: "rolling", AdapterRef: "bk_fake",
			ObjectName: id.New(id.Backup), Manifest: backup.Manifest{}, CreatedBy: "test",
		}))
	}
	page, next, err := backups.List(ctx, "", state.Page{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.NotEmpty(t, next)
	rest, next, err := backups.List(ctx, appID, state.Page{Limit: 2, Cursor: next})
	require.NoError(t, err)
	require.Len(t, rest, 1)
	require.Empty(t, next)
	require.NotContains(t, []string{page[0].ID, page[1].ID}, rest[0].ID)

	for range 3 {
		app := seedApp(t, db, owner.ID)
		require.NoError(t, backups.RecordAttempt(ctx, state.BackupAttempt{
			AppID: app, AttemptedAt: time.Now().UTC(), Outcome: state.AttemptSkipped, Message: "Nothing to back up.",
		}))
	}
	attempts, err := backups.Attempts(ctx, "", 2)
	require.NoError(t, err)
	require.Len(t, attempts, 2, "at most the limit")
}

// TestR027_TheAuditLogsFiltersAreServedByIndexesInItsOrder asserts that each
// of the audit list's filters is answered from an index ordered as the list
// is read (000057), and that the indexes left the application role without
// UPDATE or DELETE on the log (R-027).
func TestR027_TheAuditLogsFiltersAreServedByIndexesInItsOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	tx, err := db.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
	require.NoError(t, err)

	for column, where := range map[string]string{
		"target_id":    `target_id = $1`,
		"on_behalf_of": `on_behalf_of = $1`,
		"app_id":       `app_id = $1`,
		"principal_id": `principal_id = $1`,
	} {
		plan := explain(t, tx, `SELECT id FROM audit_events WHERE `+where+` ORDER BY id DESC LIMIT 100`, "usr_x")
		require.Contains(t, plan, column, "the %s filter is not served by its index:\n%s", column, plan)
		require.Contains(t, plan, "Index", plan)
	}

	var update, del bool
	require.NoError(t, db.QueryRow(ctx, `
		SELECT has_table_privilege(current_user, 'audit_events', 'UPDATE'),
		       has_table_privilege(current_user, 'audit_events', 'DELETE')`).Scan(&update, &del))
	require.False(t, update)
	require.False(t, del)
}
