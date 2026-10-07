//go:build integration

package state_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// The deploy and detection queue (issue #72, O-32). Each "replica" is its own
// connection to one database, which is all a replica is to the state store.

// queueFixture is an app with a revision, on a database two replicas share.
type queueFixture struct {
	ctx          context.Context
	live, gone   *state.DB
	appID, revID string
	owner        string
	ownerURL     string
	password     secret.Value
}

func newQueueFixture(t *testing.T, name string) queueFixture {
	t.Helper()
	ctx := context.Background()
	live, ownerURL := statetest.Connect(t)
	_, appPassword := statetest.Database(t)
	gone, err := state.ConnectCopy(ctx, ownerURL, appPassword)
	require.NoError(t, err)
	t.Cleanup(gone.Close)
	for _, db := range []*state.DB{live, gone} {
		require.NoError(t, state.NewReplicas(db).Register(ctx, state.Replica{
			ID: db.Replica(), Hostname: "test", AssertionKID: "kid", AssertionKey: make([]byte, 32),
		}))
	}

	owner := seedUser(t, live, name+"-owner")
	apps := state.NewApps(live)
	app, err := apps.Create(ctx, name, id.New(id.App), owner.ID, owner.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/" + name})
	require.NoError(t, err)
	rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion, AppID: app.ID,
		Source: spec.Source{Type: spec.SourceGit, URL: "https://example.test/" + name},
	}, spec.OriginManual, owner.ID)
	require.NoError(t, err)
	return queueFixture{ctx: ctx, live: live, gone: gone, appID: app.ID, revID: rev.ID, owner: owner.ID,
		ownerURL: ownerURL, password: appPassword}
}

// TestR256_QueuedWorkSurvivesARestart asserts that a deploy or a detection
// nobody has claimed yet is not recorded as interrupted by a restart or by the
// sweeper — it is waiting, not abandoned — and that the next replica with room
// takes it.
func TestR256_QueuedWorkSurvivesARestart(t *testing.T) {
	t.Parallel()
	f := newQueueFixture(t, "queued")
	deployments, detections := state.NewDeployments(f.live), state.NewDetections(f.live)

	dep, err := deployments.Create(f.ctx, f.appID, f.revID, state.TriggerManual, f.owner)
	require.NoError(t, err)
	require.NoError(t, detections.Start(f.ctx, f.appID))

	// The replica that took the request stops before anything claims them.
	require.NoError(t, state.NewReplicas(f.gone).Stop(f.ctx, f.gone.Replica()))
	n, err := deployments.RecoverInFlight(f.ctx)
	require.NoError(t, err)
	require.Zero(t, n, "queued work is nobody's to recover")
	n, err = detections.RecoverRunning(f.ctx)
	require.NoError(t, err)
	require.Zero(t, n)

	got, _, err := deployments.ByID(f.ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, state.DeployPending, got.Status, "still queued")
	waiting, err := deployments.Waiting(f.ctx, dep.ID)
	require.NoError(t, err)
	require.True(t, waiting)

	claimed, err := deployments.Claim(f.ctx, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, dep.ID, claimed[0].ID)
	runner, err := deployments.Runner(f.ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, f.live.Replica(), runner, "claiming records the replica running it, for its live log")

	apps, err := detections.Claim(f.ctx, 10)
	require.NoError(t, err)
	require.Equal(t, []string{f.appID}, apps)
	d, err := detections.Get(f.ctx, f.appID)
	require.NoError(t, err)
	require.Equal(t, state.DetectionRunning, d.Status)
}

// TestR256_TwoReplicasNeverClaimOneDeployment asserts that replicas claiming
// at the same moment take different deployments: SKIP LOCKED, not a race
// decided by whichever update lands last.
func TestR256_TwoReplicasNeverClaimOneDeployment(t *testing.T) {
	t.Parallel()
	f := newQueueFixture(t, "claims")
	apps := state.NewApps(f.live)

	// One queued deploy per app, since an app has at most one in flight.
	var want []string
	for i := 0; i < 40; i++ {
		app, err := apps.Create(f.ctx, "claims-"+id.New(id.App)[4:12], id.New(id.App), f.owner, f.owner,
			spec.Source{Type: spec.SourceGit, URL: "https://example.test/claims"})
		require.NoError(t, err)
		rev, err := apps.CreateRevision(f.ctx, app.ID, &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion, AppID: app.ID,
			Source: spec.Source{Type: spec.SourceGit, URL: "https://example.test/claims"},
		}, spec.OriginManual, f.owner)
		require.NoError(t, err)
		dep, err := state.NewDeployments(f.live).Create(f.ctx, app.ID, rev.ID, state.TriggerManual, f.owner)
		require.NoError(t, err)
		want = append(want, dep.ID)
	}

	var mu sync.Mutex
	seen := map[string]string{}
	var wg sync.WaitGroup
	for _, db := range []*state.DB{f.live, f.gone, f.live, f.gone} {
		wg.Add(1)
		go func(db *state.DB) {
			defer wg.Done()
			store := state.NewDeployments(db)
			for {
				got, err := store.Claim(f.ctx, 3)
				require.NoError(t, err)
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, dep := range got {
					_, twice := seen[dep.ID]
					require.False(t, twice, "deployment %s was claimed twice", dep.ID)
					seen[dep.ID] = db.Replica()
				}
				mu.Unlock()
			}
		}(db)
	}
	wg.Wait()
	require.Len(t, seen, len(want), "every queued deployment was claimed, once")
}

// TestR256_ADeadReplicasClaimedDeployIsResumedElsewhere asserts the choice
// made for O-32: a deploy claimed by a replica that then stops goes back in
// the queue and is taken by another, because every step before it commits is
// safe to repeat — and after MaxAttempts it is recorded as interrupted, so a
// build that kills whichever replica runs it does not go round for ever.
func TestR256_ADeadReplicasClaimedDeployIsResumedElsewhere(t *testing.T) {
	t.Parallel()
	f := newQueueFixture(t, "resumed")
	live, gone := state.NewDeployments(f.live), state.NewDeployments(f.gone)

	dep, err := live.Create(f.ctx, f.appID, f.revID, state.TriggerManual, f.owner)
	require.NoError(t, err)
	claimed, err := gone.Claim(f.ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.NoError(t, gone.SetStatus(f.ctx, dep.ID, state.DeployBuilding))

	// While its replica lives, the deploy is its own.
	n, err := live.RecoverInFlight(f.ctx)
	require.NoError(t, err)
	require.Zero(t, n, "a live replica's deploy carries on")
	none, err := live.Claim(f.ctx, 1)
	require.NoError(t, err)
	require.Empty(t, none)

	// It stops mid-build.
	require.NoError(t, state.NewReplicas(f.gone).Stop(f.ctx, f.gone.Replica()))
	n, err = live.RecoverInFlight(f.ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	got, _, err := live.ByID(f.ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, state.DeployPending, got.Status, "back in the queue, not failed")

	resumed, err := live.Claim(f.ctx, 1)
	require.NoError(t, err)
	require.Len(t, resumed, 1, "the surviving replica takes it")
	runner, err := live.Runner(f.ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, f.live.Replica(), runner)

	// The stopped replica, were it somehow still running, can no longer move
	// the deploy it lost.
	require.NoError(t, gone.Finish(f.ctx, dep.ID, state.DeployFailed, "", "late"))
	got, _, err = live.ByID(f.ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, state.DeployPending, got.Status, "fenced: only the claimant writes its status")

	// Every replica that takes it dies under it, up to MaxAttempts.
	for attempt := 2; attempt < state.MaxAttempts; attempt++ {
		next, err := state.ConnectCopy(f.ctx, f.ownerURL, f.password)
		require.NoError(t, err)
		t.Cleanup(next.Close)
		require.NoError(t, state.NewReplicas(next).Register(f.ctx, state.Replica{
			ID: next.Replica(), Hostname: "test", AssertionKID: "kid", AssertionKey: make([]byte, 32),
		}))
		require.NoError(t, state.NewReplicas(f.live).Stop(f.ctx, f.live.Replica()))
		_, err = state.NewDeployments(next).RecoverInFlight(f.ctx)
		require.NoError(t, err)
		again, err := state.NewDeployments(next).Claim(f.ctx, 1)
		require.NoError(t, err)
		require.Len(t, again, 1)
		f.live = next
	}
	require.NoError(t, state.NewReplicas(f.live).Stop(f.ctx, f.live.Replica()))
	last, err := state.ConnectCopy(f.ctx, f.ownerURL, f.password)
	require.NoError(t, err)
	t.Cleanup(last.Close)
	_, err = state.NewDeployments(last).RecoverInFlight(f.ctx)
	require.NoError(t, err)
	got, _, err = state.NewDeployments(last).ByID(f.ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, state.DeployFailed, got.Status, "given up on after MaxAttempts")
	require.Contains(t, got.ErrorDetail, "Pando stopped while this deploy was under way")

	inFlight, err := state.NewDeployments(last).InFlight(f.ctx, f.appID)
	require.NoError(t, err)
	require.False(t, inFlight, "the next deploy is not refused on account of one that will never finish")
}

// TestR256_AReleasedDeployIsNotCountedAsAnAttempt asserts that a replica
// shutting down hands its deploys straight back, uncounted: a rolling restart
// is not the deploy's fault.
func TestR256_AReleasedDeployIsNotCountedAsAnAttempt(t *testing.T) {
	t.Parallel()
	f := newQueueFixture(t, "released")
	deployments := state.NewDeployments(f.live)

	dep, err := deployments.Create(f.ctx, f.appID, f.revID, state.TriggerManual, f.owner)
	require.NoError(t, err)
	for i := 0; i < state.MaxAttempts+2; i++ {
		claimed, err := deployments.Claim(f.ctx, 1)
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		require.NoError(t, deployments.SetStatus(f.ctx, dep.ID, state.DeployApplying))
		n, err := deployments.Release(f.ctx, []string{dep.ID})
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	}

	// Stopping now, claimed, is the first attempt that counts.
	_, err = deployments.Claim(f.ctx, 1)
	require.NoError(t, err)
	require.NoError(t, state.NewReplicas(f.live).Stop(f.ctx, f.live.Replica()))
	_, err = state.NewDeployments(f.gone).RecoverInFlight(f.ctx)
	require.NoError(t, err)
	got, _, err := deployments.ByID(f.ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, state.DeployPending, got.Status)

	// A finished deploy is not released.
	claimed, err := state.NewDeployments(f.gone).Claim(f.ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.NoError(t, state.NewDeployments(f.gone).Finish(f.ctx, dep.ID, state.DeploySucceeded, "", ""))
	n, err := state.NewDeployments(f.gone).Release(f.ctx, []string{dep.ID})
	require.NoError(t, err)
	require.Zero(t, n)
}

// TestR256_ADeadReplicasDetectionIsResumedElsewhere is the same choice for a
// detection, which writes nothing but its own row.
func TestR256_ADeadReplicasDetectionIsResumedElsewhere(t *testing.T) {
	t.Parallel()
	f := newQueueFixture(t, "detect-resumed")
	live, gone := state.NewDetections(f.live), state.NewDetections(f.gone)

	require.NoError(t, live.Start(f.ctx, f.appID))
	claimed, err := gone.Claim(f.ctx, 5)
	require.NoError(t, err)
	require.Equal(t, []string{f.appID}, claimed)

	require.NoError(t, state.NewReplicas(f.gone).Stop(f.ctx, f.gone.Replica()))
	n, err := live.RecoverRunning(f.ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	again, err := live.Claim(f.ctx, 5)
	require.NoError(t, err)
	require.Equal(t, []string{f.appID}, again, "taken by the surviving replica")
}
