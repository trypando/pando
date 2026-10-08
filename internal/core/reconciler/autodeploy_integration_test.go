//go:build integration

package reconciler_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// movedHead is a remote whose watched ref points at sha.
type movedHead struct {
	mu    sync.Mutex
	ref   string
	sha   string
	calls int
}

func (m *movedHead) Resolve(context.Context, spec.Source, spec.AutoDeploy) (source.Tracked, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	ref := m.ref
	if ref == "" {
		ref = "refs/heads/main"
	}
	return source.Tracked{Ref: ref, Commit: m.sha}, nil
}

func (m *movedHead) move(sha string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sha = sha
}

func (m *movedHead) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// recordingDeployer stands in for approval.Service: it records the deploy as
// the service would, or refuses it.
type recordingDeployer struct {
	deployments *state.Deployments
	refuse      error

	mu    sync.Mutex
	calls []deployCall
}

type deployCall struct {
	principal authz.Principal
	rev       state.Revision
	trigger   string
}

func (d *recordingDeployer) Deploy(ctx context.Context, p authz.Principal, app state.App, rev state.Revision, trigger string) (state.Deployment, error) {
	d.mu.Lock()
	d.calls = append(d.calls, deployCall{principal: p, rev: rev, trigger: trigger})
	d.mu.Unlock()
	if d.refuse != nil {
		return state.Deployment{}, d.refuse
	}
	return d.deployments.Create(ctx, app.ID, rev.ID, trigger, p.ID)
}

func (d *recordingDeployer) made() []deployCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]deployCall(nil), d.calls...)
}

type mutablePolicy struct {
	mu  sync.Mutex
	doc policy.Document
}

func (p *mutablePolicy) Load(context.Context) (policy.Document, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.doc, nil
}

func (p *mutablePolicy) set(doc policy.Document) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.doc = doc
}

// autoApp creates a running app, pinned at commit aaaaaaa, with ad as its
// automatic deploy settings.
func autoApp(t *testing.T, apps *state.Apps, owner string, ad spec.AutoDeploy) state.App {
	t.Helper()
	ctx := context.Background()
	src := spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"}
	app, err := apps.Create(ctx, "auto-"+id.New(id.App), id.New(id.App), owner, owner, src)
	require.NoError(t, err)
	src.Commit = "aaaaaaa"
	rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		AppID:         app.ID,
		Source:        src,
		Build:         spec.Build{Strategy: spec.BuildPrebuilt},
		Workloads:     []spec.Workload{{Name: "web", Image: "example/app:1", Primary: true, Exposed: true}},
		Routing:       spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
		Runtime:       spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
		Deploy:        spec.Deploy{Strategy: spec.DeployRecreate, AutoDeploy: ad},
	}, spec.OriginEdited, owner)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, owner))
	app.PinnedSpecID = rev.ID
	return app
}

func job(db *state.DB, head reconciler.RefResolver, deployer reconciler.Deployer) *reconciler.AutoDeploy {
	return &reconciler.AutoDeploy{
		Apps:        state.NewApps(db),
		Deployments: state.NewDeployments(db),
		Checks:      state.NewAutoDeployChecks(db),
		Resolver:    head,
		Deployer:    deployer,
		Audit:       audit.New(db.Pool),
		Logger:      zap.NewNop(),
	}
}

var onBranch = spec.AutoDeploy{Enabled: true, Trigger: spec.TriggerBranchUpdated, Branch: "main"}

// TestR141_AutoDeployIsOffByDefault asserts that an app which did not ask to
// deploy automatically is never checked, whatever its branch does.
func TestR141_AutoDeployIsOffByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedOwner(t, db)
	app := autoApp(t, state.NewApps(db), owner, spec.AutoDeploy{})

	head := &movedHead{sha: "bbbbbbb"}
	deployer := &recordingDeployer{deployments: state.NewDeployments(db)}
	job(db, head, deployer).Poll(ctx)

	require.Empty(t, deployer.made())
	deps, err := state.NewDeployments(db).ListForApp(ctx, app.ID)
	require.NoError(t, err)
	require.Empty(t, deps)
}

// TestR141_BranchTriggerDeploysANewCommitAsTheSystem asserts that a moved
// branch becomes a new revision — recorded as auto-deploy's, carrying the new
// commit — deployed through the deploy service as the system principal, and
// audited with the commit it moved from and to (R-227).
func TestR141_BranchTriggerDeploysANewCommitAsTheSystem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)
	app := autoApp(t, apps, owner, spec.AutoDeploy{Enabled: true, Branch: "staging"})

	head := &movedHead{ref: "refs/heads/staging", sha: "bbbbbbb"}
	deployer := &recordingDeployer{deployments: state.NewDeployments(db)}
	job(db, head, deployer).Poll(ctx)

	calls := deployer.made()
	require.Len(t, calls, 1)
	require.Equal(t, authz.System(), calls[0].principal)
	require.Equal(t, "branch_updated", calls[0].trigger)
	require.Equal(t, spec.OriginAutoDeploy, calls[0].rev.Origin)
	require.Equal(t, "bbbbbbb", calls[0].rev.Body.Source.Commit)
	require.Equal(t, "staging", calls[0].rev.Body.Source.Ref, "the branch it follows, not the one it was deployed from")

	var detail []byte
	var kind string
	require.NoError(t, db.Pool.QueryRow(ctx, `
		SELECT principal_kind, detail FROM audit_events
		WHERE action = 'app.auto_deploy' AND app_id = $1`, app.ID).Scan(&kind, &detail))
	require.Equal(t, "system", kind)
	var got map[string]any
	require.NoError(t, json.Unmarshal(detail, &got))
	require.Equal(t, "aaaaaaa", got["from_commit"])
	require.Equal(t, "bbbbbbb", got["to_commit"])
	require.Equal(t, "poll", got["delivery"])

	check, found, err := state.NewAutoDeployChecks(db).ByApp(ctx, app.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "bbbbbbb", check.FoundCommit)
	require.Equal(t, "bbbbbbb", check.AttemptedCommit)
	require.NotEmpty(t, check.DeploymentID)
}

// TestR141_ReleaseTriggerPollsOnItsOwnInterval asserts that the release
// trigger is its own: the branch poll does not touch an app following
// releases, the release poll does, and the revision names the tag.
func TestR141_ReleaseTriggerPollsOnItsOwnInterval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedOwner(t, db)
	autoApp(t, state.NewApps(db), owner, spec.AutoDeploy{Enabled: true, Trigger: spec.TriggerReleaseTagged})

	head := &movedHead{ref: "refs/tags/v1.1.0", sha: "ccccccc"}
	deployer := &recordingDeployer{deployments: state.NewDeployments(db)}
	j := job(db, head, deployer)

	j.PollTrigger(ctx, spec.TriggerBranchUpdated)
	require.Zero(t, head.count(), "the branch poll leaves release apps alone")

	j.PollTrigger(ctx, spec.TriggerReleaseTagged)
	calls := deployer.made()
	require.Len(t, calls, 1)
	require.Equal(t, "release_tagged", calls[0].trigger)
	require.Equal(t, "refs/tags/v1.1.0", calls[0].rev.Body.Source.Ref)
	require.Equal(t, "ccccccc", calls[0].rev.Body.Source.Commit)
	require.Less(t, reconciler.BranchPollInterval, reconciler.ReleasePollInterval)
}

// TestR141_ACommitIsTriedOnce asserts O-56: a commit whose deploy is refused
// is not tried again on the next poll, and the next commit is.
func TestR141_ACommitIsTriedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedOwner(t, db)
	app := autoApp(t, state.NewApps(db), owner, onBranch)

	head := &movedHead{sha: "bbbbbbb"}
	deployer := &recordingDeployer{
		deployments: state.NewDeployments(db),
		refuse:      errs.New(errs.ValidInvalid, "This app's plan does not pass."),
	}
	j := job(db, head, deployer)

	j.Poll(ctx)
	j.Poll(ctx)
	require.Len(t, deployer.made(), 1, "the refused commit is not retried")

	check, _, err := state.NewAutoDeployChecks(db).ByApp(ctx, app.ID)
	require.NoError(t, err)
	require.Equal(t, "This app's plan does not pass.", check.Error, "and why stays on the check")

	head.move("ddddddd")
	j.Poll(ctx)
	calls := deployer.made()
	require.Len(t, calls, 2)
	require.Equal(t, "ddddddd", calls[1].rev.Body.Source.Commit)
}

// TestR141_ANewCommitWhileADeployRunsIsSkippedNotQueued asserts design 05 §5:
// with a deploy in flight, a new commit is neither deployed nor queued, and
// is picked up by the first poll after the deploy ends.
func TestR141_ANewCommitWhileADeployRunsIsSkippedNotQueued(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	deployments := state.NewDeployments(db)
	owner := seedOwner(t, db)
	app := autoApp(t, state.NewApps(db), owner, onBranch)

	running, err := deployments.Create(ctx, app.ID, app.PinnedSpecID, state.TriggerManual, owner)
	require.NoError(t, err)

	head := &movedHead{sha: "bbbbbbb"}
	deployer := &recordingDeployer{deployments: deployments}
	job(db, head, deployer).Poll(ctx)

	require.Empty(t, deployer.made())
	deps, err := deployments.ListForApp(ctx, app.ID)
	require.NoError(t, err)
	require.Len(t, deps, 1, "nothing queued behind the running deploy")
	require.Equal(t, running.ID, deps[0].ID)
	_, found, err := state.NewAutoDeployChecks(db).ByApp(ctx, app.ID)
	require.NoError(t, err)
	require.False(t, found, "nothing attempted, so nothing stops the next poll")
}

// TestR142_AWebhookCheckGoesTheWayAPollDoes asserts that a check a webhook
// asks for is the poll's own check of one app — the same deploy, recorded as
// delivered by webhook — and leaves alone the apps a poll leaves alone.
func TestR142_AWebhookCheckGoesTheWayAPollDoes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)
	app := autoApp(t, apps, owner, onBranch)

	head := &movedHead{sha: "bbbbbbb"}
	deployer := &recordingDeployer{deployments: state.NewDeployments(db)}
	j := job(db, head, deployer)
	require.NoError(t, j.CheckApp(ctx, app.ID, reconciler.DeliveryWebhook))

	calls := deployer.made()
	require.Len(t, calls, 1)
	require.Equal(t, authz.System(), calls[0].principal)
	var delivery string
	require.NoError(t, db.Pool.QueryRow(ctx, `
		SELECT detail->>'delivery' FROM audit_events
		WHERE action = 'app.auto_deploy' AND app_id = $1`, app.ID).Scan(&delivery))
	require.Equal(t, "webhook", delivery)

	failed := autoApp(t, apps, owner, onBranch)
	require.NoError(t, apps.SetState(ctx, failed.ID, state.StateFailed))
	require.NoError(t, j.CheckApp(ctx, failed.ID, reconciler.DeliveryWebhook))
	require.Len(t, deployer.made(), 1, "R-151: a webhook does not deploy a failed app either")
}

// TestR151_AutoDeployLeavesAFailedAppAlone asserts that an app in failed is
// not deployed automatically: it stays failed until a person acts.
func TestR151_AutoDeployLeavesAFailedAppAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)
	app := autoApp(t, apps, owner, onBranch)
	require.NoError(t, apps.SetState(ctx, app.ID, state.StateFailed))

	head := &movedHead{sha: "bbbbbbb"}
	deployer := &recordingDeployer{deployments: state.NewDeployments(db)}
	job(db, head, deployer).Poll(ctx)

	require.Zero(t, head.count())
	require.Empty(t, deployer.made())
}

// TestR158_AutoDeploySkipsAnAppThatNeedsApproval asserts that an app which
// already auto-deploys stops doing so when policy starts requiring approval
// for it — skipped, not queued as a request per push — and starts again when
// policy stops.
func TestR158_AutoDeploySkipsAnAppThatNeedsApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	deployments := state.NewDeployments(db)
	owner := seedOwner(t, db)
	app := autoApp(t, state.NewApps(db), owner, onBranch)

	pol := &mutablePolicy{doc: policy.Document{DeployApprovalApps: []string{app.ID}}}
	core, logs := observer.New(zap.InfoLevel)
	deployer := &recordingDeployer{deployments: deployments}
	j := job(db, &movedHead{sha: "bbbbbbb"}, deployer)
	j.Logger = zap.New(core)
	j.Policy = pol

	j.Poll(ctx)
	deps, err := deployments.ListForApp(ctx, app.ID)
	require.NoError(t, err)
	require.Empty(t, deps, "no deploy, and no request waiting for approval either")
	require.Empty(t, deployer.made())
	require.Equal(t, 1, logs.FilterMessage("auto-deploy skipped: this app's deploys need approval").Len())

	// Policy stops requiring it, and the next poll deploys the new commit.
	pol.set(policy.Document{})
	j.Poll(ctx)
	deps, err = deployments.ListForApp(ctx, app.ID)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	require.Equal(t, state.DeployPending, deps[0].Status)
	require.Len(t, deployer.made(), 1)
}

// slowHead is a resolver that takes a while, and counts how many resolves are
// under way at once.
type slowHead struct {
	running, most, calls atomic.Int32
}

func (s *slowHead) Resolve(context.Context, spec.Source, spec.AutoDeploy) (source.Tracked, error) {
	now := s.running.Add(1)
	defer s.running.Add(-1)
	s.calls.Add(1)
	for {
		prev := s.most.Load()
		if now <= prev || s.most.CompareAndSwap(prev, now) {
			break
		}
	}
	time.Sleep(30 * time.Millisecond)
	return source.Tracked{}, nil // nothing new
}

// TestR141_AutoDeployChecksAppsConcurrentlyWithinItsLimit asserts that the
// poll (R-141) checks several apps at once, and no more than its limit: in
// series, a `git ls-remote` per app outlasted the poll's own interval on a
// few thousand apps (issue #72).
func TestR141_AutoDeployChecksAppsConcurrentlyWithinItsLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)

	for i := 0; i < 12; i++ {
		autoApp(t, apps, owner, onBranch)
	}

	head := &slowHead{}
	j := job(db, head, &recordingDeployer{refuse: errors.New("nothing should deploy")})
	j.Concurrency = 4
	j.Poll(ctx)

	require.EqualValues(t, 12, head.calls.Load(), "every app is checked")
	require.LessOrEqual(t, head.most.Load(), int32(4), "never more than the limit at once")
	require.Greater(t, head.most.Load(), int32(1), "and more than one at a time")
}
