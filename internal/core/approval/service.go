package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Authorizer is the part of authz.Authorizer approval asks. Allows and
// AllowsInstall check without auditing a denial; CheckControl audits one.
type Authorizer interface {
	CheckControl(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) error
	Allows(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) (bool, error)
	AllowsInstall(ctx context.Context, p authz.Principal, verb authz.Verb) (bool, error)
}

// PolicyLoader reads host policy as it is now. Read per call, never cached
// (R-274).
type PolicyLoader interface {
	Load(ctx context.Context) (policy.Document, error)
}

// Planner is the dry run a deploy passes before anything is created.
type Planner interface {
	Check(ctx context.Context, s *spec.AppSpec) (*planner.Plan, error)
}

// Capacity holds a runtime's capacity while fn runs (state.Allocations.Hold).
type Capacity interface {
	Hold(ctx context.Context, runtimeRef string, fn func(context.Context) error) error
}

// Starter runs a deployment that is pending, in the background.
type Starter interface {
	Start(ctx context.Context, dep state.Deployment, rev state.Revision)
}

// Notifier tells people something through the notification adapters.
type Notifier interface {
	Notify(ctx context.Context, n api.Notification) error
}

// Approvers names who may approve an app's deploys, for telling them.
type Approvers interface {
	DeployApprovers(ctx context.Context, appID string) ([]string, error)
}

// Service starts deploys, and asks for, records and acts on approval of the
// ones that need it (R-154 – R-159).
//
// Every way a deploy starts goes through here — a person's deploy, a
// rollback, and the last approval of a request — so that "does this need
// approval" has one answer, and a deploy that needed it cannot start any
// other way. The HTTP handlers and, through them, the CLI and MCP tools are
// clients of it (R-261).
type Service struct {
	Deployments *state.Deployments
	Apps        *state.Apps
	Authz       Authorizer
	Policy      PolicyLoader
	Planner     Planner
	Deployer    Starter

	// Capacity serializes a deploy's plan-time capacity check with its
	// creation, per runtime (R-242). Optional: without it the two are not
	// serialized, which is right only where nothing deploys concurrently.
	Capacity Capacity

	// Audit writes an event. Failing to write one is logged by whoever
	// supplies it, never turned into a failure of the action.
	Audit func(ctx context.Context, e audit.Event)

	// Notifier and Approvers are optional: without them nobody is told, and
	// everything else works (R-159's notification is best effort).
	Notifier  Notifier
	Approvers Approvers

	Clock  clock.Clock
	Logger *zap.Logger
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now()
}

func (s *Service) logger() *zap.Logger {
	if s.Logger == nil {
		return zap.NewNop()
	}
	return s.Logger
}

func (s *Service) audit(ctx context.Context, e audit.Event) {
	if s.Audit != nil {
		s.Audit(ctx, e)
	}
}

// decider is who a decision is recorded against: the person, when a token
// acts for one (R-058), so that approving once in the console and once
// through the CLI is one approval and not two.
func decider(p authz.Principal) string {
	if p.UserID != "" {
		return p.UserID
	}
	return p.ID
}

func principalEvent(p authz.Principal, action, appID, depID string, detail map[string]any) audit.Event {
	return audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        action,
		AppID:         appID,
		TargetKind:    "deployment",
		TargetID:      depID,
		Detail:        detail,
	}
}

// --- starting a deploy ------------------------------------------------------

// Deploy deploys rev, or, when the deploy needs approval, records a request
// for it and tells the people who can approve (R-154). The deployment comes
// back either way: pending and running, or awaiting_approval.
//
// The plan runs first, so a deploy that cannot succeed is refused here
// rather than part-way through — and is never put in front of an approver.
func (s *Service) Deploy(ctx context.Context, p authz.Principal, app state.App, rev state.Revision, trigger string) (state.Deployment, error) {
	// The plan and the deploy's creation hold the runtime's capacity together
	// (R-242): once created, the deploy reserves what it asks for, and the
	// next plan on this runtime counts it.
	var dep state.Deployment
	var requested bool
	err := s.hold(ctx, rev, func(ctx context.Context) error {
		if _, err := s.Planner.Check(ctx, rev.Body); err != nil {
			return err
		}
		doc, reasons, err := s.needs(ctx, app, rev, trigger)
		if err != nil {
			return err
		}
		if len(reasons) > 0 {
			requested = true
			dep, err = s.request(ctx, p, app, rev, trigger, doc, reasons)
			return err
		}
		dep, err = s.Deployments.Create(ctx, app.ID, rev.ID, trigger, p.ID)
		return err
	})
	if err != nil || requested {
		return dep, err
	}
	// A deploy that needed no approval still supersedes a request waiting
	// for one: approving the older request afterwards would put back a
	// revision nobody is asking for any more (R-156).
	s.supersede(ctx, p, app.ID, dep.ID)

	detail := map[string]any{"spec_revision": rev.Revision, "trigger": trigger}
	// A rollback says what it rolled back from as well as to (R-389): the
	// revision pinned now is the one running, until this deploy succeeds.
	if trigger == state.TriggerRollback && app.PinnedSpecID != "" {
		detail["rolled_back_from"] = app.PinnedSpecID
	}
	if err := s.launch(ctx, p, dep, rev, detail); err != nil {
		return state.Deployment{}, err
	}
	return s.read(ctx, dep), nil
}

// errPlanRefused ends a hold whose plan did not pass, which its caller answers
// from the plan's own error.
var errPlanRefused = errors.New("the plan did not pass")

// hold runs fn holding the capacity of the runtime rev runs on, when there is
// a Capacity to hold it with.
func (s *Service) hold(ctx context.Context, rev state.Revision, fn func(context.Context) error) error {
	if s.Capacity == nil || rev.Body == nil {
		return fn(ctx)
	}
	return s.Capacity.Hold(ctx, rev.Body.Runtime.AdapterRef, fn)
}

// read is the deployment as the store has it now, with what only a read
// fills in (the revision number, the requester's name), or dep itself if it
// cannot be read: the deploy has already been recorded and started, and
// failing the request over a display detail would invite a retry that
// deploys twice.
func (s *Service) read(ctx context.Context, dep state.Deployment) state.Deployment {
	current, found, err := s.Deployments.ByID(ctx, dep.ID)
	if err != nil || !found {
		return dep
	}
	describe(&current)
	return current
}

// launch is the tail every deploy start shares: the app is deploying, the
// deploy is audited, and the runner takes it from there in the background.
func (s *Service) launch(ctx context.Context, p authz.Principal, dep state.Deployment, rev state.Revision, detail map[string]any) error {
	// What a deploy that stops before touching the runtime puts back (R-146).
	app, found, err := s.Apps.ByID(ctx, dep.AppID)
	if err != nil {
		return err
	}
	if found && app.State != state.StateDeploying {
		if err := s.Deployments.RecordPriorState(ctx, dep.ID, app.State); err != nil {
			return err
		}
	}
	if err := s.Apps.SetState(ctx, dep.AppID, state.StateDeploying); err != nil {
		return err
	}
	s.audit(ctx, principalEvent(p, "app.deploy", dep.AppID, dep.ID, detail))
	s.Deployer.Start(ctx, dep, rev)
	return nil
}

// Needed is whether deploying rev over what the app runs now needs approval,
// and why: what the plan endpoint shows before anyone deploys.
func (s *Service) Needed(ctx context.Context, app state.App, rev state.Revision, trigger string) ([]Reason, error) {
	_, reasons, err := s.needs(ctx, app, rev, trigger)
	return reasons, err
}

// NeededFor is Needed for a spec that has not been saved: what deploying a
// draft would need, for a dry-run save to show before anything is written.
func (s *Service) NeededFor(ctx context.Context, app state.App, next *spec.AppSpec) ([]Reason, error) {
	_, reasons, err := s.needs(ctx, app, state.Revision{Body: next}, state.TriggerManual)
	return reasons, err
}

func (s *Service) needs(ctx context.Context, app state.App, rev state.Revision, trigger string) (policy.Document, []Reason, error) {
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return policy.Document{}, nil, err
	}

	// R-157: rolling back to a revision that already ran needs no approval.
	// It was approved, or did not need to be, when it first ran, and rollback
	// is how a bad deploy is undone quickly. "Ran" is a deploy of it that
	// succeeded, not that it was once pinned: a revision can be pinned by hand
	// without ever being deployed.
	if trigger == state.TriggerRollback {
		ran, err := s.Deployments.RanSuccessfully(ctx, app.ID, rev.ID)
		if err != nil {
			return doc, nil, err
		}
		if ran {
			return doc, nil, nil
		}
	}

	// What the app runs is its newest successful deploy, not its pinned spec.
	// A deploy normally deploys the pinned spec, so comparing against it would
	// compare a revision with itself: a new egress loosening would never look
	// new, and switching off deploy.require_approval would not be approved.
	var running *spec.AppSpec
	runningID, err := s.Deployments.RunningSpecID(ctx, app.ID)
	if err != nil {
		return doc, nil, err
	}
	if runningID != "" {
		ran, found, err := s.Apps.RevisionByID(ctx, runningID)
		if err != nil {
			return doc, nil, err
		}
		if found {
			running = ran.Body
		}
	}
	reasons := Reasons(doc, app.ID, running, rev.Body)

	// The app's own requirement also stands when it is in the pinned spec but
	// nothing has run it yet: the app asked for approval, and deploying a
	// revision that switches it off is still a deploy it asked about.
	if app.PinnedSpecID != "" && !hasReason(reasons, ReasonAppSpec) {
		pinned, found, err := s.Apps.RevisionByID(ctx, app.PinnedSpecID)
		if err != nil {
			return doc, nil, err
		}
		if found && pinned.Body.Deploy.RequireApproval {
			reasons = append(reasons, ReasonAppSpec)
		}
	}
	return doc, reasons, nil
}

// request records a deploy waiting for approval, supersedes any older one for
// the app, and tells the approvers.
func (s *Service) request(ctx context.Context, p authz.Principal, app state.App, rev state.Revision,
	trigger string, doc policy.Document, reasons []Reason) (state.Deployment, error) {
	// Fixed now, so a policy edit does not move a waiting request's
	// goalposts (R-156).
	var expires *time.Time
	if doc.DeployApprovalExpiryHours > 0 {
		at := s.now().Add(time.Duration(doc.DeployApprovalExpiryHours) * time.Hour)
		expires = &at
	}
	codes := make([]string, 0, len(reasons))
	for _, r := range reasons {
		codes = append(codes, string(r))
	}

	dep, err := s.Deployments.CreateAwaiting(ctx, app.ID, rev.ID, trigger, p.ID, doc.ApprovalsNeeded(), expires, codes)
	if err != nil {
		return state.Deployment{}, err
	}
	s.supersede(ctx, p, app.ID, dep.ID)

	detail := map[string]any{
		"spec_revision":      rev.Revision,
		"trigger":            trigger,
		"reasons":            codes,
		"approvals_required": dep.ApprovalsRequired,
	}
	if expires != nil {
		detail["expires_at"] = expires.UTC().Format(time.RFC3339)
	}
	s.audit(ctx, principalEvent(p, "deploy.request", app.ID, dep.ID, detail))

	s.tellApprovers(ctx, p, app, dep, rev, reasons)
	return s.read(ctx, dep), nil
}

// supersede ends every older request for the app that is still waiting, and
// records each (R-159).
func (s *Service) supersede(ctx context.Context, p authz.Principal, appID, newer string) {
	ended, err := s.Deployments.SupersedeAwaiting(ctx, appID, newer)
	if err != nil {
		// Logged, not returned: the newer deploy is already recorded, and an
		// older request left waiting is still answerable — approving it
		// re-plans, and a deploy in flight refuses it.
		s.logger().Warn("could not supersede waiting deploy requests",
			zap.String("app_id", appID), zap.Error(err))
		return
	}
	for _, depID := range ended {
		s.audit(ctx, principalEvent(p, "deploy.supersede", appID, depID,
			map[string]any{"superseded_by": newer}))
	}
}

func (s *Service) tellApprovers(ctx context.Context, p authz.Principal, app state.App, dep state.Deployment, rev state.Revision, reasons []Reason) {
	if s.Notifier == nil || s.Approvers == nil {
		return
	}
	people, err := s.Approvers.DeployApprovers(ctx, app.ID)
	if err != nil {
		s.logger().Warn("could not list who may approve a deploy", zap.String("app_id", app.ID), zap.Error(err))
		return
	}
	if len(people) == 0 {
		// Nobody to tell is not a reason to refuse the request: an
		// installation may grant the verb tomorrow. Logged, so somebody
		// looking for why a request sits unanswered can find out.
		s.logger().Info("a deploy is waiting for approval and nobody holds a verb that approves it",
			zap.String("app_id", app.ID), zap.String("deployment_id", dep.ID))
		return
	}

	recipients := make([]api.Recipient, 0, len(people))
	for _, userID := range people {
		recipients = append(recipients, api.Recipient{UserID: userID})
	}

	who := p.DisplayName
	if who == "" {
		who = p.ID
	}
	lines := make([]string, 0, len(reasons))
	for _, r := range reasons {
		lines = append(lines, r.Message())
	}
	body := fmt.Sprintf("%s asked to deploy revision %d of %s. It needs %d approval(s) before it runs.\n\n%s\n\n"+
		"Approve or reject it from the app's Deploys page, or with `pando approvals approve %s %s`.",
		who, rev.Revision, app.Name, dep.ApprovalsRequired, strings.Join(lines, "\n"), app.ID, dep.ID)

	s.notify(ctx, api.Notification{
		Kind:       api.NotifyDeployApproval,
		AppID:      app.ID,
		Recipients: recipients,
		Subject:    "A deploy of " + app.Name + " is waiting for approval",
		Body:       body,
	})
}

// tellRequester tells whoever asked for a deploy how the request ended, when
// that is a person.
func (s *Service) tellRequester(ctx context.Context, dep state.Deployment, subject, body string) {
	if s.Notifier == nil || !id.Is(id.User, dep.CreatedBy) {
		return
	}
	s.notify(ctx, api.Notification{
		Kind:       api.NotifyDeployApproval,
		AppID:      dep.AppID,
		Recipients: []api.Recipient{{UserID: dep.CreatedBy}},
		Subject:    subject,
		Body:       body,
	})
}

// notify is best effort (R-159): a notification that does not go out never
// blocks or undoes the thing it was about.
func (s *Service) notify(ctx context.Context, n api.Notification) {
	if err := s.Notifier.Notify(ctx, n); err != nil {
		s.logger().Warn("could not send a deploy approval notification",
			zap.String("app_id", n.AppID), zap.Error(err))
	}
}

// --- deciding ---------------------------------------------------------------

// Approve records p's approval of a deploy waiting for one, and starts it
// when that is the last approval it needs (R-156).
//
// The plan runs again first, because policy may have changed while the
// request waited. A plan that now fails ends the request as failed with the
// plan's own error, rather than leaving it waiting: no number of further
// approvals can make it pass, and the fix — changing the spec — is a new
// revision and so a new request. A failure to plan at all (an adapter that
// cannot be reached, an internal error) is not an answer about the revision,
// so the request stays waiting and nothing is recorded.
//
// An approval that would start the deploy while another deploy of the app is
// in flight is refused, and the request keeps waiting: two deploys racing on
// one bundle is how an app ends up in a state neither intended.
func (s *Service) Approve(ctx context.Context, p authz.Principal, appID, depID, comment string) (state.Deployment, error) {
	dep, err := s.waiting(ctx, appID, depID)
	if err != nil {
		return state.Deployment{}, err
	}
	if err := s.mayDecide(ctx, p, appID); err != nil {
		return state.Deployment{}, err
	}

	me := decider(p)
	approvals := 1
	for _, d := range dep.Approvals {
		if d.Decision == state.DecisionApprove && d.PrincipalID != me {
			approvals++
		}
	}
	detail := map[string]any{"approvals": approvals, "approvals_required": dep.ApprovalsRequired}
	if comment != "" {
		detail["comment"] = comment
	}

	if approvals < dep.ApprovalsRequired {
		if err := s.Deployments.Decide(ctx, dep.ID, me, state.DecisionApprove, comment); err != nil {
			return state.Deployment{}, err
		}
		s.audit(ctx, principalEvent(p, "deploy.approve", appID, dep.ID, detail))
		return s.reload(ctx, p, dep.ID)
	}

	// The last approval it needs: this one starts it.
	inFlight, err := s.Deployments.InFlight(ctx, appID)
	if err != nil {
		return state.Deployment{}, err
	}
	if inFlight {
		return state.Deployment{}, deployingNow()
	}

	rev, found, err := s.Apps.RevisionByID(ctx, dep.SpecID)
	if err != nil {
		return state.Deployment{}, err
	}
	if !found {
		return state.Deployment{}, errs.New(errs.NotFound, "The spec revision this deploy was asked for is missing.")
	}

	// The plan and the start hold the runtime's capacity together, as a
	// deploy's plan and creation do (R-242): started, the deploy reserves
	// what it asks for.
	var planErr error
	var started bool
	err = s.hold(ctx, rev, func(ctx context.Context) error {
		_, planErr = s.Planner.Check(ctx, rev.Body)
		if planErr != nil {
			// Not the hold's failure: the request is answered below, and
			// nothing is started.
			return errPlanRefused
		}
		if err := s.Deployments.Decide(ctx, dep.ID, me, state.DecisionApprove, comment); err != nil {
			return err
		}
		var err error
		started, err = s.Deployments.StartApproved(ctx, dep.ID)
		return err
	})
	if err != nil && !errors.Is(err, errPlanRefused) {
		return state.Deployment{}, err
	}

	if planErr != nil {
		if !answersTheRevision(planErr) {
			return state.Deployment{}, planErr
		}
		if err := s.Deployments.Decide(ctx, dep.ID, me, state.DecisionApprove, comment); err != nil {
			return state.Deployment{}, err
		}
		message := "The deploy was approved, but its plan no longer passes, so it did not run."
		if e := errs.As(planErr); e != nil {
			message = e.Message
		}
		if _, err := s.Deployments.EndAwaiting(ctx, dep.ID, state.DeployFailed, string(errs.CodeOf(planErr)), message); err != nil {
			return state.Deployment{}, err
		}
		detail["plan_failed"] = string(errs.CodeOf(planErr))
		s.audit(ctx, principalEvent(p, "deploy.approve", appID, dep.ID, detail))
		s.tellRequester(ctx, dep, "A deploy you asked for was approved and could not run",
			"It was approved, but its plan no longer passes: "+message)
		if e := errs.As(planErr); e != nil {
			return state.Deployment{}, e.WithDetail("deployment_id", dep.ID).WithDetail("deployment_status", state.DeployFailed)
		}
		return state.Deployment{}, planErr
	}

	if !started {
		// Something got there first: another approval started it, it was
		// rejected or superseded, or a deploy of the app began between the
		// check above and now. The approval is recorded either way.
		current, err := s.reload(ctx, p, dep.ID)
		if err != nil {
			return state.Deployment{}, err
		}
		s.audit(ctx, principalEvent(p, "deploy.approve", appID, dep.ID, detail))
		if current.Status == state.DeployAwaitingApproval {
			return state.Deployment{}, deployingNow()
		}
		return current, nil
	}

	detail["started"] = true
	s.audit(ctx, principalEvent(p, "deploy.approve", appID, dep.ID, detail))

	dep.Status = state.DeployPending
	if err := s.launch(ctx, p, dep, rev, map[string]any{
		"spec_revision": rev.Revision,
		"trigger":       dep.Trigger,
		"requested_by":  dep.CreatedBy,
		"approved":      true,
	}); err != nil {
		return state.Deployment{}, err
	}
	s.tellRequester(ctx, dep, "A deploy you asked for was approved",
		fmt.Sprintf("Revision %d is deploying now.", rev.Revision))
	return s.reload(ctx, p, dep.ID)
}

// Reject ends a deploy waiting for approval (R-156: a rejection by any
// approver ends the request).
func (s *Service) Reject(ctx context.Context, p authz.Principal, appID, depID, comment string) (state.Deployment, error) {
	dep, err := s.waiting(ctx, appID, depID)
	if err != nil {
		return state.Deployment{}, err
	}
	if err := s.mayDecide(ctx, p, appID); err != nil {
		return state.Deployment{}, err
	}

	if err := s.Deployments.Decide(ctx, dep.ID, decider(p), state.DecisionReject, comment); err != nil {
		return state.Deployment{}, err
	}
	ended, err := s.Deployments.EndAwaiting(ctx, dep.ID, state.DeployRejected, "",
		"This deploy was rejected, so it did not run.")
	if err != nil {
		return state.Deployment{}, err
	}
	if !ended {
		current, err := s.reload(ctx, p, dep.ID)
		if err != nil {
			return state.Deployment{}, err
		}
		return state.Deployment{}, notWaiting(current)
	}

	detail := map[string]any{}
	if comment != "" {
		detail["comment"] = comment
	}
	s.audit(ctx, principalEvent(p, "deploy.reject", appID, dep.ID, detail))

	body := "It did not run."
	if comment != "" {
		body = "It did not run. The comment: " + comment
	}
	s.tellRequester(ctx, dep, "A deploy you asked for was rejected", body)
	return s.reload(ctx, p, dep.ID)
}

// waiting returns the deploy if it is still waiting for approval, and
// otherwise an error that says what became of it. A request whose wait has
// run out is expired here, rather than left for the next sweep to notice.
func (s *Service) waiting(ctx context.Context, appID, depID string) (state.Deployment, error) {
	dep, found, err := s.Deployments.ByID(ctx, depID)
	if err != nil {
		return state.Deployment{}, err
	}
	if !found || dep.AppID != appID {
		return state.Deployment{}, errs.New(errs.NotFound, "There is no such deploy for this app.")
	}
	if dep.Status != state.DeployAwaitingApproval {
		return state.Deployment{}, notWaiting(dep)
	}
	if dep.ApprovalExpiresAt != nil && !dep.ApprovalExpiresAt.After(s.now()) {
		ended, err := s.Deployments.EndAwaiting(ctx, dep.ID, state.DeployExpired, string(errs.StateInvalid),
			"Nobody approved this deploy before its request expired. Deploy again to ask again.")
		if err != nil {
			return state.Deployment{}, err
		}
		if ended {
			s.recordExpiry(ctx, dep)
		}
		return state.Deployment{}, errs.Newf(errs.StateInvalid,
			"This deploy's request expired at %s, before it had its approvals.",
			dep.ApprovalExpiresAt.UTC().Format(time.RFC3339)).
			WithRemedy("Deploy the app again to make a new request.")
	}
	return dep, nil
}

func notWaiting(dep state.Deployment) error {
	what := map[string]string{
		state.DeployPending:    "it has been approved and is starting",
		state.DeployBuilding:   "it has been approved and is building",
		state.DeployApplying:   "it has been approved and is being applied",
		state.DeploySucceeded:  "it has already run",
		state.DeployFailed:     "it has already run, or tried to, and failed",
		state.DeploySuperseded: "a newer deploy of this app replaced it",
		state.DeployRejected:   "it was rejected",
		state.DeployExpired:    "its request expired",
	}[dep.Status]
	if what == "" {
		what = "it is " + dep.Status
	}
	e := errs.Newf(errs.StateInvalid, "This deploy is not waiting for approval: %s.", what).
		WithDetail("status", dep.Status)
	if dep.Status == state.DeploySuperseded || dep.Status == state.DeployExpired {
		e = e.WithRemedy("Look for the app's newest deploy request, or deploy again to make one.")
	}
	return e
}

func deployingNow() error {
	return errs.New(errs.StateInvalid,
		"This app is deploying right now, so this deploy cannot start yet. The request is still waiting.").
		WithRemedy("Approve it again once the current deploy has finished.")
}

// answersTheRevision reports whether a plan failure is a verdict on the
// revision — something more approvals could not change — rather than a
// failure to plan at all.
func answersTheRevision(err error) bool {
	code := string(errs.CodeOf(err))
	for _, prefix := range []string{"PLAN_", "POLICY_", "VALID_", "CAPACITY_"} {
		if strings.HasPrefix(code, prefix) {
			return true
		}
	}
	return false
}

// mayDecide is R-155: install.deploys.approve, or app.deploy.approve on this
// app. Either approves its holder's own request.
//
// Checked without auditing first, so that somebody allowed by the second verb
// does not leave a denial of the first in the audit log. Only when neither
// allows is the per-app check made again through the audited path, which
// records the one denial that happened.
func (s *Service) mayDecide(ctx context.Context, p authz.Principal, appID string) error {
	ok, err := s.CanDecide(ctx, p, appID)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if err := s.Authz.CheckControl(ctx, p, appID, authz.AppDeployApprove); err != nil {
		if errs.CodeOf(err) == errs.PermVerbRequired {
			return errs.Newf(errs.PermVerbRequired,
				"You do not have permission to approve or reject this app's deploys. It takes %s, or %s on this app.",
				authz.InstallDeploysApprove, authz.AppDeployApprove).
				WithRemedy("Ask an administrator to approve it, or to give you a role that holds one of those permissions.")
		}
		return err
	}
	return nil
}

// CanDecide reports whether p may approve or reject the app's deploys,
// without auditing anything: what the console shows, not an attempt.
func (s *Service) CanDecide(ctx context.Context, p authz.Principal, appID string) (bool, error) {
	if p.Kind == authz.KindAnonymous {
		return false, nil
	}
	ok, err := s.Authz.AllowsInstall(ctx, p, authz.InstallDeploysApprove)
	if err != nil || ok {
		return ok, err
	}
	return s.Authz.Allows(ctx, p, appID, authz.AppDeployApprove)
}

func (s *Service) reload(ctx context.Context, p authz.Principal, depID string) (state.Deployment, error) {
	dep, found, err := s.Deployments.ByID(ctx, depID)
	if err != nil {
		return state.Deployment{}, err
	}
	if !found {
		return state.Deployment{}, errs.New(errs.NotFound, "There is no such deploy for this app.")
	}
	deps := []state.Deployment{dep}
	if err := s.Describe(ctx, p, deps); err != nil {
		return state.Deployment{}, err
	}
	return deps[0], nil
}

// --- reading ----------------------------------------------------------------

// Describe fills in what the store leaves to the service: what each approval
// reason means, and whether p may decide each deploy that is waiting.
func (s *Service) Describe(ctx context.Context, p authz.Principal, deps []state.Deployment) error {
	decides := map[string]bool{}
	for i := range deps {
		describe(&deps[i])
		if deps[i].Status != state.DeployAwaitingApproval {
			continue
		}
		ok, seen := decides[deps[i].AppID]
		if !seen {
			var err error
			if ok, err = s.CanDecide(ctx, p, deps[i].AppID); err != nil {
				return err
			}
			decides[deps[i].AppID] = ok
		}
		deps[i].CanDecide = ok
	}
	return nil
}

func describe(dep *state.Deployment) {
	for i := range dep.ApprovalReasons {
		dep.ApprovalReasons[i].Message = Reason(dep.ApprovalReasons[i].Reason).Message()
	}
}

// awaitingScanLimit bounds how many waiting requests one page reads while
// looking for ones p may view. Past it the page comes back short, with a
// cursor to carry on from: a request from somebody who can see few of many
// waiting deploys costs a bounded number of authorization checks, not one per
// waiting deploy in the install (issue #72).
var awaitingScanLimit = 4 * state.MaxPageSize

// Awaiting lists one page of the deploys waiting for approval on every app p
// may view, oldest first, each saying whether p may decide it, and the cursor
// for the next page (empty after the last). A page may hold fewer than its
// limit and still have a cursor, when the requests read were on apps p cannot
// see.
func (s *Service) Awaiting(ctx context.Context, p authz.Principal, page state.Page) ([]state.AwaitingApproval, string, error) {
	after, err := state.AwaitingKeyFrom(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	size := page.Size()
	now := s.now()
	out := make([]state.AwaitingApproval, 0)
	views := map[string]bool{}
	scanned := 0
	for {
		batch, err := s.Deployments.ListAwaitingAfter(ctx, after, size)
		if err != nil {
			return nil, "", err
		}
		for _, item := range batch {
			if len(out) == size || scanned >= awaitingScanLimit {
				// Stopped before this row: the next page starts after the
				// last one read.
				return out, after.Cursor(), nil
			}
			scanned++
			after = state.AwaitingKey{StartedAt: item.StartedAt, ID: item.ID}
			ok, err := s.awaitingVisible(ctx, p, item, now, views)
			if err != nil {
				return nil, "", err
			}
			if !ok {
				continue
			}
			deps := []state.Deployment{item.Deployment}
			if err := s.Describe(ctx, p, deps); err != nil {
				return nil, "", err
			}
			item.Deployment = deps[0]
			out = append(out, item)
		}
		if len(batch) < size {
			return out, "", nil // read to the end
		}
	}
}

// awaitingVisible reports whether a waiting request belongs on p's list: not
// past its expiry, and on an app p may view. views memoizes the view check by
// app within one request only — never across requests (R-274).
func (s *Service) awaitingVisible(ctx context.Context, p authz.Principal, item state.AwaitingApproval,
	now time.Time, views map[string]bool) (bool, error) {
	// Past its expiry and not yet swept: it is not waiting any more, whatever
	// the row says.
	if item.ApprovalExpiresAt != nil && !item.ApprovalExpiresAt.After(now) {
		return false, nil
	}
	ok, seen := views[item.AppID]
	if !seen {
		var err error
		if ok, err = s.Authz.Allows(ctx, p, item.AppID, authz.AppView); err != nil {
			return false, err
		}
		views[item.AppID] = ok
	}
	return ok, nil
}

// AutoDeployPaused reports whether an app's pinned spec asks for auto-deploy
// and approval now stops it (R-158): what the console says on the app.
func (s *Service) AutoDeployPaused(ctx context.Context, appID string, pinned *spec.AppSpec) (bool, error) {
	if pinned == nil || !pinned.Deploy.AutoDeploy.Enabled {
		return false, nil
	}
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return false, err
	}
	return BlocksAutoDeploy(doc, appID, pinned), nil
}

// --- expiry -----------------------------------------------------------------

// ExpireDue ends every request whose wait has run out, and records each
// (R-159). Run on a timer; Approve and Reject also expire the one they are
// asked about, so a request is never approved after its time.
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	expired, err := s.Deployments.ExpireDue(ctx, s.now())
	if err != nil {
		return 0, err
	}
	for _, dep := range expired {
		s.recordExpiry(ctx, dep)
	}
	return len(expired), nil
}

func (s *Service) recordExpiry(ctx context.Context, dep state.Deployment) {
	detail := map[string]any{}
	if dep.ApprovalExpiresAt != nil {
		detail["expired_at"] = dep.ApprovalExpiresAt.UTC().Format(time.RFC3339)
	}
	s.audit(ctx, audit.Event{
		PrincipalKind: audit.KindSystem,
		PrincipalID:   "system",
		Action:        "deploy.expire",
		AppID:         dep.AppID,
		TargetKind:    "deployment",
		TargetID:      dep.ID,
		Detail:        detail,
	})
	s.tellRequester(ctx, dep, "A deploy you asked for expired",
		"Nobody approved it before its request expired, so it did not run. Deploy again to ask again.")
}

// RunExpiry expires due requests every interval until ctx ends.
func (s *Service) RunExpiry(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := s.ExpireDue(ctx); err != nil {
				s.logger().Warn("could not expire deploy requests", zap.Error(err))
			} else if n > 0 {
				s.logger().Info("expired deploy requests nobody answered", zap.Int("count", n))
			}
		}
	}
}

func hasReason(reasons []Reason, want Reason) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
