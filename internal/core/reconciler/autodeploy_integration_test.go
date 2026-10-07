//go:build integration

package reconciler_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

type movedHead struct{ sha string }

func (m movedHead) Resolve(context.Context, spec.Source) (string, error) { return m.sha, nil }

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

// TestR158_AutoDeploySkipsAnAppThatNeedsApproval asserts that an app which
// already auto-deploys stops doing so when policy starts requiring approval
// for it — skipped, not queued as a request per push — and starts again when
// policy stops.
func TestR158_AutoDeploySkipsAnAppThatNeedsApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	deployments := state.NewDeployments(db)
	owner := seedOwner(t, db)

	app, err := apps.Create(ctx, "auto-"+id.New(id.App), id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"})
	require.NoError(t, err)
	rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		AppID:         app.ID,
		Source:        spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main", Commit: "aaaaaaa"},
		Build:         spec.Build{Strategy: spec.BuildPrebuilt},
		Workloads:     []spec.Workload{{Name: "web", Image: "example/app:1", Primary: true, Exposed: true}},
		Routing:       spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
		Runtime:       spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
		Deploy: spec.Deploy{Strategy: spec.DeployRecreate,
			AutoDeploy: spec.AutoDeploy{Enabled: true, Trigger: spec.TriggerBranchUpdated, Branch: "main"}},
	}, spec.OriginEdited, owner)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, owner))

	pol := &mutablePolicy{doc: policy.Document{DeployApprovalApps: []string{app.ID}}}
	core, logs := observer.New(zap.InfoLevel)
	var enqueued []string
	job := &reconciler.AutoDeploy{
		Apps:        apps,
		Deployments: deployments,
		Resolver:    movedHead{sha: "bbbbbbb"},
		Enqueue: func(_ context.Context, dep state.Deployment, _ state.Revision) {
			enqueued = append(enqueued, dep.ID)
		},
		Logger: zap.New(core),
		Policy: pol,
	}

	job.Poll(ctx)
	deps, err := deployments.ListForApp(ctx, app.ID)
	require.NoError(t, err)
	require.Empty(t, deps, "no deploy, and no request waiting for approval either")
	require.Empty(t, enqueued)
	require.Equal(t, 1, logs.FilterMessage("auto-deploy skipped: this app's deploys need approval").Len())

	// Policy stops requiring it, and the next poll deploys the new commit.
	pol.set(policy.Document{})
	job.Poll(ctx)
	deps, err = deployments.ListForApp(ctx, app.ID)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	require.Equal(t, state.DeployPending, deps[0].Status)
	require.Len(t, enqueued, 1)
}

// slowHead is a resolver that takes a while, and counts how many resolves are
// under way at once.
type slowHead struct {
	running, most, calls atomic.Int32
}

func (s *slowHead) Resolve(context.Context, spec.Source) (string, error) {
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
	return "", nil // nothing new
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
		app, err := apps.Create(ctx, "auto-"+id.New(id.App), id.New(id.App), owner, owner,
			spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"})
		require.NoError(t, err)
		rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion,
			AppID:         app.ID,
			Source:        spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main", Commit: "aaaaaaa"},
			Build:         spec.Build{Strategy: spec.BuildPrebuilt},
			Workloads:     []spec.Workload{{Name: "web", Image: "example/app:1", Primary: true, Exposed: true}},
			Routing:       spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
			Runtime:       spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
			Deploy: spec.Deploy{Strategy: spec.DeployRecreate,
				AutoDeploy: spec.AutoDeploy{Enabled: true, Trigger: spec.TriggerBranchUpdated, Branch: "main"}},
		}, spec.OriginEdited, owner)
		require.NoError(t, err)
		require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, owner))
	}

	head := &slowHead{}
	job := &reconciler.AutoDeploy{
		Apps: apps, Deployments: state.NewDeployments(db), Resolver: head,
		Logger: zap.NewNop(), Concurrency: 4,
	}
	job.Poll(ctx)

	require.EqualValues(t, 12, head.calls.Load(), "every app is checked")
	require.LessOrEqual(t, head.most.Load(), int32(4), "never more than the limit at once")
	require.Greater(t, head.most.Load(), int32(1), "and more than one at a time")
}
