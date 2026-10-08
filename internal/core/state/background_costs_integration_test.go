//go:build integration

package state_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// The derived columns of migration 60, checked against the queries they
// replaced (issue #72).

// legacyAllocated is AllocatedOn as it was before migration 60: the pinned
// revisions' JSON, summed. The columns must always give the same answer.
func legacyAllocated(t *testing.T, db *state.DB, ref, exclude string) planner.Allocation {
	t.Helper()
	var a planner.Allocation
	require.NoError(t, db.QueryRow(context.Background(), `
		SELECT
			coalesce(sum((r.body->'resources'->>'cpu_millis')::int), 0),
			coalesce(sum((r.body->'resources'->>'memory_bytes')::bigint), 0),
			coalesce(sum((r.body->'resources'->>'disk_bytes')::bigint), 0),
			coalesce(sum((r.body->'retention'->>'log_bytes')::bigint), 0)
		FROM apps a
		JOIN spec_revisions r ON r.id = a.pinned_spec_id
		WHERE a.deleted_at IS NULL AND a.id <> $2
		  AND a.state IN ('running', 'degraded', 'deploying')
		  AND r.body->'runtime'->>'adapter_ref' = $1`, ref, exclude).
		Scan(&a.CPUMillis, &a.MemoryBytes, &a.DiskBytes, &a.LogBytes))
	return a
}

func sizedSpec(runtime string, n int) *spec.AppSpec {
	s := minimalSpec()
	s.Runtime.AdapterRef = runtime
	s.Routing.Port = 9000 + n
	s.Resources = spec.Resources{CPUMillis: 100 + n, MemoryBytes: int64(n+1) << 20, DiskBytes: int64(n+2) << 20}
	s.Retention.LogBytes = int64(n+3) << 10
	return s
}

// TestR242_CommittedResourcesStayExactUnderConcurrentPins asserts R-242's
// arithmetic after migration 60: what each app reserves is copied onto the app
// when its pin moves, and the sum the planner reads equals the sum over the
// pinned revisions — with pins landing concurrently, apps moving between
// runtimes and sizes, apps stopping, and the app being planned left out.
func TestR242_CommittedResourcesStayExactUnderConcurrentPins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)
	allocations := state.NewAllocations(db)

	const n = 24
	ids := make([]string, n)
	revs := make([][2]state.Revision, n)
	for i := range n {
		app, err := apps.Create(ctx, fmt.Sprintf("app %d", i), fmt.Sprintf("app-%d", i), alice.ID, alice.ID,
			spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
		require.NoError(t, err)
		ids[i] = app.ID
		runtime := []string{"rt_a", "rt_b"}[i%2]
		first, err := apps.CreateRevision(ctx, app.ID, sizedSpec(runtime, i), spec.OriginManual, alice.ID)
		require.NoError(t, err)
		// The second revision moves every third app to the other runtime.
		other := runtime
		if i%3 == 0 {
			other = map[string]string{"rt_a": "rt_b", "rt_b": "rt_a"}[runtime]
		}
		second, err := apps.CreateRevision(ctx, app.ID, sizedSpec(other, i+100), spec.OriginEdited, alice.ID)
		require.NoError(t, err)
		revs[i] = [2]state.Revision{first, second}
	}

	pinAll := func(which int) {
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				require.NoError(t, apps.Pin(ctx, ids[i], revs[i][which].ID, state.StateRunning, alice.ID))
			}()
		}
		wg.Wait()
	}
	check := func(why string) {
		t.Helper()
		for _, ref := range []string{"rt_a", "rt_b", "rt_none"} {
			for _, exclude := range []string{"", ids[0], ids[1]} {
				got, err := allocations.AllocatedOn(ctx, ref, exclude)
				require.NoError(t, err)
				require.Equal(t, legacyAllocated(t, db, ref, exclude), got, "%s: %s excluding %q", why, ref, exclude)
			}
		}
	}

	pinAll(0)
	check("first pins")
	got, err := allocations.AllocatedOn(ctx, "rt_a", "")
	require.NoError(t, err)
	require.NotZero(t, got.CPUMillis, "something is committed")

	pinAll(1)
	check("repinned, some to the other runtime")

	// Stopping and failing take an app out of the sum without touching its pin.
	require.NoError(t, apps.SetState(ctx, ids[2], state.StateStopped))
	require.NoError(t, apps.SetState(ctx, ids[3], state.StateDegraded))
	require.NoError(t, apps.SetState(ctx, ids[4], state.StateDeploying))
	check("states changed")

	// Rolling back is a pin like any other.
	pinAll(0)
	check("rolled back")
}

// TestR367_OnlyAChangeRoutingReadsIsAnnounced asserts migration 61's trigger:
// a subscription made, turned off or removed is announced on
// SubscriptionsChannel, and a delivery's success or failure — which updates
// the row on every send — is not, so replicas do not reread the list for it.
// Notifications arrive in commit order, so one seen after another proves
// nothing came between.
func TestR367_OnlyAChangeRoutingReadsIsAnnounced(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db := connected(t)
	alice := seedUser(t, db, "alice")
	subs := state.NewSubscriptions(db)

	told := make(chan struct{}, 16)
	up := make(chan bool, 4)
	go subs.Watch(ctx, func() { told <- struct{}{} }, func(c bool) { up <- c })
	require.True(t, <-up, "listening")

	expect := func(n int, why string) {
		t.Helper()
		for range n {
			select {
			case <-told:
			case <-time.After(10 * time.Second):
				t.Fatalf("not told: %s", why)
			}
		}
	}

	sub, err := subs.Create(ctx, state.Subscription{
		ID: id.New(id.Subscription), OwnerID: alice.ID, Events: []string{"deploy.failed"},
		Destination: state.DestinationWebhook, URL: "https://example.test/hook", CreatedBy: alice.ID,
	})
	require.NoError(t, err)
	expect(1, "made")

	_, _, err = subs.Failed(ctx, sub.ID, time.Now())
	require.NoError(t, err)
	require.NoError(t, subs.Succeeded(ctx, sub.ID))
	desc := "renamed"
	_, err = subs.Update(ctx, sub.ID, state.SubscriptionPatch{Description: &desc})
	require.NoError(t, err)
	_, err = subs.Disable(ctx, sub.ID, "the endpoint is gone")
	require.NoError(t, err)
	expect(1, "turned off")
	select {
	case <-told:
		t.Fatal("a delivery's outcome or a new description was announced as a change")
	default:
	}

	require.NoError(t, subs.Delete(ctx, sub.ID))
	expect(1, "removed")
}

// legacyScore is the pinned revision's newest scan as the security pass read
// it before migration 60, with a lookup per app.
func legacyScore(t *testing.T, db *state.DB, appID string) (*int, *int) {
	t.Helper()
	var all, fixable *int
	err := db.QueryRow(context.Background(), `
		SELECT s.score, s.score_fixable FROM apps a
		LEFT JOIN LATERAL (
		    SELECT sc.score, sc.score_fixable FROM app_scans sc
		    WHERE sc.app_id = a.id AND (sc.spec_id = a.pinned_spec_id OR sc.spec_id IS NULL)
		    ORDER BY (sc.spec_id IS NOT NULL) DESC, sc.ran_at DESC LIMIT 1
		) s ON true
		WHERE a.id = $1`, appID).Scan(&all, &fixable)
	require.NoError(t, err)
	return all, fixable
}

// TestR315_TheSecurityPassReadsOnlyTheAppsItCouldActOn asserts the security
// pass's read after issue #72: one set-based query that returns the apps that
// are marked or do not meet the threshold — including those never scanned —
// and leaves out the ones the pass would find fine and leave alone (R-315,
// R-316). The scores it reads follow every scan and every pin.
func TestR315_TheSecurityPassReadsOnlyTheAppsItCouldActOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)
	scans := state.NewScans(db)

	type app struct {
		id  string
		rev state.Revision
	}
	made := map[string]app{}
	for i, name := range []string{"fine", "low", "unscanned", "marked", "fixable", "archived", "rolled"} {
		a, err := apps.Create(ctx, name, name, alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/" + name})
		require.NoError(t, err)
		rev, err := apps.CreateRevision(ctx, a.ID, sizedSpec("rt_a", i), spec.OriginManual, alice.ID)
		require.NoError(t, err)
		require.NoError(t, apps.Pin(ctx, a.ID, rev.ID, state.StateRunning, alice.ID))
		made[name] = app{a.ID, rev}
	}
	record := func(name string, all, fixable *int) {
		t.Helper()
		_, err := scans.Record(ctx, state.Scan{AppID: made[name].id, SpecID: made[name].rev.ID,
			ScannerRef: "scn_trivy", Score: all, ScoreFixable: fixable})
		require.NoError(t, err)
	}
	record("fine", score(90), score(95))
	record("low", score(30), score(40))
	record("marked", score(90), nil)
	record("fixable", score(30), score(80))
	record("archived", score(10), nil)
	record("rolled", score(20), nil)
	require.NoError(t, scans.MarkInsecure(ctx, made["marked"].id, time.Now()))
	require.NoError(t, apps.SetState(ctx, made["archived"].id, state.StateArchived))

	// A newer scan of the same revision replaces the score; one of a revision
	// the app is not running does not count until it is pinned.
	time.Sleep(10 * time.Millisecond)
	record("rolled", score(85), nil)
	next, err := apps.CreateRevision(ctx, made["rolled"].id, sizedSpec("rt_a", 40), spec.OriginEdited, alice.ID)
	require.NoError(t, err)
	_, err = scans.Record(ctx, state.Scan{AppID: made["rolled"].id, SpecID: next.ID, ScannerRef: "scn_trivy", Score: score(5)})
	require.NoError(t, err)

	names := func(th state.SecurityThreshold) []string {
		got, err := scans.LiveSecurityState(ctx, th)
		require.NoError(t, err)
		byID := map[string]string{}
		for name, a := range made {
			byID[a.id] = name
		}
		var out []string
		for _, row := range got {
			out = append(out, byID[row.AppID])
			all, fixable := legacyScore(t, db, row.AppID)
			require.Equal(t, all, row.Score, "the kept score is the newest scan's, for %s", byID[row.AppID])
			require.Equal(t, fixable, row.ScoreFixable)
		}
		return out
	}

	require.ElementsMatch(t, []string{"low", "unscanned", "marked", "fixable"},
		names(state.SecurityThreshold{MinScore: 50}))
	require.ElementsMatch(t, []string{"low", "unscanned", "marked"},
		names(state.SecurityThreshold{MinScore: 50, IgnoreUnfixable: true}),
		"reading the fixable score, the app whose fixable findings are few meets the threshold")

	// Pinning the revision with the bad scan brings that score with it.
	require.NoError(t, apps.Pin(ctx, made["rolled"].id, next.ID, state.StateRunning, alice.ID))
	require.Contains(t, names(state.SecurityThreshold{MinScore: 50}), "rolled")

	// A scan removed — retention, or the app's own cascade — is recomputed.
	_, err = db.Exec(ctx, `DELETE FROM app_scans WHERE app_id = $1 AND spec_id = $2`, made["rolled"].id, next.ID)
	require.NoError(t, err)
	all, _ := legacyScore(t, db, made["rolled"].id)
	require.Nil(t, all, "nothing describes the pinned revision now")
	require.Contains(t, names(state.SecurityThreshold{MinScore: 50}), "rolled", "and an unscanned app is still the pass's")
}
