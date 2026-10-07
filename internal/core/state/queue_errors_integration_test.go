//go:build integration

package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// TestR256_TheQueueDepthCountsOnlyWorkNobodyHasClaimed asserts what the
// queue's depth means (issue #72, O-32): a deploy waiting for a replica counts,
// one a replica has taken does not, and asking for no work takes none.
func TestR256_TheQueueDepthCountsOnlyWorkNobodyHasClaimed(t *testing.T) {
	t.Parallel()
	f := newQueueFixture(t, "depth")
	deployments, detections := state.NewDeployments(f.live), state.NewDetections(f.live)

	depth, err := deployments.QueueDepth(f.ctx)
	require.NoError(t, err)
	require.Zero(t, depth)

	dep, err := deployments.Create(f.ctx, f.appID, f.revID, state.TriggerManual, f.owner)
	require.NoError(t, err)
	depth, err = deployments.QueueDepth(f.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, depth, "a queued deploy nobody has taken is waiting")

	none, err := deployments.Claim(f.ctx, 0)
	require.NoError(t, err)
	require.Empty(t, none, "room for nothing takes nothing")
	depth, err = deployments.QueueDepth(f.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, depth)

	claimed, err := deployments.Claim(f.ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, dep.ID, claimed[0].ID)
	depth, err = deployments.QueueDepth(f.ctx)
	require.NoError(t, err)
	require.Zero(t, depth, "a claimed deploy is no longer waiting")

	n, err := deployments.Release(f.ctx, nil)
	require.NoError(t, err)
	require.Zero(t, n, "releasing nothing releases nothing")

	_, err = detections.Queue(f.ctx, f.appID)
	require.NoError(t, err)
	apps, err := detections.Claim(f.ctx, 0)
	require.NoError(t, err)
	require.Empty(t, apps)
	n, err = detections.Release(f.ctx, nil)
	require.NoError(t, err)
	require.Zero(t, n)
	apps, err = detections.Claim(f.ctx, 5)
	require.NoError(t, err)
	require.Equal(t, []string{f.appID}, apps, "the detection was still queued")
}

// TestR105_TheQueueAndRetentionStoresSayWhatFailedWhenTheDatabaseIsGone
// asserts that every store the work queue, the sweeper and the retention job
// lean on reports a lost database as an internal error with a message saying
// what it was doing — not a driver error, and not an empty result that a
// caller would read as "nothing to do".
func TestR105_TheQueueAndRetentionStoresSayWhatFailedWhenTheDatabaseIsGone(t *testing.T) {
	t.Parallel()
	f := newQueueFixture(t, "gone")
	db, err := state.ConnectCopy(f.ctx, f.ownerURL, f.password)
	require.NoError(t, err)
	db.Close()

	deployments, detections := state.NewDeployments(db), state.NewDetections(db)
	retention := state.NewRetention(db)
	now := time.Now().UTC()

	cases := map[string]struct {
		run  func(context.Context) error
		says string
	}{
		"claim deploys": {func(ctx context.Context) error { _, err := deployments.Claim(ctx, 1); return err },
			"Could not take deploys from the queue."},
		"release deploys": {func(ctx context.Context) error { _, err := deployments.Release(ctx, []string{"dep_x"}); return err },
			"Could not return deploys to the queue."},
		"queue depth": {func(ctx context.Context) error { _, err := deployments.QueueDepth(ctx); return err },
			"Could not read the deploy queue."},
		"waiting": {func(ctx context.Context) error { _, err := deployments.Waiting(ctx, "dep_x"); return err },
			"Could not read the deploy."},
		"record prior state": {func(ctx context.Context) error {
			return deployments.RecordPriorState(ctx, "dep_x", state.StateRunning)
		}, "Could not record the deploy."},
		// Never "" with no error: a deploy reads that as "nothing to put back".
		"read prior state": {func(ctx context.Context) error { _, err := deployments.PriorState(ctx, "dep_x"); return err },
			"Could not read the deploy."},
		"settle app state": {func(ctx context.Context) error {
			return state.NewApps(db).SetStateIf(ctx, f.appID, state.StateDeploying, state.StateRunning)
		}, "Could not update the app."},
		"recover deploys": {func(ctx context.Context) error { _, err := deployments.RecoverInFlight(ctx); return err },
			"Could not record interrupted deploys."},
		"queue detection": {func(ctx context.Context) error { _, err := detections.Queue(ctx, f.appID); return err },
			"Could not start detection."},
		"claim detections": {func(ctx context.Context) error { _, err := detections.Claim(ctx, 1); return err },
			"Could not take detections from the queue."},
		"release detections": {func(ctx context.Context) error { _, err := detections.Release(ctx, []string{f.appID}); return err },
			"Could not return detections to the queue."},
		"recover detections": {func(ctx context.Context) error { _, err := detections.RecoverRunning(ctx); return err },
			"Could not record interrupted detections."},
		"due backups": {func(ctx context.Context) error {
			_, err := state.NewApps(db).DueForBackup(ctx, now, now, 10)
			return err
		}, "Could not list apps due a backup."},
		"enabled subscriptions": {func(ctx context.Context) error { _, _, err := state.NewSubscriptions(db).Enabled(ctx); return err },
			"Could not list subscriptions."},
		"silent replicas": {func(ctx context.Context) error { _, err := state.NewReplicas(db).StopSilent(ctx); return err },
			"Could not record silent replicas as stopped."},
		"security state": {func(ctx context.Context) error { _, err := state.NewScans(db).LiveSecurityState(ctx); return err },
			"Could not list apps for the security pass."},
		"old deploys": {func(ctx context.Context) error { _, err := retention.Deployments(ctx, 50, 10); return err },
			"Could not remove old deploys."},
		"deleted apps": {func(ctx context.Context) error { _, err := retention.DeletedApps(ctx, now, 10); return err },
			"Could not remove old detections."},
		"old scans": {func(ctx context.Context) error { _, err := retention.Scans(ctx, now, 10); return err },
			"Could not remove old scans."},
		"old sessions": {func(ctx context.Context) error { _, err := retention.Sessions(ctx, now, 10); return err },
			"Could not remove old sessions."},
		"old notifications": {func(ctx context.Context) error { _, err := retention.Notifications(ctx, now, 10); return err },
			"Could not remove old notifications."},
		"old idempotency keys": {func(ctx context.Context) error { _, err := retention.IdempotencyKeys(ctx, now, 10); return err },
			"Could not remove old idempotency keys."},
		"old sign-in flows": {func(ctx context.Context) error { _, err := retention.SSOFlows(ctx, now, now, 10); return err },
			"Could not remove old sign-in flows."},
	}
	for name, c := range cases {
		err := c.run(f.ctx)
		require.Error(t, err, name)
		require.Equal(t, errs.Internal, errs.CodeOf(err), name)
		e := errs.As(err)
		require.NotNil(t, e, name)
		require.Equal(t, c.says, e.Message, name)
	}
}
