package reconciler

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/approval"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/work"
	"github.com/trypando/pando/internal/errs"
)

// Auto-deploy polling intervals. [P], design 05 §5.
const (
	BranchPollInterval  = 5 * time.Minute
	ReleasePollInterval = 15 * time.Minute
)

// How a check came about: the poll, or a webhook from the git host (R-142).
const (
	DeliveryPoll    = "poll"
	DeliveryWebhook = "webhook"
)

// AutoDeploy watches tracked refs and deploys what they point at (R-141).
//
// A separate scheduled job, deliberately not part of the reconciler. It never
// modifies a running app: it creates a spec revision carrying the new commit
// and deploys it through the same service a person's deploy goes through, so
// every plan-time check, the capacity hold and the audit event are the same.
// The reconciler converges what is pinned; this decides what should be pinned.
// Conflating them would make an unrelated drift correction able to ship new
// code.
//
// Off by default (R-141). Nothing here runs for an app that did not ask.
type AutoDeploy struct {
	Apps        *state.Apps
	Deployments *state.Deployments
	Checks      *state.AutoDeployChecks
	Resolver    RefResolver

	// Deployer starts a deploy as a person's would (approval.Service).
	Deployer Deployer

	// Audit records each auto-deploy, with the commit it moves from and to,
	// before anything is created (R-227, R-228).
	Audit AuditWriter

	Logger *zap.Logger

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

// RefResolver finds what an app's auto-deploy trigger points at now.
//
// Listing a remote's refs rather than cloning: this runs every few minutes for
// every app that auto-deploys, and cloning each time to learn a SHA that has
// usually not changed would be most of Pando's network traffic.
type RefResolver interface {
	Resolve(ctx context.Context, src spec.Source, ad spec.AutoDeploy) (source.Tracked, error)
}

// Deployer starts a deploy of rev on behalf of p.
type Deployer interface {
	Deploy(ctx context.Context, p authz.Principal, app state.App, rev state.Revision, trigger string) (state.Deployment, error)
}

// AuditWriter appends to the audit log.
type AuditWriter interface {
	Write(ctx context.Context, e audit.Event) error
}

// Run polls until the context is canceled: apps following a branch every
// BranchPollInterval, apps following releases every ReleasePollInterval.
// Releases are rarer than pushes, and a release check lists every tag.
func (a *AutoDeploy) Run(ctx context.Context) {
	branches := time.NewTicker(BranchPollInterval)
	defer branches.Stop()
	releases := time.NewTicker(ReleasePollInterval)
	defer releases.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-branches.C:
			a.poll(ctx, spec.TriggerBranchUpdated)
		case <-releases.C:
			a.poll(ctx, spec.TriggerReleaseTagged)
		}
	}
}

// Poll checks every app that auto-deploys, whichever trigger it uses.
func (a *AutoDeploy) Poll(ctx context.Context) { a.poll(ctx, "") }

// PollTrigger checks the apps that auto-deploy on trigger, and no others:
// what one tick of Run does.
func (a *AutoDeploy) PollTrigger(ctx context.Context, trigger spec.AutoDeployTrigger) {
	a.poll(ctx, trigger)
}

func (a *AutoDeploy) poll(ctx context.Context, only spec.AutoDeployTrigger) {
	apps, err := a.Apps.WithAutoDeploy(ctx)
	if err != nil {
		a.Logger.Warn("could not list apps that deploy automatically", zap.Error(err))
		return
	}

	var doc policy.Document
	if len(apps) > 0 {
		if doc, err = a.policy(ctx); err != nil {
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
		if err := a.check(ctx, doc, app, only, DeliveryPoll); err != nil {
			a.Logger.Warn("could not check for new commits",
				zap.String("app_id", app.ID), zap.Error(err))
		}
	})
}

func (a *AutoDeploy) policy(ctx context.Context) (policy.Document, error) {
	if a.Policy == nil {
		return policy.Document{}, nil
	}
	return a.Policy.Load(ctx)
}

// TriggerOf is the trigger an app's settings use; empty is the branch.
func TriggerOf(ad spec.AutoDeploy) spec.AutoDeployTrigger {
	if ad.Trigger == spec.TriggerReleaseTagged {
		return spec.TriggerReleaseTagged
	}
	return spec.TriggerBranchUpdated
}

func (a *AutoDeploy) check(ctx context.Context, doc policy.Document, app state.App, only spec.AutoDeployTrigger, delivery string) error {
	rev, found, err := a.Apps.RevisionByID(ctx, app.PinnedSpecID)
	if err != nil || !found {
		return err
	}
	ad := rev.Body.Deploy.AutoDeploy
	if !ad.Enabled || (only != "" && TriggerOf(ad) != only) {
		return nil
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

	// Skipped, not queued. A queue on a fast-moving branch produces a backlog
	// nobody wants, and the next check picks up whatever is newest anyway.
	inFlight, err := a.Deployments.InFlight(ctx, app.ID)
	if err != nil || inFlight {
		return err
	}

	tracked, err := a.Resolver.Resolve(ctx, rev.Body.Source, ad)
	if err != nil {
		a.recordCheck(ctx, app.ID, source.Tracked{}, message(err))
		return err
	}
	if tracked.Commit == "" {
		a.recordCheck(ctx, app.ID, tracked, nothingFound(rev.Body.Source, ad))
		return nil
	}
	if tracked.Commit == rev.Body.Source.Commit {
		a.recordCheck(ctx, app.ID, tracked, "")
		return nil
	}

	// O-56: a commit is tried once. A push that fails to deploy is not tried
	// again every five minutes; the next commit is, and a person can deploy
	// this one by hand. The reason it failed stays on the check.
	last, seen, err := a.Checks.ByApp(ctx, app.ID)
	if err != nil {
		return err
	}
	if seen && last.AttemptedCommit == tracked.Commit {
		a.recordCheck(ctx, app.ID, tracked, last.Error)
		return nil
	}

	return a.deploy(ctx, app, rev, ad, tracked, delivery)
}

// deploy cuts a revision carrying the new commit and deploys it. The deploy
// itself never resolves a ref (design 01 §2.1); this is the explicit act that
// does (R-120).
func (a *AutoDeploy) deploy(ctx context.Context, app state.App, rev state.Revision, ad spec.AutoDeploy, tracked source.Tracked, delivery string) error {
	trigger := string(TriggerOf(ad))

	// Written before anything is created (R-228), and refusing to go on when
	// it cannot be: an auto-deploy nobody can account for afterwards is worse
	// than one that waits for the next check.
	if err := a.Audit.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem,
		PrincipalID:   authz.System().ID,
		Action:        "app.auto_deploy",
		AppID:         app.ID,
		TargetKind:    "app",
		TargetID:      app.ID,
		Detail: map[string]any{
			"trigger":     trigger,
			"delivery":    delivery,
			"ref":         tracked.Ref,
			"from_commit": rev.Body.Source.Commit,
			"to_commit":   tracked.Commit,
		},
	}); err != nil {
		return err
	}

	next := *rev.Body
	next.Source.Commit = tracked.Commit
	switch {
	case TriggerOf(ad) == spec.TriggerReleaseTagged:
		next.Source.Ref = tracked.Ref
	case ad.Branch != "":
		next.Source.Ref = ad.Branch
	}

	created, err := a.Apps.CreateRevision(ctx, app.ID, &next, spec.OriginAutoDeploy, "auto-deploy")
	if err != nil {
		return err
	}

	dep, err := a.Deployer.Deploy(ctx, authz.System(), app, created, trigger)
	if err != nil {
		// Refused, most often at plan time. Recorded as tried, so this
		// commit is not tried again, with the reason for the console.
		a.recordAttempt(ctx, app.ID, tracked, "", message(err))
		a.Logger.Info("auto-deploy refused",
			zap.String("app_id", app.ID), zap.String("commit", tracked.Commit), zap.Error(err))
		return nil
	}
	a.recordAttempt(ctx, app.ID, tracked, dep.ID, "")
	a.Logger.Info("auto-deploy started",
		zap.String("app_id", app.ID), zap.String("commit", tracked.Commit),
		zap.String("deployment_id", dep.ID))
	return nil
}

// The record of a check is for people reading the console. Failing to write
// it is logged, and does not undo a deploy that has started.
func (a *AutoDeploy) recordCheck(ctx context.Context, appID string, t source.Tracked, msg string) {
	if err := a.Checks.Checked(ctx, appID, t.Ref, t.Commit, msg); err != nil {
		a.Logger.Warn("could not record the auto-deploy check", zap.String("app_id", appID), zap.Error(err))
	}
}

func (a *AutoDeploy) recordAttempt(ctx context.Context, appID string, t source.Tracked, depID, msg string) {
	if err := a.Checks.Attempted(ctx, appID, t.Ref, t.Commit, depID, msg); err != nil {
		a.Logger.Warn("could not record the auto-deploy attempt", zap.String("app_id", appID), zap.Error(err))
	}
}

func message(err error) string {
	if e := errs.As(err); e != nil {
		return e.Message
	}
	return "Pando could not check this app for new commits."
}

func nothingFound(src spec.Source, ad spec.AutoDeploy) string {
	if src.Type != spec.SourceGit {
		// Watching an image's tags is R-143, which is later.
		return "This app's source is not a git repository, so there is no branch or tag for automatic deploys to watch."
	}
	if TriggerOf(ad) == spec.TriggerReleaseTagged {
		if ad.TagPattern == "" {
			return "No tag in this repository counts as a release yet. A release is a tag such as v1.2.3, unless the app sets a tag pattern."
		}
		return "No tag in this repository matches the release pattern " + ad.TagPattern + "."
	}
	branch := ad.Branch
	if branch == "" {
		branch = src.Ref
	}
	if branch == "" {
		return "This app does not name a branch to follow. Set one in its automatic deploy settings."
	}
	return "The branch " + branch + " does not exist in this app's repository."
}
