//go:build integration

package state_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

func awaitingFixture(t *testing.T) (context.Context, *state.Deployments, string, string, string) {
	t.Helper()
	ctx := context.Background()
	db := connected(t)
	owner := seedUser(t, db, "approval-owner-"+id.New(id.User)[4:12])
	apps := state.NewApps(db)

	app, err := apps.Create(ctx, "approval-"+id.New(id.App)[4:12], id.New(id.App), owner.ID, owner.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion, AppID: app.ID,
		Source: spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"},
	}, spec.OriginManual, owner.ID)
	require.NoError(t, err)
	return ctx, state.NewDeployments(db), app.ID, rev.ID, owner.ID
}

// TestR156_TwoApprovalsArrivingTogetherStartTheDeployOnce asserts the
// conditional move from awaiting_approval to pending: of two final approvals
// racing, exactly one starts it.
func TestR156_TwoApprovalsArrivingTogetherStartTheDeployOnce(t *testing.T) {
	t.Parallel()
	ctx, deployments, appID, specID, owner := awaitingFixture(t)

	dep, err := deployments.CreateAwaiting(ctx, appID, specID, state.TriggerManual, owner, 1, nil, []string{"install"})
	require.NoError(t, err)
	require.Equal(t, state.DeployAwaitingApproval, dep.Status)

	inFlight, err := deployments.InFlight(ctx, appID)
	require.NoError(t, err)
	require.False(t, inFlight, "a request waiting for approval is not a deploy in flight")

	var wg sync.WaitGroup
	results := make([]bool, 8)
	for n := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started, err := deployments.StartApproved(ctx, dep.ID)
			require.NoError(t, err)
			results[n] = started
		}()
	}
	wg.Wait()

	starts := 0
	for _, started := range results {
		if started {
			starts++
		}
	}
	require.Equal(t, 1, starts)

	got, _, err := deployments.ByID(ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, state.DeployPending, got.Status)
}

// TestR156_ARequestDoesNotStartAlongsideADeployInFlight asserts that the
// start itself refuses while the app is deploying, not only the check before
// it.
func TestR156_ARequestDoesNotStartAlongsideADeployInFlight(t *testing.T) {
	t.Parallel()
	ctx, deployments, appID, specID, owner := awaitingFixture(t)

	waiting, err := deployments.CreateAwaiting(ctx, appID, specID, state.TriggerManual, owner, 1, nil, []string{"install"})
	require.NoError(t, err)
	running, err := deployments.Create(ctx, appID, specID, state.TriggerManual, owner)
	require.NoError(t, err)

	started, err := deployments.StartApproved(ctx, waiting.ID)
	require.NoError(t, err)
	require.False(t, started)

	require.NoError(t, deployments.Finish(ctx, running.ID, state.DeploySucceeded, "", ""))
	started, err = deployments.StartApproved(ctx, waiting.ID)
	require.NoError(t, err)
	require.True(t, started)
}

// TestR156_DecisionsAreOnePerPersonAndTheRestartLeavesRequestsAlone asserts
// two things the store owns: a person decides once (deciding again replaces
// the answer), and a restart abandons deploys in flight but not requests that
// are waiting for somebody — nothing about a restart answers them.
func TestR156_DecisionsAreOnePerPersonAndTheRestartLeavesRequestsAlone(t *testing.T) {
	t.Parallel()
	ctx, deployments, appID, specID, owner := awaitingFixture(t)

	expires := time.Now().UTC().Add(time.Hour)
	dep, err := deployments.CreateAwaiting(ctx, appID, specID, state.TriggerManual, owner, 2, &expires,
		[]string{"install", "app_spec"})
	require.NoError(t, err)

	require.NoError(t, deployments.Decide(ctx, dep.ID, owner, state.DecisionApprove, ""))
	require.NoError(t, deployments.Decide(ctx, dep.ID, owner, state.DecisionApprove, "still fine"))

	got, _, err := deployments.ByID(ctx, dep.ID)
	require.NoError(t, err)
	require.Len(t, got.Approvals, 1)
	require.Equal(t, "still fine", got.Approvals[0].Comment)
	require.NotEmpty(t, got.Approvals[0].PrincipalName)
	require.Equal(t, 2, got.ApprovalsRequired)
	require.Len(t, got.ApprovalReasons, 2)
	require.WithinDuration(t, expires, *got.ApprovalExpiresAt, time.Second)
	require.Equal(t, 1, got.SpecRevision)

	_, err = deployments.RecoverInFlight(ctx)
	require.NoError(t, err)
	got, _, err = deployments.ByID(ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, state.DeployAwaitingApproval, got.Status)

	// Superseding ends it, and the decision stays on the record.
	newer, err := deployments.CreateAwaiting(ctx, appID, specID, state.TriggerManual, owner, 1, nil, nil)
	require.NoError(t, err)
	ended, err := deployments.SupersedeAwaiting(ctx, appID, newer.ID)
	require.NoError(t, err)
	require.Equal(t, []string{dep.ID}, ended)

	waiting, err := deployments.ListAwaiting(ctx)
	require.NoError(t, err)
	var ids []string
	for _, w := range waiting {
		if w.AppID == appID {
			ids = append(ids, w.ID)
		}
	}
	require.Equal(t, []string{newer.ID}, ids)
}
