package assist

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// ---------------------------------------------------------------------------
// Fakes

// fakeAI is an AI adapter whose answers and failures are set per test, and
// which records what core sent it.
type fakeAI struct {
	caps    api.AICapabilities
	capsErr error

	// err fails every function call; summaryErr fails only the summary.
	err        error
	summaryErr error

	access  api.AccessDraft
	policy  api.PolicyDraft
	search  api.AuditSearch
	summary api.AuditSummary
	answer  api.ReferenceAnswer

	accessReq   api.AccessRequest
	policyReq   api.PolicyRequest
	searchReq   api.AuditSearchRequest
	summaryReq  api.AuditSummaryRequest
	summaryRan  bool
	refReq      api.ReferenceRequest
	hadDeadline time.Time

	// lookFor, when set, is looked up with every lookup a request offers,
	// as a model calling the tools would, and the results kept in looked.
	lookFor []string
	looked  lookedUp
	lookErr error
}

// lookedUp is what a fakeAI's lookups returned.
type lookedUp struct {
	people []api.PersonInfo
	apps   []api.AppInfo
	groups []api.GroupInfo
}

func (f *fakeAI) lookUp(ctx context.Context, l *api.Lookup) {
	if l == nil {
		return
	}
	for _, q := range f.lookFor {
		if l.People != nil {
			got, err := l.People(ctx, q)
			f.looked.people = append(f.looked.people, got...)
			f.lookErr = errors.Join(f.lookErr, err)
		}
		if l.Apps != nil {
			got, err := l.Apps(ctx, q)
			f.looked.apps = append(f.looked.apps, got...)
			f.lookErr = errors.Join(f.lookErr, err)
		}
		if l.Groups != nil {
			got, err := l.Groups(ctx, q)
			f.looked.groups = append(f.looked.groups, got...)
			f.lookErr = errors.Join(f.lookErr, err)
		}
	}
}

func (f *fakeAI) Kind() string                                     { return "fake" }
func (f *fakeAI) Category() api.Category                           { return api.CategoryAI }
func (f *fakeAI) Configure(context.Context, json.RawMessage) error { return nil }
func (f *fakeAI) HealthCheck(context.Context) error                { return nil }

func (f *fakeAI) Capabilities(context.Context) (api.AICapabilities, error) {
	return f.caps, f.capsErr
}

func (f *fakeAI) RepairPlan(context.Context, api.ScreenRequest) (api.ScreenResult, error) {
	return api.ScreenResult{}, errors.New("not used")
}

func (f *fakeAI) AnswerQuestions(context.Context, api.ScreenRequest) (api.ScreenResult, error) {
	return api.ScreenResult{}, errors.New("not used")
}

func (f *fakeAI) RevisePlan(context.Context, api.ScreenRequest) (api.ScreenResult, error) {
	return api.ScreenResult{}, errors.New("not used")
}

func (f *fakeAI) DraftAccess(ctx context.Context, req api.AccessRequest) (api.AccessDraft, error) {
	f.accessReq = req
	f.hadDeadline, _ = ctx.Deadline()
	f.lookUp(ctx, req.Lookup)
	return f.access, f.err
}

func (f *fakeAI) DraftPolicy(_ context.Context, req api.PolicyRequest) (api.PolicyDraft, error) {
	f.policyReq = req
	return f.policy, f.err
}

func (f *fakeAI) SearchAudit(ctx context.Context, req api.AuditSearchRequest) (api.AuditSearch, error) {
	f.searchReq = req
	f.lookUp(ctx, req.Lookup)
	return f.search, f.err
}

func (f *fakeAI) SummarizeAudit(_ context.Context, req api.AuditSummaryRequest) (api.AuditSummary, error) {
	f.summaryReq = req
	f.summaryRan = true
	return f.summary, f.summaryErr
}

func (f *fakeAI) AnswerReference(ctx context.Context, req api.ReferenceRequest) (api.ReferenceAnswer, error) {
	f.refReq = req
	f.hadDeadline, _ = ctx.Deadline()
	return f.answer, f.err
}

// assistFunctions are the four administrative functions this package runs.
var assistFunctions = []api.AIFunction{
	api.AIFunctionDraftAccess, api.AIFunctionDraftPolicy,
	api.AIFunctionSearchAudit, api.AIFunctionAnswerReference,
}

func doesAll() api.AICapabilities {
	return api.AICapabilities{Functions: assistFunctions, Model: "house-model"}
}

// errUnbounded is what every fake store returns for a read with no bound: no
// limit, or a limit past a page, with neither a query nor IDs to narrow it.
// O-54: nothing the assistant does reads a whole table.
var errUnbounded = errors.New("an unbounded read of a whole table")

func bounded(limit int, narrowed bool) error {
	if limit <= 0 || limit > state.MaxPageSize || (!narrowed && limit > api.LookupLimit) {
		return errUnbounded
	}
	return nil
}

// matches is a case-insensitive substring match on any of fields, as the
// stores' ILIKE is; an empty query matches everything.
func matches(q string, fields ...string) bool {
	q = strings.ToLower(q)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return q == ""
}

type fakeUsers struct {
	users []state.User
	err   error

	// calls counts every read, searches and pages.
	calls *int
}

func (f fakeUsers) count() {
	if f.calls != nil {
		*f.calls++
	}
}

func (f fakeUsers) Search(_ context.Context, q string, limit int) ([]state.User, error) {
	f.count()
	if err := bounded(limit, false); err != nil {
		return nil, err
	}
	var out []state.User
	for _, u := range f.users {
		if len(out) < limit && matches(q, u.ExternalID, u.DisplayName, u.Email) {
			out = append(out, u)
		}
	}
	return out, f.err
}

func (f fakeUsers) ListPage(_ context.Context, page state.Page) ([]state.User, string, int, error) {
	f.count()
	if err := bounded(page.Limit, len(page.IDs) > 0); err != nil {
		return nil, "", 0, err
	}
	var out []state.User
	for _, u := range f.users {
		if len(out) < page.Limit && (len(page.IDs) == 0 || slices.Contains(page.IDs, u.ID)) &&
			matches(page.Query, u.ExternalID, u.DisplayName, u.Email) {
			out = append(out, u)
		}
	}
	return out, "", len(out), f.err
}

type fakeApps struct {
	apps []state.App
	err  error

	// granted are the apps ListForPrincipalPage returns: those the
	// principal holds a grant on.
	granted map[string]bool

	// every and mine count which list was read.
	every, mine *int
}

func (f fakeApps) page(page state.Page, only map[string]bool) ([]state.App, string, int, error) {
	if err := bounded(page.Limit, len(page.IDs) > 0); err != nil {
		return nil, "", 0, err
	}
	var out []state.App
	for _, a := range f.apps {
		if only != nil && !only[a.ID] {
			continue
		}
		if len(out) < page.Limit && (len(page.IDs) == 0 || slices.Contains(page.IDs, a.ID)) && matches(page.Query, a.Name, a.Slug) {
			out = append(out, a)
		}
	}
	return out, "", len(out), f.err
}

func (f fakeApps) ListAllPage(_ context.Context, page state.Page) ([]state.App, string, int, error) {
	if f.every != nil {
		*f.every++
	}
	return f.page(page, nil)
}

func (f fakeApps) ListForPrincipalPage(_ context.Context, _ authz.Principal, page state.Page) ([]state.App, string, int, error) {
	if f.mine != nil {
		*f.mine++
	}
	only := f.granted
	if only == nil {
		only = map[string]bool{}
	}
	return f.page(page, only)
}

type fakeRoles struct {
	roles []state.RoleRow
	err   error
}

func (f fakeRoles) List(context.Context) ([]state.RoleRow, error) { return f.roles, f.err }

type fakeGroups struct {
	groups []state.Group
	err    error
}

func (f fakeGroups) Search(_ context.Context, q string, limit int) ([]state.Group, error) {
	if err := bounded(limit, q != ""); err != nil {
		return nil, err
	}
	var out []state.Group
	for _, g := range f.groups {
		if len(out) < limit && matches(q, g.Name) {
			out = append(out, g)
		}
	}
	return out, f.err
}

type fakeVerbs struct {
	verbs []string
	err   error
}

func (f fakeVerbs) InstallVerbsFor(context.Context, authz.Principal) ([]string, error) {
	return f.verbs, f.err
}

type fakePolicy struct {
	doc policy.Document
	err error
}

func (f fakePolicy) Load(context.Context) (policy.Document, error) { return f.doc, f.err }

type fakeAudit struct {
	records []audit.Record
	listErr error

	query  audit.Query
	listed bool
}

func (f *fakeAudit) List(_ context.Context, q audit.Query) ([]audit.Record, error) {
	f.query = q
	f.listed = true
	return f.records, f.listErr
}

var (
	errBoom = errors.New("boom")
	admin   = authz.Principal{Kind: "user", ID: "usr_admin", UserID: "usr_admin"}

	// adminVerbs are what admin holds install-wide in these tests: enough
	// to read accounts, groups and every app, and to read the audit log.
	adminVerbs = []string{"install.audit.read", "install.view", "install.apps.view"}
)

// newService is a Service over fixed people, apps, roles and groups, with
// every function assigned to ai (when non-nil) as ai_fake on model.
func newService(t *testing.T, ai *fakeAI, model string) *Service {
	t.Helper()
	reg := api.NewRegistry()
	if ai != nil {
		require.NoError(t, reg.Register("ai_fake", ai))
		var as []api.AIAssignment
		for _, fn := range assistFunctions {
			as = append(as, api.AIAssignment{Function: fn, AdapterRef: "ai_fake", Model: model})
		}
		reg.SetAIAssignments(as)
	}
	return &Service{
		Registry: reg,
		Users: fakeUsers{users: []state.User{
			{ID: "usr_ada", ExternalID: "ada", Email: "Ada@Example.com", DisplayName: "Ada Lovelace"},
			{ID: "usr_bob", ExternalID: "bob", Email: "bob@example.com", DisplayName: "Bob Byte"},
		}},
		Apps:   fakeApps{apps: []state.App{{ID: "app_blog", Name: "blog", Slug: "blog"}}},
		Roles:  fakeRoles{roles: []state.RoleRow{{ID: "role_viewer", Name: "Viewer", Scope: "app", Builtin: true, Verbs: []string{"app.view"}}}},
		Groups: fakeGroups{groups: []state.Group{{ID: "grp_ops", Name: "Ops"}}},
		Verbs:  fakeVerbs{verbs: adminVerbs},
		Policy: fakePolicy{},
		Audit:  &fakeAudit{},
	}
}

func requireCode(t *testing.T, err error, code errs.Code) *errs.Error {
	t.Helper()
	require.Error(t, err)
	e := errs.As(err)
	require.NotNil(t, e, "want an *errs.Error, got %v", err)
	require.Equal(t, code, e.Code, e.Message)
	return e
}

// ---------------------------------------------------------------------------
// ask and call

// TestAskRefusesEmptyAndOverlong asserts a question must say something and
// stay under MaxAsk characters, counted as characters rather than bytes.
func TestAskRefusesEmptyAndOverlong(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "")
	ctx := context.Background()

	_, err := s.AnswerReference(ctx, "   \n")
	e := requireCode(t, err, errs.ValidInvalid)
	assert.Equal(t, "Say what you want to do.", e.Message)

	_, err = s.AnswerReference(ctx, strings.Repeat("é", MaxAsk+1))
	e = requireCode(t, err, errs.ValidInvalid)
	assert.Contains(t, e.Message, "longer than 2000 characters")

	// MaxAsk two-byte characters is 4000 bytes, and still accepted.
	_, err = s.AnswerReference(ctx, strings.Repeat("é", MaxAsk))
	require.NoError(t, err)

	_, err = s.AnswerReference(ctx, "  how do I deploy?  ")
	require.NoError(t, err)
	assert.Equal(t, "how do I deploy?", ai.refReq.Question, "the question is trimmed")
}

// TestEveryFunctionRefusesAnEmptyAsk asserts the other three functions check
// their text before reading anything.
func TestEveryFunctionRefusesAnEmptyAsk(t *testing.T) {
	s := newService(t, &fakeAI{caps: doesAll()}, "")
	ctx := context.Background()

	_, err := s.DraftAccess(ctx, admin, "", nil)
	requireCode(t, err, errs.ValidInvalid)
	_, err = s.DraftPolicy(ctx, " ", nil)
	requireCode(t, err, errs.ValidInvalid)
	_, err = s.SearchAudit(ctx, admin, "")
	requireCode(t, err, errs.ValidInvalid)
}

// TestR343_UnassignedFunctionIsOff asserts a function no adapter is assigned
// is refused as unavailable, naming how to assign it.
func TestR343_UnassignedFunctionIsOff(t *testing.T) {
	s := newService(t, nil, "")
	ctx := context.Background()

	_, err := s.DraftAccess(ctx, admin, "let Ada deploy", nil)
	e := requireCode(t, err, errs.AdapterUnavailable)
	assert.Equal(t, "Access drafting is not assigned to an AI adapter on this installation.", e.Message)
	assert.Contains(t, e.Remedy, "PUT /api/v1/ai/functions/draft_access")

	_, err = s.DraftPolicy(ctx, "no exec", nil)
	requireCode(t, err, errs.AdapterUnavailable)
	_, err = s.SearchAudit(ctx, admin, "who deleted blog")
	requireCode(t, err, errs.AdapterUnavailable)
	_, err = s.AnswerReference(ctx, "how do I deploy")
	requireCode(t, err, errs.AdapterUnavailable)
}

// TestCallNamesAnAssignedAdapterThatIsNotRunning asserts an assignment to an
// adapter the registry does not hold is off, and the message names it.
func TestCallNamesAnAssignedAdapterThatIsNotRunning(t *testing.T) {
	s := newService(t, nil, "")
	s.Registry.SetAIAssignments([]api.AIAssignment{{Function: api.AIFunctionAnswerReference, AdapterRef: "ai_gone"}})

	_, err := s.AnswerReference(context.Background(), "how do I deploy")
	e := requireCode(t, err, errs.AdapterUnavailable)
	assert.Equal(t, "Reference help is assigned to the adapter ai_gone, which is not running.", e.Message)
}

// TestCallRefusesWhenCapabilitiesFail asserts an adapter that cannot say what
// it does is unavailable, and is not called.
func TestCallRefusesWhenCapabilitiesFail(t *testing.T) {
	ai := &fakeAI{capsErr: errBoom}
	s := newService(t, ai, "")

	_, err := s.AnswerReference(context.Background(), "how do I deploy")
	e := requireCode(t, err, errs.AdapterUnavailable)
	assert.Equal(t, "Pando could not reach the adapter ai_fake.", e.Message)
	assert.Empty(t, ai.refReq.Question, "the adapter was not asked")
}

// TestCallRefusesAnAdapterThatNoLongerPerformsTheFunction asserts an
// assignment is rechecked against the adapter's capabilities on each call.
func TestCallRefusesAnAdapterThatNoLongerPerformsTheFunction(t *testing.T) {
	ai := &fakeAI{caps: api.AICapabilities{Functions: []api.AIFunction{api.AIFunctionRepairPlan}}}
	s := newService(t, ai, "")

	_, err := s.AnswerReference(context.Background(), "how do I deploy")
	e := requireCode(t, err, errs.AdapterUnavailable)
	assert.Equal(t, "Reference help is assigned to the adapter ai_fake, which no longer performs it.", e.Message)
	assert.Contains(t, e.Remedy, "PUT /api/v1/ai/functions/answer_reference")
}

// TestR259_AssignedModelReachesOnlyAnAdapterThatChooses asserts an
// assignment's model is sent to an adapter that chooses models, dropped for
// one that does not, and that the reported model is what actually ran.
func TestR259_AssignedModelReachesOnlyAnAdapterThatChooses(t *testing.T) {
	ctx := context.Background()

	// Cannot choose: the assignment's model is dropped, the adapter's own runs.
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "big-model")
	res, err := s.AnswerReference(ctx, "how do I deploy")
	require.NoError(t, err)
	assert.Empty(t, ai.refReq.Model)
	assert.Equal(t, Ran{AdapterID: "ai_fake", Model: "house-model"}, res.Ran)

	// Chooses: the assignment's model is sent and reported.
	caps := doesAll()
	caps.ChoosesModel = true
	ai = &fakeAI{caps: caps}
	s = newService(t, ai, "big-model")
	res, err = s.AnswerReference(ctx, "how do I deploy")
	require.NoError(t, err)
	assert.Equal(t, "big-model", ai.refReq.Model)
	assert.Equal(t, "big-model", res.Model)

	// The adapter's own report of what ran wins.
	ai.answer.Model = "big-model-2026"
	res, err = s.AnswerReference(ctx, "how do I deploy")
	require.NoError(t, err)
	assert.Equal(t, "big-model-2026", res.Model)
}

// TestCallBoundsTheAdapterCall asserts the adapter is called with a deadline:
// the configured timeout, else DefaultTimeout.
func TestCallBoundsTheAdapterCall(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "")
	ctx := context.Background()

	before := time.Now()
	_, err := s.AnswerReference(ctx, "how do I deploy")
	require.NoError(t, err)
	assert.WithinDuration(t, before.Add(DefaultTimeout), ai.hadDeadline, 5*time.Second)

	s.Timeout = time.Second
	before = time.Now()
	_, err = s.AnswerReference(ctx, "how do I deploy")
	require.NoError(t, err)
	assert.WithinDuration(t, before.Add(time.Second), ai.hadDeadline, 500*time.Millisecond)
}

// TestAdapterErrorIsAdapterFailed asserts each function reports an adapter's
// failure as ADAPTER_FAILED, naming the function, with a way forward.
func TestAdapterErrorIsAdapterFailed(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		title string
		run   func(s *Service) error
	}{
		{"access", "access drafting", func(s *Service) error { _, err := s.DraftAccess(ctx, admin, "let Ada deploy", nil); return err }},
		{"policy", "policy drafting", func(s *Service) error { _, err := s.DraftPolicy(ctx, "no exec", nil); return err }},
		{"audit", "audit search", func(s *Service) error { _, err := s.SearchAudit(ctx, admin, "who deleted blog"); return err }},
		{"reference", "reference help", func(s *Service) error { _, err := s.AnswerReference(ctx, "how do I deploy"); return err }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newService(t, &fakeAI{caps: doesAll(), err: errors.New("rate limited")}, "")
			e := requireCode(t, c.run(s), errs.AdapterFailed)
			assert.Equal(t, "The AI adapter could not finish "+c.title+": rate limited", e.Message)
			assert.Equal(t, "Try again in a moment, or do this without AI.", e.Remedy)
		})
	}
}

// ---------------------------------------------------------------------------
// Access (R-343)

// TestR343_AccessCatalogIsWhatTheCallerCouldGrant asserts the adapter is
// offered every app verb and only the install verbs the caller holds, along
// with the roles, groups and people that exist.
func TestR343_AccessCatalogIsWhatTheCallerCouldGrant(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "")
	current := &api.AccessDraft{Reply: "earlier"}

	_, err := s.DraftAccess(context.Background(), admin, "let Ada deploy", current)
	require.NoError(t, err)

	offered := map[string]string{}
	for _, v := range ai.accessReq.Verbs {
		offered[v.Name] = v.Scope
	}
	assert.Equal(t, "install", offered["install.audit.read"], "held install verb is offered")
	assert.NotContains(t, offered, "install.users.manage", "an install verb not held is not offered")
	assert.NotContains(t, offered, "app.create", "app.create is install-scoped and not held")
	assert.Equal(t, "app", offered["app.deploy"])
	assert.Equal(t, "app", offered["app.exec"])

	require.Len(t, ai.accessReq.Roles, 1)
	assert.Equal(t, api.RoleInfo{ID: "role_viewer", Name: "Viewer", Scope: "app", Builtin: true, Verbs: []string{"app.view"}}, ai.accessReq.Roles[0])
	// O-54: the people the description names, not every account; no group
	// matches its words.
	assert.Equal(t, []api.PersonInfo{{ID: "usr_ada", Username: "ada", Name: "Ada Lovelace", Email: "Ada@Example.com"}}, ai.accessReq.People)
	assert.Empty(t, ai.accessReq.Groups)
	assert.Nil(t, ai.accessReq.Lookup, "this adapter does not look things up")
	assert.Same(t, current, ai.accessReq.Current)
	assert.Equal(t, "let Ada deploy", ai.accessReq.Description)
}

// TestAccessWithoutVerbsSourceOffersNoInstallVerbs asserts a Service with no
// Verbs source offers app verbs only.
func TestAccessWithoutVerbsSourceOffersNoInstallVerbs(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "")
	s.Verbs = nil

	_, err := s.DraftAccess(context.Background(), admin, "let Ada deploy", nil)
	require.NoError(t, err)
	for _, v := range ai.accessReq.Verbs {
		assert.Equal(t, "app", v.Scope, v.Name)
	}
}

// TestR343_AccessDraftKeepsOnlyVerbsTheCallerCanGrant asserts an install
// verb the caller does not hold, and a verb that does not exist, are refused
// with a reason, while the rest of the role is kept, deduplicated and sorted.
func TestR343_AccessDraftKeepsOnlyVerbsTheCallerCanGrant(t *testing.T) {
	ai := &fakeAI{caps: doesAll(), access: api.AccessDraft{
		Role: &api.RoleDraft{Name: "  Auditors ", Scope: "install", Verbs: []string{
			"install.users.manage", "install.audit.read", "install.fly", "install.audit.read",
		}},
		Reply: "  Drafted an auditor role.  ",
		Model: "house-model-7",
	}}
	s := newService(t, ai, "")

	res, err := s.DraftAccess(context.Background(), admin, "people who can read the audit log", nil)
	require.NoError(t, err)
	require.NotNil(t, res.Role)
	assert.Equal(t, api.RoleDraft{Name: "Auditors", Scope: "install", Verbs: []string{"install.audit.read"}}, *res.Role)
	assert.Equal(t, []string{
		`install.users.manage is not a permission you can grant, so it was left out of "Auditors".`,
		`install.fly is not a permission you can grant, so it was left out of "Auditors".`,
	}, res.Refused)
	assert.Equal(t, "Drafted an auditor role.", res.Reply)
	assert.Equal(t, "house-model-7", res.Model)
	assert.Nil(t, res.Group)
}

// TestR080_AccessDraftRefusesVerbsOfTheOtherScope asserts an app role is
// refused install verbs and an install role app verbs.
func TestR080_AccessDraftRefusesVerbsOfTheOtherScope(t *testing.T) {
	ai := &fakeAI{caps: doesAll(), access: api.AccessDraft{
		Role: &api.RoleDraft{Name: "Deployers", Scope: "app", Verbs: []string{"app.restart", "install.audit.read", "app.deploy"}},
	}}
	s := newService(t, ai, "")

	res, err := s.DraftAccess(context.Background(), admin, "deployers", nil)
	require.NoError(t, err)
	require.NotNil(t, res.Role)
	assert.Equal(t, []string{"app.deploy", "app.restart"}, res.Role.Verbs)
	assert.Equal(t, []string{`install.audit.read is an install permission and "Deployers" is an app role, so it was left out. A role holds permissions of one scope.`}, res.Refused)

	ai.access.Role = &api.RoleDraft{Name: "Mixed", Scope: "install", Verbs: []string{"app.deploy"}}
	res, err = s.DraftAccess(context.Background(), admin, "mixed", nil)
	require.NoError(t, err)
	assert.Nil(t, res.Role, "a role left with no verb is dropped")
	assert.Equal(t, []string{
		`app.deploy is an app permission and "Mixed" is an install role, so it was left out. A role holds permissions of one scope.`,
		`The role "Mixed" had no permission Pando could draft, so it was left out.`,
	}, res.Refused)
}

// TestR082_AccessDraftRefusesAnExistingRoleName asserts a draft naming a role
// that exists, in any case, is refused rather than drafted again.
func TestR082_AccessDraftRefusesAnExistingRoleName(t *testing.T) {
	ai := &fakeAI{caps: doesAll(), access: api.AccessDraft{
		Role: &api.RoleDraft{Name: "VIEWER", Scope: "app", Verbs: []string{"app.view"}},
	}}
	s := newService(t, ai, "")

	res, err := s.DraftAccess(context.Background(), admin, "viewers", nil)
	require.NoError(t, err)
	assert.Nil(t, res.Role)
	require.Len(t, res.Refused, 1)
	assert.Contains(t, res.Refused[0], `A role called "VIEWER" already exists`)
}

// TestAccessDraftRefusesARoleWithoutNameOrScope asserts a role with no name,
// or a scope that is neither install nor app, is left out with a reason.
func TestAccessDraftRefusesARoleWithoutNameOrScope(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "")
	ctx := context.Background()

	ai.access.Role = &api.RoleDraft{Name: "  ", Scope: "app", Verbs: []string{"app.view"}}
	res, err := s.DraftAccess(ctx, admin, "a role", nil)
	require.NoError(t, err)
	assert.Nil(t, res.Role)
	assert.Equal(t, []string{"The role had no name, so it was left out."}, res.Refused)

	ai.access.Role = &api.RoleDraft{Name: "Global", Scope: "global", Verbs: []string{"app.view"}}
	res, err = s.DraftAccess(ctx, admin, "a role", nil)
	require.NoError(t, err)
	assert.Nil(t, res.Role)
	assert.Equal(t, []string{`The role "Global" had the scope "global", which is not install or app, so it was left out.`}, res.Refused)
}

// TestR343_AccessDraftGroupKeepsOnlyKnownPeople asserts a drafted group's
// members are accounts that exist, each once.
func TestR343_AccessDraftGroupKeepsOnlyKnownPeople(t *testing.T) {
	ai := &fakeAI{caps: doesAll(), access: api.AccessDraft{
		Group: &api.GroupDraft{Name: " Release ", Members: []string{"usr_ada", " usr_ada ", "usr_ghost", "usr_bob"}},
	}}
	s := newService(t, ai, "")

	res, err := s.DraftAccess(context.Background(), admin, "a release group", nil)
	require.NoError(t, err)
	require.NotNil(t, res.Group)
	assert.Equal(t, api.GroupDraft{Name: "Release", Members: []string{"usr_ada", "usr_bob"}}, *res.Group)
	assert.Equal(t, []string{`"usr_ghost" is not an account on this installation, so it was left out of the group.`}, res.Refused)
}

// TestAccessDraftRefusesAGroupWithoutNameOrWithAnExistingOne asserts a group
// with no name, or the name of a group that exists in any case, is left out.
func TestAccessDraftRefusesAGroupWithoutNameOrWithAnExistingOne(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "")
	ctx := context.Background()

	ai.access.Group = &api.GroupDraft{Name: ""}
	res, err := s.DraftAccess(ctx, admin, "a group", nil)
	require.NoError(t, err)
	assert.Nil(t, res.Group)
	assert.Equal(t, []string{"The group had no name, so it was left out."}, res.Refused)

	ai.access.Group = &api.GroupDraft{Name: "ops", Members: []string{"usr_ada"}}
	res, err = s.DraftAccess(ctx, admin, "a group", nil)
	require.NoError(t, err)
	assert.Nil(t, res.Group)
	assert.Equal(t, []string{`A group called "ops" already exists, so no new one was drafted. Add people to it instead.`}, res.Refused)
}

// TestDraftAccessPropagatesReadErrors asserts a failure reading what the
// draft is checked against stops the draft before the adapter is asked.
func TestDraftAccessPropagatesReadErrors(t *testing.T) {
	cases := map[string]func(s *Service){
		"verbs":  func(s *Service) { s.Verbs = fakeVerbs{err: errBoom} },
		"roles":  func(s *Service) { s.Roles = fakeRoles{err: errBoom} },
		"groups": func(s *Service) { s.Groups = fakeGroups{err: errBoom} },
		"users":  func(s *Service) { s.Users = fakeUsers{err: errBoom} },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			ai := &fakeAI{caps: doesAll()}
			s := newService(t, ai, "")
			breakIt(s)
			_, err := s.DraftAccess(context.Background(), admin, "let Ada deploy", nil)
			require.ErrorIs(t, err, errBoom)
			assert.Empty(t, ai.accessReq.Description, "the adapter was not asked")
		})
	}
}

// ---------------------------------------------------------------------------
// Policy (R-344)

const configFile = "/etc/pando/pando.yaml"

// policyService is a Service whose stored policy has a security floor, with
// disabled_verbs fixed by the environment and max_token_lifetime_days by the
// config file.
func policyService(t *testing.T, ai *fakeAI) *Service {
	t.Helper()
	s := newService(t, ai, "")
	overlay, err := policy.NewOverlay([]policy.Setting{
		{Key: "disabled_verbs", Value: "app.exec", Source: policy.Source{Kind: "env", Name: "PANDO_POLICY_DISABLED_VERBS"}},
		{Key: "max_token_lifetime_days", Value: 30, Source: policy.Source{Kind: "file", Name: configFile, Key: "policy.max_token_lifetime_days"}},
	})
	require.NoError(t, err)
	s.Overlay = overlay
	// The effective document, startup fields included.
	s.Policy = fakePolicy{doc: overlay.Apply(policy.Document{MinSecurityScore: 60})}
	return s
}

func changeKeys(cs []PolicyChange) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Key)
	}
	return out
}

// TestR271_PolicyDraftDeclinesFieldsSetAtStartup asserts a change to a field
// the startup configuration fixes is declined, citing the variable or file,
// whatever the adapter returned, and the adapter is told which fields those are.
func TestR271_PolicyDraftDeclinesFieldsSetAtStartup(t *testing.T) {
	ai := &fakeAI{caps: doesAll(), policy: api.PolicyDraft{Changes: map[string]json.RawMessage{
		"disabled_verbs":          json.RawMessage(`[]`),
		"max_token_lifetime_days": json.RawMessage(`365`),
	}}}
	s := policyService(t, ai)

	res, err := s.DraftPolicy(context.Background(), "let people exec and keep tokens a year", nil)
	require.NoError(t, err)

	require.Len(t, res.Declined, 2)
	assert.Equal(t, PolicyDeclined{
		Key: "disabled_verbs", Source: policy.Source{Kind: "env", Name: "PANDO_POLICY_DISABLED_VERBS"},
		Reason: "disabled_verbs is set by the environment variable PANDO_POLICY_DISABLED_VERBS and can't be changed here. " +
			"Unset it and restart Pando to manage it from the console.",
	}, res.Declined[0])
	assert.Equal(t, "max_token_lifetime_days", res.Declined[1].Key)
	assert.Equal(t, "max_token_lifetime_days is set in "+configFile+" (policy.max_token_lifetime_days) and can't be changed here. "+
		"Remove it from that file and restart Pando to manage it from the console.", res.Declined[1].Reason)

	assert.Empty(t, res.Changes)
	assert.NotNil(t, res.Changes, "no change is an empty list, not null")
	assert.Equal(t, []string{"app.exec"}, res.Proposed.DisabledVerbs)
	assert.Equal(t, 30, res.Proposed.MaxTokenLifetimeDays)

	fixed := map[string]bool{}
	for _, f := range ai.policyReq.Fields {
		fixed[f.Key] = f.Fixed
		assert.NotEmpty(t, f.Type, f.Key)
	}
	assert.True(t, fixed["disabled_verbs"])
	assert.True(t, fixed["max_token_lifetime_days"])
	assert.False(t, fixed["min_security_score"])
	assert.Nil(t, ai.policyReq.Draft, "no draft so far")
	assert.JSONEq(t, `{"disabled_verbs":["app.exec"],"max_token_lifetime_days":30,"min_security_score":60}`, string(ai.policyReq.Current))
}

// TestFixedReasonWithoutAKnownSource asserts a field fixed from a source that
// is neither a variable nor a file still gets a sentence.
func TestFixedReasonWithoutAKnownSource(t *testing.T) {
	assert.Equal(t, "egress_allowlist is set in Pando's startup configuration and can't be changed here.",
		fixedReason(policy.Fixed{Key: "egress_allowlist"}))
}

// TestR344_PolicyDraftRefusesWhatWouldNotRead asserts an unknown field, a
// value of the wrong type, and a verb list naming a verb that does not exist
// are refused with a reason, and only valid changes reach the proposal.
func TestR344_PolicyDraftRefusesWhatWouldNotRead(t *testing.T) {
	ai := &fakeAI{caps: doesAll(), policy: api.PolicyDraft{
		Changes: map[string]json.RawMessage{
			"agent_disabled_verbs": json.RawMessage(`["app.exec","app.teleport"]`),
			"egress_allowlist":     json.RawMessage(`["api.example.com"]`),
			"insecure_grace_hours": json.RawMessage(`"soon"`),
			"min_security_score":   json.RawMessage(`60`),
			"not_a_field":          json.RawMessage(`true`),
		},
		Reply: " Limited egress. ",
		Model: "house-model-7",
	}}
	s := policyService(t, ai)

	res, err := s.DraftPolicy(context.Background(), "only allow the payments API", nil)
	require.NoError(t, err)

	require.Len(t, res.Refused, 3)
	assert.Equal(t, `agent_disabled_verbs was left unchanged: "app.teleport" is not a permission Pando has.`, res.Refused[0])
	assert.Contains(t, res.Refused[1], "insecure_grace_hours was left unchanged: ")
	assert.Contains(t, res.Refused[1], "not a whole number")
	assert.Equal(t, `not_a_field was left unchanged: "not_a_field" is not a host policy setting.`, res.Refused[2])

	// min_security_score was proposed at its stored value, so it is no change.
	assert.Equal(t, []string{"egress_allowlist"}, changeKeys(res.Changes))
	assert.JSONEq(t, `null`, string(res.Changes[0].From))
	assert.JSONEq(t, `["api.example.com"]`, string(res.Changes[0].To))

	assert.Equal(t, []string{"api.example.com"}, res.Proposed.EgressAllowlist)
	assert.Empty(t, res.Proposed.AgentDisabledVerbs)
	assert.Equal(t, "Limited egress.", res.Reply)
	assert.Equal(t, "house-model-7", res.Model)
	assert.Empty(t, res.Declined)
}

// TestR344_AZeroValueForAnUnsetSettingIsNoChange asserts a proposal that sets
// an unset switch to false, or an unset number to 0, lists no change: the
// stored document omits zeros, and saving would change nothing.
func TestR344_AZeroValueForAnUnsetSettingIsNoChange(t *testing.T) {
	ai := &fakeAI{caps: doesAll(), policy: api.PolicyDraft{
		Changes: map[string]json.RawMessage{
			"require_backup_before_destroy": json.RawMessage(`false`),
			"insecure_grace_hours":          json.RawMessage(`0`),
			"disable_ai_screening":          json.RawMessage(`true`),
		},
	}}
	res, err := policyService(t, ai).DraftPolicy(context.Background(), "turn off AI screening", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"disable_ai_screening"}, changeKeys(res.Changes))
}

// TestBadVerbsChecksOnlyVerbLists asserts only the two verb lists are checked
// against the catalog, and a list of real verbs passes.
func TestBadVerbsChecksOnlyVerbLists(t *testing.T) {
	assert.Empty(t, badVerbs("disabled_verbs", json.RawMessage(`["app.exec","install.audit.read"]`)))
	assert.Empty(t, badVerbs("egress_allowlist", json.RawMessage(`["app.teleport"]`)))
	assert.Equal(t, `disabled_verbs was left unchanged: "app.teleport" is not a permission Pando has.`,
		badVerbs("disabled_verbs", json.RawMessage(`["app.teleport"]`)))
}

// TestR344_RefiningAPolicyDraftKeepsEarlierChanges asserts a refinement is
// applied to the draft so far, changes are still reported against the stored
// policy, and fixed fields the draft altered are put back.
func TestR344_RefiningAPolicyDraftKeepsEarlierChanges(t *testing.T) {
	ai := &fakeAI{caps: doesAll(), policy: api.PolicyDraft{Changes: map[string]json.RawMessage{
		"require_backup_before_destroy": json.RawMessage(`true`),
	}}}
	s := policyService(t, ai)

	soFar := policy.Document{
		SourceAllowlist:      []string{"github.com/acme/*"},
		DisabledVerbs:        []string{}, // a fixed field the draft tried to clear
		MaxTokenLifetimeDays: 30,
		MinSecurityScore:     60,
	}
	res, err := s.DraftPolicy(context.Background(), "and require a backup before deleting", &soFar)
	require.NoError(t, err)

	assert.JSONEq(t, `{"disabled_verbs":["app.exec"],"max_token_lifetime_days":30,"min_security_score":60,"source_allowlist":["github.com/acme/*"]}`,
		string(ai.policyReq.Draft), "the adapter sees the draft with startup fields restored")

	assert.Equal(t, []string{"require_backup_before_destroy", "source_allowlist"}, changeKeys(res.Changes))
	assert.Equal(t, []string{"app.exec"}, res.Proposed.DisabledVerbs)
	assert.Equal(t, []string{"github.com/acme/*"}, res.Proposed.SourceAllowlist)
	assert.True(t, res.Proposed.RequireBackupBeforeDestroy)
}

// TestDraftPolicyPropagatesLoadError asserts a policy that cannot be read
// stops the draft.
func TestDraftPolicyPropagatesLoadError(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := policyService(t, ai)
	s.Policy = fakePolicy{err: errBoom}

	_, err := s.DraftPolicy(context.Background(), "no exec", nil)
	require.ErrorIs(t, err, errBoom)
	assert.Empty(t, ai.policyReq.Description)
}

// ---------------------------------------------------------------------------
// Audit search (R-345)

func records(n int) []audit.Record {
	out := make([]audit.Record, n)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	for i := range out {
		out[i] = audit.Record{ID: int64(i + 1), OccurredAt: at, Action: "app.deploy", PrincipalKind: "user",
			PrincipalID: "usr_ada", AppID: "app_blog", TargetKind: "app", TargetID: "app_blog", Detail: map[string]any{"n": i}}
	}
	return out
}

// TestR345_AuditSearchRunsTheFilterAndSummarizes asserts core runs the
// adapter's filter itself, bounded, and hands the adapter a capped set of the
// records it found.
func TestR345_AuditSearchRunsTheFilterAndSummarizes(t *testing.T) {
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	ai := &fakeAI{caps: doesAll(),
		search: api.AuditSearch{Filter: api.AuditFilter{
			Actions: []string{"app.deploy"}, AppID: " app_blog ", PrincipalID: "ada", PrincipalKind: "user",
			TargetKind: "app", TargetID: "app_blog", Since: &since, Until: &until,
		}, Note: "  Deploys of blog.  ", Model: "house-model-7"},
		summary: api.AuditSummary{Summary: "  Ada deployed blog 250 times.  "},
	}
	s := newService(t, ai, "")
	a := &fakeAudit{records: records(250)}
	s.Audit = a
	now := time.Date(2026, 9, 26, 9, 0, 0, 0, time.FixedZone("x", 7200))
	s.Now = func() time.Time { return now }

	res, err := s.SearchAudit(context.Background(), admin, "what did ada deploy yesterday")
	require.NoError(t, err)

	assert.Equal(t, now.UTC(), ai.searchReq.Now)
	assert.Equal(t, audit.Actions, ai.searchReq.Actions, "the code's catalog, not a scan of the log (O-54)")
	assert.Equal(t, []api.PersonInfo{{ID: "usr_ada", Username: "ada", Name: "Ada Lovelace", Email: "Ada@Example.com"}},
		ai.searchReq.People, "the person the question names")
	assert.Empty(t, ai.searchReq.Apps, "no app matches the question's words")

	assert.Equal(t, audit.Query{
		Actions: []string{"app.deploy"}, AppID: "app_blog", PrincipalID: "usr_ada", PrincipalKind: "user",
		TargetKind: "app", TargetID: "app_blog", Since: since, Until: until, Limit: 200,
	}, a.query)

	assert.Equal(t, 250, res.Matched)
	assert.True(t, res.Truncated)
	assert.Equal(t, "Deploys of blog.", res.Note)
	assert.Equal(t, "Ada deployed blog 250 times.", res.Summary)
	assert.Equal(t, "house-model-7", res.Model)
	assert.Equal(t, "usr_ada", res.Filter.PrincipalID)

	require.Len(t, ai.summaryReq.Records, 100)
	assert.True(t, ai.summaryReq.Truncated)
	assert.Equal(t, time.UTC, ai.summaryReq.Records[0].At.Location())
	assert.Equal(t, "app_blog", ai.summaryReq.Records[0].AppID)
	assert.Equal(t, res.Filter, ai.summaryReq.Filter)

	// The summary names the people and apps the records hold, by ID.
	assert.Equal(t, []api.AppInfo{{ID: "app_blog", Name: "blog", Slug: "blog"}}, ai.summaryReq.Apps)
	require.Len(t, ai.summaryReq.People, 1)
	assert.Equal(t, "usr_ada", ai.summaryReq.People[0].ID)
}

// TestAuditSearchUnderTheLimitIsNotTruncated asserts a result smaller than the
// limits is reported whole.
func TestAuditSearchUnderTheLimitIsNotTruncated(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "")
	s.Audit = &fakeAudit{records: records(3)}

	res, err := s.SearchAudit(context.Background(), admin, "what happened")
	require.NoError(t, err)
	assert.Equal(t, 3, res.Matched)
	assert.False(t, res.Truncated)
	assert.Len(t, ai.summaryReq.Records, 3)
	assert.False(t, ai.summaryReq.Truncated)
}

// TestR345_AuditSearchResolvesPeopleByName asserts a person named by
// username, email or display name, in any case, is searched for by ID, and a
// name matching nobody is left as given. Each is one bounded search (O-54).
func TestR345_AuditSearchResolvesPeopleByName(t *testing.T) {
	s := newService(t, nil, "")
	calls := 0
	s.Users = fakeUsers{users: []state.User{
		{ID: "usr_ada", ExternalID: "ada", Email: "Ada@Example.com", DisplayName: "Ada Lovelace"},
		{ID: "usr_bob", ExternalID: "bob"},
	}, calls: &calls}
	v, err := s.sightOf(context.Background(), admin)
	require.NoError(t, err)
	for in, want := range map[string]string{
		"usr_ada":          "usr_ada",
		" ADA ":            "usr_ada",
		"ada@example.com":  "usr_ada",
		"ada lovelace":     "usr_ada",
		"Bob":              "usr_bob",
		"nobody":           "nobody",
		"":                 "",
		"usr_somebodyelse": "usr_somebodyelse",
	} {
		got, err := s.resolvePerson(context.Background(), v, in)
		require.NoError(t, err)
		assert.Equal(t, want, got, in)
	}
	assert.Equal(t, 5, calls, "an ID or an empty name is not searched for")

	// Someone who may not read accounts gets the name back as it is.
	s.Verbs = fakeVerbs{verbs: []string{"install.audit.read"}}
	v, err = s.sightOf(context.Background(), admin)
	require.NoError(t, err)
	got, err := s.resolvePerson(context.Background(), v, "ada")
	require.NoError(t, err)
	assert.Equal(t, "ada", got)

	ai := &fakeAI{caps: doesAll(), search: api.AuditSearch{Filter: api.AuditFilter{PrincipalID: "ADA", Involving: "bob"}}}
	s = newService(t, ai, "")
	res, err := s.SearchAudit(context.Background(), admin, "what did they do")
	require.NoError(t, err)
	assert.Equal(t, "usr_ada", res.Filter.PrincipalID)
	assert.Equal(t, "usr_bob", res.Filter.Involving)
}

// TestCleanFilterTrimsAndCaps asserts blank actions are dropped, at most ten
// are kept, and an unknown principal kind is cleared rather than searched.
func TestCleanFilterTrimsAndCaps(t *testing.T) {
	actions := []string{" ", ""}
	for i := 0; i < 12; i++ {
		actions = append(actions, " app.a"+string(rune('a'+i))+" ")
	}
	f, err := cleanFilter(api.AuditFilter{
		Actions: actions, PrincipalKind: "robot", TargetKind: " user ", TargetID: " usr_ada ",
	})
	require.NoError(t, err)
	require.Len(t, f.Actions, 10)
	assert.Equal(t, "app.aa", f.Actions[0])
	assert.Equal(t, "app.aj", f.Actions[9])
	assert.Empty(t, f.PrincipalKind)
	assert.Equal(t, "user", f.TargetKind)
	assert.Equal(t, "usr_ada", f.TargetID)

	for _, kind := range []string{"user", "token", "system", "anonymous"} {
		f, err := cleanFilter(api.AuditFilter{PrincipalKind: " " + kind})
		require.NoError(t, err)
		assert.Equal(t, kind, f.PrincipalKind)
	}
}

// TestAuditSearchRefusesATimeRangeThatEndsBeforeItStarts asserts a backwards
// or empty range is the adapter's failure, and the log is not searched.
func TestAuditSearchRefusesATimeRangeThatEndsBeforeItStarts(t *testing.T) {
	since := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	for name, until := range map[string]time.Time{
		"before": since.Add(-time.Hour),
		"equal":  since,
	} {
		t.Run(name, func(t *testing.T) {
			until := until
			ai := &fakeAI{caps: doesAll(), search: api.AuditSearch{Filter: api.AuditFilter{Since: &since, Until: &until}}}
			s := newService(t, ai, "")
			a := &fakeAudit{}
			s.Audit = a

			_, err := s.SearchAudit(context.Background(), admin, "what happened")
			e := requireCode(t, err, errs.AdapterFailed)
			assert.Contains(t, e.Message, "ends before it starts")
			assert.NotEmpty(t, e.Remedy)
			assert.False(t, a.listed)
			assert.False(t, ai.summaryRan)
		})
	}

	// A range with one end open is fine.
	f, err := cleanFilter(api.AuditFilter{Since: &since})
	require.NoError(t, err)
	assert.Nil(t, f.Until)
}

// TestSearchAuditPropagatesErrors asserts a failure reading people, apps,
// action names or the log itself stops the search, and a failed summary is
// the adapter's failure.
func TestSearchAuditPropagatesErrors(t *testing.T) {
	cases := map[string]func(s *Service){
		"users": func(s *Service) { s.Users = fakeUsers{err: errBoom} },
		"apps":  func(s *Service) { s.Apps = fakeApps{err: errBoom} },
		"list":  func(s *Service) { s.Audit = &fakeAudit{listErr: errBoom} },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			ai := &fakeAI{caps: doesAll()}
			s := newService(t, ai, "")
			breakIt(s)
			_, err := s.SearchAudit(context.Background(), admin, "what happened")
			require.ErrorIs(t, err, errBoom)
			assert.False(t, ai.summaryRan)
		})
	}

	t.Run("summary", func(t *testing.T) {
		s := newService(t, &fakeAI{caps: doesAll(), summaryErr: errors.New("overloaded")}, "")
		_, err := s.SearchAudit(context.Background(), admin, "what happened")
		e := requireCode(t, err, errs.AdapterFailed)
		assert.Equal(t, "The AI adapter could not finish audit search: overloaded", e.Message)
	})
}

// ---------------------------------------------------------------------------
// Reference (R-346)

// TestR346_ReferenceAnswerKeepsOnlyCitationsInTheReference asserts a citation
// the reference does not contain is dropped, and the reference is what the
// adapter is given.
func TestR346_ReferenceAnswerKeepsOnlyCitationsInTheReference(t *testing.T) {
	ref := "## POST /api/v1/apps\nCreates an app.\n## pando deploy\nDeploys it.\n"
	ai := &fakeAI{caps: doesAll(), answer: api.ReferenceAnswer{
		Answer:  "  Use POST /api/v1/apps.  ",
		Cites:   []string{" POST /api/v1/apps ", "GET /api/v1/made-up", "  ", "pando deploy"},
		Covered: true,
	}}
	s := newService(t, ai, "")
	s.Reference = func() string { return ref }

	res, err := s.AnswerReference(context.Background(), "how do I create an app")
	require.NoError(t, err)
	assert.Equal(t, ref, ai.refReq.Reference)
	assert.Equal(t, "Use POST /api/v1/apps.", res.Answer)
	assert.Equal(t, []string{"POST /api/v1/apps", "pando deploy"}, res.Cites)
	assert.True(t, res.Covered)
}

// TestR346_ReferenceAnswerWithoutCitationsIsAnEmptyList asserts no citation,
// or no reference at all, reports an empty list rather than null.
func TestR346_ReferenceAnswerWithoutCitationsIsAnEmptyList(t *testing.T) {
	ai := &fakeAI{caps: doesAll(), answer: api.ReferenceAnswer{Answer: "Pando does not do that.", Cites: []string{"POST /api/v1/apps"}}}
	s := newService(t, ai, "")
	s.Reference = nil

	res, err := s.AnswerReference(context.Background(), "how do I mine bitcoin")
	require.NoError(t, err)
	assert.Empty(t, ai.refReq.Reference)
	assert.NotNil(t, res.Cites)
	assert.Empty(t, res.Cites)
	assert.False(t, res.Covered)

	raw, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"cites":[]`)
}
