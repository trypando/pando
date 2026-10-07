package reconciler

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/approval"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/work"
)

// Auto-deploy polling intervals. [P], design 05 §5.
const (
	BranchPollInterval  = 5 * time.Minute
	ReleasePollInterval = 15 * time.Minute
)

// AutoDeploy watches tracked refs and enqueues deployments (R-141).
//
// A separate scheduled job, deliberately not part of the reconciler. It never
// modifies a running app: it creates a spec revision carrying the new commit
// and enqueues a deployment, and everything then flows through the normal path
// including every plan-time check. The reconciler converges what is pinned;
// this decides what should be pinned. Conflating them would make an unrelated
// drift correction able to ship new code.
//
// Off by default (R-141). Nothing here runs for an app that did not ask.
type AutoDeploy struct {
	Apps        *state.Apps
	Deployments *state.Deployments
	Resolver    RefResolver

	// Enqueue tells the deploy queue a deployment is waiting. The
	// deployment is already queued when this is called (Deployments.Create);
	// any replica's queue runs it (issue #72, O-32).
	Enqueue func(ctx context.Context, dep state.Deployment, rev state.Revision)
	Logger  *zap.Logger

	// Concurrency is how many apps are checked at once. Eight when zero
	// [P]: each check is a `git ls-remote` against somebody's git host, and
	// in series a poll over a few thousand apps outlasted its own interval.
	Concurrency int

	// Policy is host policy as it is now. An app whose deploys need approval
	// does not auto-deploy (R-158); nil reads as no policy, so nothing is
	// skipped for it.
	Policy PolicyLoader
}

// PolicyLoader reads host policy, per poll rather than cached (R-274): an
// administrator who starts requiring approval stops the next poll.
type PolicyLoader interface {
	Load(ctx context.Context) (policy.Document, error)
}

// RefResolver turns a ref into the commit it currently points at.
//
// Listing a remote's refs rather than cloning: this runs every five minutes for
// every app tracking a branch, and cloning each time to learn a SHA that has
// usually not changed would be most of Pando's network traffic.
type RefResolver interface {
	Resolve(ctx context.Context, src spec.Source) (string, error)
}

// Run polls until the context is canceled.
func (a *AutoDeploy) Run(ctx context.Context) {
	ticker := time.NewTicker(BranchPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.Poll(ctx)
		}
	}
}

// Poll checks every app that tracks a ref.
func (a *AutoDeploy) Poll(ctx context.Context) {
	apps, err := a.Apps.WithAutoDeploy(ctx)
	if err != nil {
		a.Logger.Warn("could not list apps tracking a branch", zap.Error(err))
		return
	}

	var doc policy.Document
	if a.Policy != nil && len(apps) > 0 {
		if doc, err = a.Policy.Load(ctx); err != nil {
			// Skipping the poll rather than polling without policy: deploying
			// an app that may need approval without asking is the one
			// outcome this must not produce.
			a.Logger.Warn("could not read host policy; skipping this auto-deploy poll", zap.Error(err))
			return
		}
	}

	concurrency := a.Concurrency
	if concurrency <= 0 {
		concurrency = 8
	}
	work.Each(ctx, concurrency, apps, func(ctx context.Context, app state.App) {
		if err := a.pollOne(ctx, doc, app); err != nil {
			a.Logger.Warn("could not check for new commits",
				zap.String("app_id", app.ID), zap.Error(err))
		}
	})
}

func (a *AutoDeploy) pollOne(ctx context.Context, doc policy.Document, app state.App) error {
	// Skipped, not queued. A queue on a fast-moving branch produces a backlog
	// nobody wants, and the next poll picks up whatever is newest anyway.
	inFlight, err := a.Deployments.InFlight(ctx, app.ID)
	if err != nil || inFlight {
		return err
	}

	rev, found, err := a.Apps.RevisionByID(ctx, app.PinnedSpecID)
	if err != nil || !found {
		return err
	}

	// R-158: auto-deploy and approval do not combine. A spec that turns
	// auto-deploy on is refused while approval is required, so this is an
	// app that already auto-deployed when policy started requiring approval
	// for it. It stops; it does not queue a request per push, which is the
	// backlog nobody wants to read. The app's status says so to the console.
	if approval.BlocksAutoDeploy(doc, app.ID, rev.Body) {
		a.Logger.Info("auto-deploy skipped: this app's deploys need approval",
			zap.String("app_id", app.ID))
		return nil
	}

	head, err := a.Resolver.Resolve(ctx, rev.Body.Source)
	if err != nil {
		return err
	}
	if head == "" || head == rev.Body.Source.Commit {
		return nil
	}

	// A new revision carrying the new commit, and then the ordinary path. The
	// deploy itself never resolves a ref (design 01 §2.1) — this is the
	// explicit act that does it.
	next := *rev.Body
	next.Source.Commit = head

	created, err := a.Apps.CreateRevision(ctx, app.ID, &next, spec.OriginDetected, "auto-deploy")
	if err != nil {
		return err
	}

	dep, err := a.Deployments.Create(ctx, app.ID, created.ID,
		triggerFor(rev.Body.Deploy.AutoDeploy.Trigger), "auto-deploy")
	if err != nil {
		return err
	}

	a.Logger.Info("auto-deploy enqueued",
		zap.String("app_id", app.ID), zap.String("commit", head))

	if a.Enqueue != nil {
		a.Enqueue(ctx, dep, created)
	}
	return nil
}

func triggerFor(t spec.AutoDeployTrigger) string {
	if t == spec.TriggerReleaseTagged {
		return "release_tagged"
	}
	return "branch_updated"
}
