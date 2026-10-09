//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/planner"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// Deploy approval (R-154 – R-159), end to end through the API.
//
// The harness has no runtime to plan or deploy onto, so the approval
// service's planner passes (or fails, when a test says so) and its deployer
// records what it was asked to start instead of starting it. Everything else
// — the store, the authorizer, host policy, the audit log — is real.

type stubPlanner struct {
	mu   sync.Mutex
	fail error
}

func (p *stubPlanner) Check(context.Context, *spec.AppSpec) (*planner.Plan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return nil, p.fail
	}
	return &planner.Plan{}, nil
}

func (p *stubPlanner) failWith(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = err
}

type recordingStarter struct {
	mu      sync.Mutex
	started []string
}

func (s *recordingStarter) Start(_ context.Context, dep state.Deployment, _ state.Revision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, dep.ID)
}

func (s *recordingStarter) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.started...)
}

type recordingNotifier struct {
	mu   sync.Mutex
	sent []adapterapi.Notification
}

func (n *recordingNotifier) Notify(_ context.Context, msg adapterapi.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, msg)
	return nil
}

func (n *recordingNotifier) all() []adapterapi.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]adapterapi.Notification(nil), n.sent...)
}

// approvals is an installation with the approval service's seams replaced.
type approvals struct {
	*install
	plans    *stubPlanner
	starts   *recordingStarter
	notified *recordingNotifier
}

func newApprovals(t *testing.T) *approvals {
	t.Helper()
	i := newInstall(t)
	a := &approvals{install: i, plans: &stubPlanner{}, starts: &recordingStarter{}, notified: &recordingNotifier{}}
	i.Server.Approvals.Planner = a.plans
	i.Server.Approvals.Deployer = a.starts
	i.Server.Approvals.Notifier = a.notified
	return a
}

// policy edits host policy, starting from what is stored.
func (a *approvals) policy(edit func(*corepolicy.Document)) {
	a.t.Helper()
	ctx := context.Background()
	doc, err := a.Server.PolicyStore.Load(ctx)
	require.NoError(a.t, err)
	edit(&doc)
	require.NoError(a.t, a.Server.PolicyStore.Save(ctx, doc, a.AdminID))
}

// owner makes a user who owns appID (the built-in Owner role, granted).
func (a *approvals) owner(admin *session, appID, username string) *session {
	a.t.Helper()
	s := a.user(username)
	got := a.do(admin, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": a.userID(s), "role_id": "role_owner",
	})
	require.Equal(a.t, http.StatusCreated, got.Code, got.String())
	return s
}

// reviewer makes a user holding app.deploy.approve on appID through a custom
// role, which is how an installation grants it (R-155: no built-in role has it).
func (a *approvals) reviewer(admin *session, appID, username string) *session {
	a.t.Helper()
	s := a.user(username)
	roleID := a.customRole(admin, "deploy reviewer "+username, "app", "app.view", "app.deploy.approve")
	got := a.do(admin, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": a.userID(s), "role_id": roleID,
	})
	require.Equal(a.t, http.StatusCreated, got.Code, got.String())
	return s
}

type deployView struct {
	ID                string     `json:"id"`
	AppID             string     `json:"app_id"`
	SpecID            string     `json:"spec_id"`
	SpecRevision      int        `json:"spec_revision"`
	Status            string     `json:"status"`
	Trigger           string     `json:"trigger"`
	CreatedBy         string     `json:"created_by"`
	RequestedByName   string     `json:"requested_by_name"`
	ErrorCode         string     `json:"error_code"`
	ApprovalsRequired int        `json:"approvals_required"`
	ApprovalExpiresAt *time.Time `json:"approval_expires_at"`
	ApprovalReasons   []struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"approval_reasons"`
	Approvals []struct {
		PrincipalID   string `json:"principal_id"`
		PrincipalName string `json:"principal_name"`
		Decision      string `json:"decision"`
		Comment       string `json:"comment"`
	} `json:"approvals"`
	CanDecide bool   `json:"can_decide"`
	AppName   string `json:"app_name"`
	AppSlug   string `json:"app_slug"`
}

func (a *approvals) deploy(s *session, appID string) deployView {
	a.t.Helper()
	got := a.do(s, http.MethodPost, "/apps/"+appID+"/deployments", map[string]any{})
	require.Equal(a.t, http.StatusAccepted, got.Code, got.String())
	var dep deployView
	got.JSON(a.t, &dep)
	require.NotEmpty(a.t, dep.ID)
	return dep
}

func (a *approvals) decide(s *session, appID, depID, action string, body map[string]any) reply {
	a.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	return a.do(s, http.MethodPost, "/apps/"+appID+"/deployments/"+depID+"/"+action, body)
}

func (a *approvals) deployment(s *session, appID, depID string) deployView {
	a.t.Helper()
	got := a.do(s, http.MethodGet, "/apps/"+appID+"/deployments/"+depID, nil)
	require.Equal(a.t, http.StatusOK, got.Code, got.String())
	var dep deployView
	got.JSON(a.t, &dep)
	return dep
}

func (a *approvals) waiting(s *session) []deployView {
	a.t.Helper()
	got := a.do(s, http.MethodGet, "/approvals", nil)
	require.Equal(a.t, http.StatusOK, got.Code, got.String())
	var out struct {
		Approvals []deployView `json:"approvals"`
	}
	got.JSON(a.t, &out)
	require.NotNil(a.t, out.Approvals, "an empty list is [], never null: %s", got.String())
	return out.Approvals
}

func (a *approvals) appState(s *session, appID string) string {
	a.t.Helper()
	var app struct {
		State string `json:"state"`
	}
	a.do(s, http.MethodGet, "/apps/"+appID, nil).JSON(a.t, &app)
	return app.State
}

func (a *approvals) auditActions(appID string) map[string]int {
	a.t.Helper()
	records, err := a.Server.AuditLog.List(context.Background(), audit.Query{AppID: appID, Limit: 500})
	require.NoError(a.t, err)
	out := map[string]int{}
	for _, r := range records {
		out[r.Action]++
	}
	return out
}

// TestR154_ADeployNeedsNoApprovalUnlessSomethingAsksForIt asserts R-154's
// "off by default, adds nothing": an ordinary deploy starts at once.
func TestR154_ADeployNeedsNoApprovalUnlessSomethingAsksForIt(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.appWithSpec(admin, "notes")

	dep := a.deploy(admin, appID)
	require.Equal(t, state.DeployPending, dep.Status)
	require.Zero(t, dep.ApprovalsRequired)
	require.Equal(t, []string{dep.ID}, a.starts.ids())
	require.Equal(t, state.StateDeploying, a.appState(admin, appID))
	require.Empty(t, a.waiting(admin))
}

// TestR154_ADeployThatNeedsApprovalWaitsForIt asserts that a deploy policy
// requires approval of is recorded waiting, says why, leaves the app alone,
// and tells the people who can approve it (R-159).
func TestR154_ADeployThatNeedsApprovalWaitsForIt(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.appWithSpec(admin, "notes")
	dev := a.owner(admin, appID, "dev")
	before := a.appState(admin, appID)

	a.policy(func(d *corepolicy.Document) {
		d.DeployApprovalApps = []string{appID}
		// Set rather than inherited: the policy row migration 000005 seeds
		// predates the field, so an install reads zero — never — until
		// somebody sets it. Flagged in the change's report.
		d.DeployApprovalExpiryHours = corepolicy.DefaultDeployApprovalExpiryHours
	})

	dep := a.deploy(dev, appID)
	require.Equal(t, state.DeployAwaitingApproval, dep.Status)
	require.Equal(t, 1, dep.ApprovalsRequired)
	require.Equal(t, 1, dep.SpecRevision)
	require.Equal(t, "dev", dep.RequestedByName)
	require.Len(t, dep.ApprovalReasons, 1)
	require.Equal(t, "app_policy", dep.ApprovalReasons[0].Reason)
	require.Equal(t, "This installation requires approval for this app's deploys.", dep.ApprovalReasons[0].Message)
	require.NotNil(t, dep.ApprovalExpiresAt, "policy's default expiry is seven days (R-156)")
	require.WithinDuration(t, time.Now().Add(7*24*time.Hour), *dep.ApprovalExpiresAt, time.Minute)
	require.False(t, dep.CanDecide, "an owner does not hold an approving verb (R-155)")

	require.Empty(t, a.starts.ids(), "nothing starts until it is approved")
	require.Equal(t, before, a.appState(admin, appID), "the app is not deploying while it waits")

	// The administrator sees it waiting, and may decide it.
	listed := a.waiting(admin)
	require.Len(t, listed, 1)
	require.Equal(t, dep.ID, listed[0].ID)
	require.Equal(t, "notes", listed[0].AppName)
	require.NotEmpty(t, listed[0].AppSlug)
	require.True(t, listed[0].CanDecide)

	// The owner sees it too, and may not; somebody with no access to the app
	// sees nothing, and is answered with an empty list rather than refused.
	listed = a.waiting(dev)
	require.Len(t, listed, 1)
	require.False(t, listed[0].CanDecide)
	require.Empty(t, a.waiting(a.user("stranger")))

	// The administrator holds install.deploys.approve, so is told.
	sent := a.notified.all()
	require.Len(t, sent, 1)
	require.Equal(t, adapterapi.NotifyDeployApproval, sent[0].Kind)
	require.Contains(t, sent[0].Recipients, adapterapi.Recipient{UserID: a.AdminID})
	require.Contains(t, sent[0].Body, "pando approvals approve "+appID+" "+dep.ID)
}

// TestR154_TheAppCanRequireApprovalOfItselfAndTurningThatOffIsApproved asserts
// that deploy.require_approval is read from the running spec as well as the
// next, so switching it off is itself a deploy that needs approval.
func TestR154_TheAppCanRequireApprovalOfItselfAndTurningThatOffIsApproved(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.createApp(admin, "notes")

	strict := minimalSpec()
	strict["deploy"] = map[string]any{"strategy": "recreate", "require_approval": true}
	a.pinSpec(admin, appID, a.writeSpec(admin, appID, strict))

	dep := a.deploy(admin, appID)
	require.Equal(t, state.DeployAwaitingApproval, dep.Status)
	require.Equal(t, "app_spec", dep.ApprovalReasons[0].Reason)

	// The next revision turns it off; the running one still asks.
	a.writeSpec(admin, appID, minimalSpec())
	got := a.do(admin, http.MethodPost, "/apps/"+appID+"/deployments", map[string]any{"spec_revision": 2})
	require.Equal(t, http.StatusAccepted, got.Code, got.String())
	var next deployView
	got.JSON(t, &next)
	require.Equal(t, state.DeployAwaitingApproval, next.Status)
}

// TestR155_EitherVerbApprovesAndSelfApprovalIsAllowed asserts the two verbs:
// install.deploys.approve approves anything, including its holder's own
// request; app.deploy.approve approves that app's.
func TestR155_EitherVerbApprovesAndSelfApprovalIsAllowed(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.appWithSpec(admin, "notes")
	dev := a.owner(admin, appID, "dev")
	reviewer := a.reviewer(admin, appID, "reviewer")
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalRequired = true })

	// The administrator approves their own request (R-155).
	own := a.deploy(admin, appID)
	got := a.decide(admin, appID, own.ID, "approve", map[string]any{"comment": "mine, and fine"})
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var approved deployView
	got.JSON(t, &approved)
	require.Equal(t, state.DeployPending, approved.Status)
	require.Len(t, approved.Approvals, 1)
	require.Equal(t, "approve", approved.Approvals[0].Decision)
	require.Equal(t, "mine, and fine", approved.Approvals[0].Comment)
	require.NotEmpty(t, approved.Approvals[0].PrincipalName)
	require.Equal(t, []string{own.ID}, a.starts.ids())
	require.Equal(t, state.StateDeploying, a.appState(admin, appID))

	// Let that one finish, so the next can start.
	require.NoError(t, a.Server.Deployments.Finish(context.Background(), own.ID, state.DeploySucceeded, "", ""))

	// app.deploy.approve on the app approves the owner's request.
	theirs := a.deploy(dev, appID)
	require.True(t, a.deployment(reviewer, appID, theirs.ID).CanDecide)

	// Both kinds of approver are told; the owner, who cannot approve, is not.
	sent := a.notified.all()
	told := sent[len(sent)-1].Recipients
	require.Contains(t, told, adapterapi.Recipient{UserID: a.AdminID})
	require.Contains(t, told, adapterapi.Recipient{UserID: a.userID(reviewer)})
	require.NotContains(t, told, adapterapi.Recipient{UserID: a.userID(dev)})
	got = a.decide(reviewer, appID, theirs.ID, "approve", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.Equal(t, []string{own.ID, theirs.ID}, a.starts.ids())
	require.NoError(t, a.Server.Deployments.Finish(context.Background(), theirs.ID, state.DeploySucceeded, "", ""))

	// And its holder may approve their own request too.
	roleID := a.customRole(admin, "self reviewer", "app", "app.view", "app.deploy", "app.deploy.approve")
	self := a.user("self")
	granted := a.do(admin, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "user", "principal_id": a.userID(self), "role_id": roleID,
	})
	require.Equal(t, http.StatusCreated, granted.Code, granted.String())
	mine := a.deploy(self, appID)
	got = a.decide(self, appID, mine.ID, "approve", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.Contains(t, a.starts.ids(), mine.ID)
}

// TestR155_AnOwnerCannotApproveByDefault asserts that no built-in app role
// holds app.deploy.approve, so an owner's request needs somebody else.
func TestR155_AnOwnerCannotApproveByDefault(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.appWithSpec(admin, "notes")
	dev := a.owner(admin, appID, "dev")
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalRequired = true })

	dep := a.deploy(dev, appID)
	got := a.decide(dev, appID, dep.ID, "approve", nil)
	require.Equal(t, http.StatusForbidden, got.Code, got.String())
	require.Equal(t, string(errs.PermVerbRequired), got.ErrorCode())
	require.Contains(t, got.String(), "install.deploys.approve")
	require.Contains(t, got.String(), "app.deploy.approve")

	got = a.decide(dev, appID, dep.ID, "reject", nil)
	require.Equal(t, http.StatusForbidden, got.Code, got.String())

	require.Equal(t, state.DeployAwaitingApproval, a.deployment(admin, appID, dep.ID).Status)
	require.Empty(t, a.starts.ids())
}

// TestR155_AppManagerCannotApprove asserts that managing every app does not
// stand for app.deploy.approve: the built-in App manager holds every other
// app verb's install-wide counterpart and not install.deploys.approve.
func TestR155_AppManagerCannotApprove(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.appWithSpec(admin, "notes")
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalRequired = true })

	manager := a.user("manager")
	got := a.do(admin, http.MethodPut, "/users/"+a.userID(manager)+"/role", map[string]any{"role_id": "role_app_manager"})
	require.Equal(t, http.StatusOK, got.Code, got.String())

	// It manages the app — it can deploy it — and still cannot approve.
	dep := a.deploy(manager, appID)
	require.Equal(t, state.DeployAwaitingApproval, dep.Status)
	require.False(t, dep.CanDecide)

	got = a.decide(manager, appID, dep.ID, "approve", nil)
	require.Equal(t, http.StatusForbidden, got.Code, got.String())
	require.Equal(t, string(errs.PermVerbRequired), got.ErrorCode())
}

// TestR154_AnAgentCannotApproveByDefault asserts that approval is a human
// sign-off: the shipped policy denies both verbs to tokens.
func TestR154_AnAgentCannotApproveByDefault(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.appWithSpec(admin, "notes")
	a.policy(func(d *corepolicy.Document) {
		*d = corepolicy.Default()
		d.DeployApprovalRequired = true
	})

	agent := a.tokenFor(admin)
	dep := a.deploy(agent, appID)
	require.False(t, dep.CanDecide)

	got := a.decide(agent, appID, dep.ID, "approve", nil)
	require.Equal(t, http.StatusForbidden, got.Code, got.String())
	require.Equal(t, string(errs.PolicyExecDisabled), got.ErrorCode())
	require.Contains(t, got.String(), "Tokens and agents are not allowed")

	// The same person, signed in, may.
	got = a.decide(admin, appID, dep.ID, "approve", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
}

// TestR156_ADeployNeedsTheConfiguredNumberOfApprovals asserts the count, and
// that one person approving twice is one approval.
func TestR156_ADeployNeedsTheConfiguredNumberOfApprovals(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.appWithSpec(admin, "notes")
	reviewer := a.reviewer(admin, appID, "reviewer")
	a.policy(func(d *corepolicy.Document) {
		d.DeployApprovalRequired = true
		d.DeployApprovalCount = 2
	})

	dep := a.deploy(admin, appID)
	require.Equal(t, 2, dep.ApprovalsRequired)

	for range 2 {
		got := a.decide(admin, appID, dep.ID, "approve", nil)
		require.Equal(t, http.StatusOK, got.Code, got.String())
		var still deployView
		got.JSON(t, &still)
		require.Equal(t, state.DeployAwaitingApproval, still.Status, "one person is one approval")
		require.Len(t, still.Approvals, 1)
	}
	require.Empty(t, a.starts.ids())

	// The count is fixed when the request is made: lowering it now does not
	// move this request's goalposts.
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalCount = 1 })
	require.Equal(t, 2, a.deployment(admin, appID, dep.ID).ApprovalsRequired)

	got := a.decide(reviewer, appID, dep.ID, "approve", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var started deployView
	got.JSON(t, &started)
	require.Equal(t, state.DeployPending, started.Status)
	require.Len(t, started.Approvals, 2)
	require.Equal(t, []string{dep.ID}, a.starts.ids())
}

// TestR156_ANewerRequestSupersedesAnOlderOne asserts that only the newest
// request for an app waits.
func TestR156_ANewerRequestSupersedesAnOlderOne(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.appWithSpec(admin, "notes")
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalRequired = true })

	older := a.deploy(admin, appID)
	newer := a.deploy(admin, appID)

	require.Equal(t, state.DeploySuperseded, a.deployment(admin, appID, older.ID).Status)
	listed := a.waiting(admin)
	require.Len(t, listed, 1)
	require.Equal(t, newer.ID, listed[0].ID)

	got := a.decide(admin, appID, older.ID, "approve", nil)
	require.Equal(t, http.StatusConflict, got.Code, got.String())
	require.Equal(t, string(errs.StateInvalid), got.ErrorCode())
	require.Contains(t, got.String(), "newer deploy")

	// A deploy that needs no approval supersedes a waiting request too:
	// approving the request afterwards would put back an older revision.
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalRequired = false })
	direct := a.deploy(admin, appID)
	require.Equal(t, state.DeployPending, direct.Status)
	require.Equal(t, state.DeploySuperseded, a.deployment(admin, appID, newer.ID).Status)
}

// TestR156_ARequestExpires asserts the expiry: the sweep ends a request whose
// wait ran out, and approving one that ran out before the sweep reached it is
// refused and expires it on the spot.
func TestR156_ARequestExpires(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	ctx := context.Background()
	first := a.appWithSpec(admin, "notes")
	second := a.appWithSpec(admin, "wiki")
	a.policy(func(d *corepolicy.Document) {
		d.DeployApprovalRequired = true
		d.DeployApprovalExpiryHours = 1
	})

	swept := a.deploy(admin, first)
	lazy := a.deploy(admin, second)
	for _, depID := range []string{swept.ID, lazy.ID} {
		_, err := a.db.Exec(ctx,
			`UPDATE deployments SET approval_expires_at = now() - interval '1 minute' WHERE id = $1`, depID)
		require.NoError(t, err)
	}

	// Past its time and not yet swept, it is not listed as waiting.
	require.Empty(t, a.waiting(admin))

	got := a.decide(admin, second, lazy.ID, "approve", nil)
	require.Equal(t, http.StatusConflict, got.Code, got.String())
	require.Contains(t, got.String(), "expired")
	require.Equal(t, state.DeployExpired, a.deployment(admin, second, lazy.ID).Status)

	n, err := a.Server.Approvals.ExpireDue(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the lazily expired one is not expired twice")
	require.Equal(t, state.DeployExpired, a.deployment(admin, first, swept.ID).Status)
	require.Empty(t, a.starts.ids())

	// Zero is forever.
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalExpiryHours = 0 })
	forever := a.deploy(admin, first)
	require.Nil(t, forever.ApprovalExpiresAt)
}

// TestR156_ARejectionEndsTheRequest asserts that one rejection is the end of
// it, whatever approvals came before.
func TestR156_ARejectionEndsTheRequest(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.appWithSpec(admin, "notes")
	reviewer := a.reviewer(admin, appID, "reviewer")
	a.policy(func(d *corepolicy.Document) {
		d.DeployApprovalRequired = true
		d.DeployApprovalCount = 2
	})

	dep := a.deploy(admin, appID)
	require.Equal(t, http.StatusOK, a.decide(admin, appID, dep.ID, "approve", nil).Code)

	got := a.decide(reviewer, appID, dep.ID, "reject", map[string]any{"comment": "not on a Friday"})
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var rejected deployView
	got.JSON(t, &rejected)
	require.Equal(t, state.DeployRejected, rejected.Status)
	require.Len(t, rejected.Approvals, 2)

	got = a.decide(admin, appID, dep.ID, "approve", nil)
	require.Equal(t, http.StatusConflict, got.Code, got.String())
	require.Contains(t, got.String(), "rejected")
	require.Empty(t, a.starts.ids())
}

// TestR156_TheLastApprovalPlansAgain asserts that the plan runs again at
// approval, because policy may have changed while the request waited: a plan
// that now fails ends the request as failed, with the plan's own error, and a
// failure to plan at all leaves it waiting.
func TestR156_TheLastApprovalPlansAgain(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.appWithSpec(admin, "notes")
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalRequired = true })

	dep := a.deploy(admin, appID)

	a.plans.failWith(errs.New(errs.AdapterUnavailable, "Pando cannot reach \"rt_docker\" right now."))
	got := a.decide(admin, appID, dep.ID, "approve", nil)
	require.Equal(t, http.StatusBadGateway, got.Code, got.String())
	waiting := a.deployment(admin, appID, dep.ID)
	require.Equal(t, state.DeployAwaitingApproval, waiting.Status, "an unreachable adapter is no verdict on the revision")
	require.Empty(t, waiting.Approvals)

	a.plans.failWith(errs.New(errs.PlanNoAdapterMeetsPolicy, "No runtime here meets the installation's isolation floor."))
	got = a.decide(admin, appID, dep.ID, "approve", nil)
	require.Equal(t, http.StatusConflict, got.Code, got.String())
	require.Equal(t, string(errs.PlanNoAdapterMeetsPolicy), got.ErrorCode())
	failed := a.deployment(admin, appID, dep.ID)
	require.Equal(t, state.DeployFailed, failed.Status)
	require.Equal(t, string(errs.PlanNoAdapterMeetsPolicy), failed.ErrorCode)
	require.Empty(t, a.starts.ids())
}

// TestR156_ApprovalIsRefusedWhileTheAppIsDeploying asserts that the last
// approval does not start a deploy alongside another, and the request keeps
// waiting.
func TestR156_ApprovalIsRefusedWhileTheAppIsDeploying(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	ctx := context.Background()
	appID := a.appWithSpec(admin, "notes")
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalRequired = true })

	dep := a.deploy(admin, appID)

	app, _, err := a.Apps.ByID(ctx, appID)
	require.NoError(t, err)
	running, err := a.Server.Deployments.Create(ctx, appID, app.PinnedSpecID, state.TriggerManual, a.AdminID)
	require.NoError(t, err)

	got := a.decide(admin, appID, dep.ID, "approve", nil)
	require.Equal(t, http.StatusConflict, got.Code, got.String())
	require.Contains(t, got.String(), "deploying right now")
	require.Equal(t, state.DeployAwaitingApproval, a.deployment(admin, appID, dep.ID).Status)

	require.NoError(t, a.Server.Deployments.Finish(ctx, running.ID, state.DeploySucceeded, "", ""))
	got = a.decide(admin, appID, dep.ID, "approve", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.Equal(t, []string{dep.ID}, a.starts.ids())
}

// TestR157_RollbackToARevisionThatRanNeedsNoApproval asserts that rolling back
// to a revision that already ran successfully is free, and that "rollback" to
// one that never ran is an ordinary deploy that needs approval.
func TestR157_RollbackToARevisionThatRanNeedsNoApproval(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	ctx := context.Background()
	appID := a.createApp(admin, "notes")

	first := a.writeSpec(admin, appID, minimalSpec())
	a.pinSpec(admin, appID, first)
	app, _, err := a.Apps.ByID(ctx, appID)
	require.NoError(t, err)
	ran, err := a.Server.Deployments.Create(ctx, appID, app.PinnedSpecID, state.TriggerManual, a.AdminID)
	require.NoError(t, err)
	require.NoError(t, a.Server.Deployments.Finish(ctx, ran.ID, state.DeploySucceeded, "", ""))

	neverRan := a.writeSpec(admin, appID, minimalSpec())
	latest := a.writeSpec(admin, appID, minimalSpec())
	a.pinSpec(admin, appID, latest)

	a.policy(func(d *corepolicy.Document) { d.DeployApprovalRequired = true })

	got := a.do(admin, http.MethodPost, "/apps/"+appID+"/deployments/rollback", map[string]any{"to": first})
	require.Equal(t, http.StatusAccepted, got.Code, got.String())
	var back deployView
	got.JSON(t, &back)
	require.Equal(t, state.DeployPending, back.Status)
	require.Equal(t, state.TriggerRollback, back.Trigger)
	require.Equal(t, first, back.SpecRevision)
	require.Equal(t, []string{back.ID}, a.starts.ids())
	require.NoError(t, a.Server.Deployments.Finish(ctx, back.ID, state.DeploySucceeded, "", ""))

	got = a.do(admin, http.MethodPost, "/apps/"+appID+"/deployments/rollback",
		map[string]any{"spec_revision": neverRan})
	require.Equal(t, http.StatusAccepted, got.Code, got.String())
	var notReally deployView
	got.JSON(t, &notReally)
	require.Equal(t, state.DeployAwaitingApproval, notReally.Status,
		"a revision that never ran was not approved before, so rolling 'back' to it is no exemption")
	require.Equal(t, neverRan, notReally.SpecRevision)
}

// TestR389_ARollbackRecordsWhatItRolledBackFrom asserts R-389: the rollback's
// app.deploy event names the revision that was running as well as the one it
// went back to.
func TestR389_ARollbackRecordsWhatItRolledBackFrom(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	ctx := context.Background()
	appID := a.createApp(admin, "notes")

	first := a.writeSpec(admin, appID, minimalSpec())
	a.pinSpec(admin, appID, first)
	app, _, err := a.Apps.ByID(ctx, appID)
	require.NoError(t, err)
	ran, err := a.Server.Deployments.Create(ctx, appID, app.PinnedSpecID, state.TriggerManual, a.AdminID)
	require.NoError(t, err)
	require.NoError(t, a.Server.Deployments.Finish(ctx, ran.ID, state.DeploySucceeded, "", ""))

	latest := a.writeSpec(admin, appID, minimalSpec())
	a.pinSpec(admin, appID, latest)
	running, _, err := a.Apps.ByID(ctx, appID)
	require.NoError(t, err)

	got := a.do(admin, http.MethodPost, "/apps/"+appID+"/deployments/rollback", map[string]any{"to": first})
	require.Equal(t, http.StatusAccepted, got.Code, got.String())

	var log struct {
		Events []struct {
			Detail map[string]any `json:"detail"`
		} `json:"events"`
	}
	got = a.do(admin, http.MethodGet, "/audit?action=app.deploy&app_id="+appID, nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	got.JSON(t, &log)
	require.NotEmpty(t, log.Events)
	newest := log.Events[0].Detail
	require.Equal(t, "rollback", newest["trigger"])
	require.EqualValues(t, first, newest["spec_revision"])
	require.Equal(t, running.PinnedSpecID, newest["rolled_back_from"])
}

// TestR158_TheStatusSaysWhenApprovalPausesAutoDeploy asserts what the console
// reads to say so on the app.
func TestR158_TheStatusSaysWhenApprovalPausesAutoDeploy(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.createApp(admin, "notes")
	tracking := minimalSpec()
	tracking["deploy"] = map[string]any{"strategy": "recreate", "auto_deploy": map[string]any{"enabled": true, "branch": "main"}}
	a.pinSpec(admin, appID, a.writeSpec(admin, appID, tracking))

	status := func() bool {
		var body struct {
			Paused *bool `json:"auto_deploy_paused"`
		}
		got := a.do(admin, http.MethodGet, "/apps/"+appID+"/status", nil)
		require.Equal(t, http.StatusOK, got.Code, got.String())
		got.JSON(t, &body)
		require.NotNil(t, body.Paused, got.String())
		return *body.Paused
	}
	require.False(t, status())

	a.policy(func(d *corepolicy.Document) { d.DeployApprovalApps = []string{appID} })
	require.True(t, status())
}

// TestR159_EveryStepOfAnApprovalIsAudited asserts the audit events: request,
// approve, reject, expire and supersede.
func TestR159_EveryStepOfAnApprovalIsAudited(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	ctx := context.Background()
	appID := a.appWithSpec(admin, "notes")
	a.policy(func(d *corepolicy.Document) { d.DeployApprovalRequired = true })

	superseded := a.deploy(admin, appID)
	rejected := a.deploy(admin, appID)
	require.Equal(t, http.StatusOK, a.decide(admin, appID, rejected.ID, "reject", nil).Code)

	expired := a.deploy(admin, appID)
	_, err := a.db.Exec(ctx, `UPDATE deployments SET approval_expires_at = now() - interval '1 minute' WHERE id = $1`, expired.ID)
	require.NoError(t, err)
	_, err = a.Server.Approvals.ExpireDue(ctx)
	require.NoError(t, err)

	approved := a.deploy(admin, appID)
	require.Equal(t, http.StatusOK, a.decide(admin, appID, approved.ID, "approve", nil).Code)

	actions := a.auditActions(appID)
	require.Equal(t, 4, actions["deploy.request"], "%v", actions)
	require.Equal(t, 1, actions["deploy.supersede"], "%v", actions)
	require.Equal(t, 1, actions["deploy.reject"], "%v", actions)
	require.Equal(t, 1, actions["deploy.expire"], "%v", actions)
	require.Equal(t, 1, actions["deploy.approve"], "%v", actions)
	require.Equal(t, 1, actions["app.deploy"], "the approved deploy starting is audited like any other: %v", actions)

	records, err := a.Server.AuditLog.List(ctx, audit.Query{AppID: appID, Action: "deploy.supersede"})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, superseded.ID, records[0].TargetID)
	require.Equal(t, rejected.ID, records[0].Detail["superseded_by"])
}

// TestR154_DeployingTheSpecThatLoosensEgressWaitsForApproval asserts that an
// egress loosening is measured against what the app last ran, not its pinned
// spec. A deploy normally deploys the pinned spec, so comparing against it
// would compare the revision with itself and never find the loosening new.
func TestR154_DeployingTheSpecThatLoosensEgressWaitsForApproval(t *testing.T) {
	t.Parallel()
	a := newApprovals(t)
	admin := a.admin()
	appID := a.createApp(admin, "notes")
	a.policy(func(d *corepolicy.Document) {
		d.EgressBlockPrivate = true
		d.EgressLoosening = corepolicy.EgressLooseningApproval
	})

	// Turning the installation's private-address block off for this app is
	// a loosening (R-182); under this policy it needs approval (R-183).
	loose := minimalSpec()
	loose["egress"] = map[string]any{"mode": "inherit", "block_private": false}
	a.pinSpec(admin, appID, a.writeSpec(admin, appID, loose))

	dep := a.deploy(admin, appID)
	require.Equal(t, state.DeployAwaitingApproval, dep.Status)
	require.Equal(t, "egress_loosening", dep.ApprovalReasons[0].Reason)
}
