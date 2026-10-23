package specgate_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/approval"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/specgate"
	"github.com/trypando/pando/internal/errs"
)

const appID = "app_01HQ8"

// store is the authorizer's view of one app with one grant per user, and the
// built-in roles' egress verbs (R-184): Owner holds both, Operator tighten.
type store struct{ roles map[string]authz.Role }

func (s store) UserStatus(context.Context, string) (string, error) { return "active", nil }
func (s store) ControlGrantsFor(_ context.Context, app string, p authz.Principal) ([]authz.Grant, error) {
	if app != appID {
		return nil, nil
	}
	if _, ok := s.roles[p.UserID]; !ok {
		return nil, nil
	}
	return []authz.Grant{{AppID: app, Plane: "control", PrincipalKind: "user", PrincipalID: p.UserID, RoleID: p.UserID}}, nil
}
func (s store) InstallGrantsFor(context.Context, authz.Principal) ([]authz.Grant, error) {
	return nil, nil
}
func (s store) IsOwner(context.Context, string, string) (bool, error) { return false, nil }
func (s store) HasDataGrant(context.Context, string, authz.Principal) (bool, error) {
	return false, nil
}
func (s store) AnonymousAccess(context.Context, string) (bool, bool, error)    { return false, false, nil }
func (s store) PasscodeUnlocked(context.Context, string, string) (bool, error) { return false, nil }
func (s store) Role(_ context.Context, id string) (authz.Role, error)          { return s.roles[id], nil }

type auditor struct{ denied []authz.Verb }

func (a *auditor) Denied(_ context.Context, _ authz.Principal, _ string, v authz.Verb, _ errs.Code) {
	a.denied = append(a.denied, v)
}

func (a *auditor) ThroughInstall(context.Context, authz.Principal, string, authz.Verb, authz.Verb, authz.Grant) {
}

func newAuthorizer() (*authz.Authorizer, *auditor) {
	base := []authz.Verb{authz.AppView, authz.AppSpecEdit, authz.AppDeploy}
	with := func(vs ...authz.Verb) authz.Role {
		return authz.Role{Verbs: append(append([]authz.Verb{}, base...), vs...)}
	}
	a := &auditor{}
	return authz.New(store{roles: map[string]authz.Role{
		"owner":      with(authz.AppEgressTighten, authz.AppEgressLoosen, authz.AppResourceOverride),
		"operator":   with(authz.AppEgressTighten),
		"loosenonly": with(authz.AppEgressLoosen),
		"editor":     with(),
	}}, nil, a), a
}

func user(id string) authz.Principal {
	return authz.Principal{Kind: authz.KindUser, ID: id, UserID: id, Status: "active"}
}

func withEgress(e spec.Egress) *spec.AppSpec {
	return &spec.AppSpec{AppID: appID, Egress: e}
}

func allowlist(rule policy.EgressLoosening) policy.Document {
	return policy.Document{EgressMode: spec.EgressAllowlist, EgressList: []string{"api.github.com"}, EgressLoosening: rule}
}

var (
	tighten = spec.Egress{Remove: []string{"api.github.com"}}
	loosen  = spec.Egress{Add: []string{"api.openai.example"}}
)

func check(t *testing.T, who string, doc policy.Document, pinned, next *spec.AppSpec) (specgate.Result, *auditor, error) {
	t.Helper()
	az, a := newAuthorizer()
	res, err := specgate.Check(context.Background(), az, user(who), specgate.Change{AppID: appID, Policy: doc, Pinned: pinned, Next: next})
	return res, a, err
}

// TestR184_TighteningNeedsEitherEgressVerb asserts R-184: any change to an
// app's egress needs app.egress.tighten or app.egress.loosen, and holding
// loosen covers it without a denial of tighten being audited.
func TestR184_TighteningNeedsEitherEgressVerb(t *testing.T) {
	pinned := withEgress(spec.Egress{})
	next := withEgress(tighten)

	for _, who := range []string{"owner", "operator", "loosenonly"} {
		res, a, err := check(t, who, allowlist(""), pinned, next)
		require.NoError(t, err, who)
		require.True(t, res.EgressChanged)
		require.Empty(t, res.Loosenings)
		require.Empty(t, a.denied, "%s: nothing denied, so nothing audited", who)
	}

	_, a, err := check(t, "editor", allowlist(""), pinned, next)
	require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err))
	e := errs.As(err)
	require.Contains(t, e.Message, "app.egress.tighten")
	require.Contains(t, e.Message, "app.egress.loosen")
	require.NotEmpty(t, e.Remedy)
	require.Equal(t, []authz.Verb{authz.AppEgressTighten}, a.denied, "the refusal is audited (design 06 §6)")
}

// TestR184_AnUnchangedEgressAsksNothing asserts that writing a spec whose
// egress did not change needs neither egress verb, however the egress is
// spelled — including the shape from before issue #79, and a loosening the
// pinned spec already carries under a policy that now forbids it (the plan
// refuses that deploy, R-183; saving other changes is not refused here).
func TestR184_AnUnchangedEgressAsksNothing(t *testing.T) {
	old := withEgress(spec.Egress{Mode: spec.EgressAllowlist, Allowlist: []string{"api.example.com"}})
	same := withEgress(spec.Egress{Mode: spec.EgressAllowlist, List: []string{"api.example.com"}})
	res, _, err := check(t, "editor", allowlist(policy.EgressLooseningForbidden), old, same)
	require.NoError(t, err)
	require.False(t, res.EgressChanged)

	res, _, err = check(t, "editor", allowlist(policy.EgressLooseningForbidden), withEgress(loosen), withEgress(loosen))
	require.NoError(t, err)
	require.False(t, res.EgressChanged)

	// A first spec that leaves egress alone is not a change either.
	res, _, err = check(t, "editor", allowlist(""), nil, withEgress(spec.Egress{}))
	require.NoError(t, err)
	require.False(t, res.EgressChanged)
}

// TestR184_LooseningUnderEachGate asserts R-183 and R-184 across the three
// settings of egress_loosening, for Owner and Operator.
func TestR184_LooseningUnderEachGate(t *testing.T) {
	pinned := withEgress(spec.Egress{})
	next := withEgress(loosen)

	// verb: Owner may; Operator may not, and is told which entries and which
	// verb.
	res, _, err := check(t, "owner", allowlist(policy.EgressLooseningVerb), pinned, next)
	require.NoError(t, err)
	require.Len(t, res.Loosenings, 1)
	require.False(t, res.NeedsApproval)

	_, a, err := check(t, "operator", allowlist(policy.EgressLooseningVerb), pinned, next)
	require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err))
	e := errs.As(err)
	require.Contains(t, e.Message, "app.egress.loosen")
	require.Contains(t, e.Message, "api.openai.example", "R-105: names the entry")
	require.NotContains(t, e.Message, "Error")
	require.Equal(t, []authz.Verb{authz.AppEgressLoosen}, a.denied,
		"loosen is asked unaudited first; only the refusal that stands is audited")

	// The default is verb.
	_, _, err = check(t, "operator", allowlist(""), pinned, next)
	require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err))

	// approval: anybody who may change egress may propose it; the deploy
	// waits for approval.
	for _, who := range []string{"owner", "operator"} {
		res, _, err := check(t, who, allowlist(policy.EgressLooseningApproval), pinned, next)
		require.NoError(t, err, who)
		require.True(t, res.NeedsApproval, who)
	}
	_, _, err = check(t, "editor", allowlist(policy.EgressLooseningApproval), pinned, next)
	require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err), "proposing still needs an egress verb")

	// forbidden: nobody may, Owner included.
	for _, who := range []string{"owner", "operator", "loosenonly"} {
		_, _, err := check(t, who, allowlist(policy.EgressLooseningForbidden), pinned, next)
		require.Equal(t, errs.PlanEgressLooseningForbidden, errs.CodeOf(err), who)
		e := errs.As(err)
		require.Contains(t, e.Message, "api.openai.example", who)
		require.NotEmpty(t, e.Remedy)
		require.NotNil(t, e.Details["loosenings"])
	}

	// Tightening under forbidden is fine for Operator.
	_, _, err = check(t, "operator", allowlist(policy.EgressLooseningForbidden), pinned, withEgress(tighten))
	require.NoError(t, err)
}

// TestR184_OnlyNewLooseningsAreGated asserts R-183: a loosening the pinned
// spec already carries was allowed when it was written. Operator may edit an
// app that runs with one, and may not add another.
func TestR184_OnlyNewLooseningsAreGated(t *testing.T) {
	pinned := withEgress(loosen)

	keep := withEgress(spec.Egress{Add: loosen.Add, BlockPrivate: ptr(true)})
	res, _, err := check(t, "operator", allowlist(policy.EgressLooseningVerb), pinned, keep)
	require.NoError(t, err)
	require.True(t, res.EgressChanged)
	require.Empty(t, res.Loosenings)

	more := withEgress(spec.Egress{Add: append([]string{"api.anthropic.example"}, loosen.Add...)})
	_, _, err = check(t, "operator", allowlist(policy.EgressLooseningVerb), pinned, more)
	require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "api.anthropic.example")
	require.NotContains(t, errs.As(err).Message, "api.openai.example", "only what is new is named")

	// A loosening against private-range blocking, on an install that has it.
	doc := policy.Document{EgressBlockPrivate: true}
	_, _, err = check(t, "operator", doc, withEgress(spec.Egress{}), withEgress(spec.Egress{BlockPrivate: ptr(false)}))
	require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err))
	_, _, err = check(t, "owner", doc, withEgress(spec.Egress{}), withEgress(spec.Egress{BlockPrivate: ptr(false)}))
	require.NoError(t, err)
}

// TestR158_AutoDeployIsRefusedWhileApprovalIsRequired asserts R-158: a spec
// that turns on auto-deploy is refused while the app's deploys need approval,
// and the refusal says why and what to do.
func TestR158_AutoDeployIsRefusedWhileApprovalIsRequired(t *testing.T) {
	auto := &spec.AppSpec{AppID: appID}
	auto.Deploy.AutoDeploy = spec.AutoDeploy{Enabled: true, Trigger: spec.TriggerBranchUpdated}

	_, _, err := check(t, "owner", policy.Default(), nil, auto)
	require.NoError(t, err, "no approval, no refusal")

	for _, tc := range []struct {
		doc    policy.Document
		reason approval.Reason
	}{
		{policy.Document{DeployApprovalRequired: true}, approval.ReasonInstall},
		{policy.Document{DeployApprovalApps: []string{appID}}, approval.ReasonAppPolicy},
	} {
		_, _, err := check(t, "owner", tc.doc, nil, auto)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
		e := errs.As(err)
		require.Contains(t, e.Message, "approval")
		require.Contains(t, e.Message, tc.reason.Message(), "says why")
		require.Contains(t, e.Remedy, "deploy.auto_deploy.enabled", "says what to do")
		require.Contains(t, e.Remedy, "administrator")
		require.Equal(t, []approval.Reason{tc.reason}, e.Details["reasons"])
	}

	// Another app on the list does not refuse this one.
	_, _, err = check(t, "owner", policy.Document{DeployApprovalApps: []string{"app_other"}}, nil, auto)
	require.NoError(t, err)

	// A loosening that needs approval is approved once, with the deploy that
	// brings it, and does not stop auto-deploy.
	auto.Egress = loosen
	_, _, err = check(t, "owner", allowlist(policy.EgressLooseningApproval), nil, auto)
	require.NoError(t, err)

	// Auto-deploy off is never refused.
	off := &spec.AppSpec{AppID: appID}
	_, _, err = check(t, "owner", policy.Document{DeployApprovalRequired: true}, nil, off)
	require.NoError(t, err)
}

// TestR154_ThePlanSaysWhetherApprovalIsNeeded asserts R-154 for what the
// plan shows.
func TestR154_ThePlanSaysWhetherApprovalIsNeeded(t *testing.T) {
	none := specgate.ApprovalFor(policy.Default(), appID, nil, &spec.AppSpec{AppID: appID})
	require.False(t, none.Required)
	require.NotNil(t, none.Reasons, "an empty list, not null")

	got := specgate.ApprovalFor(policy.Document{DeployApprovalRequired: true}, appID, nil, &spec.AppSpec{AppID: appID})
	require.True(t, got.Required)
	require.Equal(t, []specgate.ApprovalReason{{Reason: approval.ReasonInstall, Message: approval.ReasonInstall.Message()}}, got.Reasons)
}

func ptr[T any](v T) *T { return &v }

// TestR184_ADryRunRefusesWithoutAuditing asserts specgate.Quiet: the same
// refusal a save gets, in the same words, with nothing written to the audit
// log, because a dry run attempts nothing.
func TestR184_ADryRunRefusesWithoutAuditing(t *testing.T) {
	ctx := context.Background()
	pinned := withEgress(spec.Egress{})

	for _, c := range []struct {
		who  string
		next *spec.AppSpec
		verb string
	}{
		{"editor", withEgress(tighten), "app.egress.tighten"},
		{"operator", withEgress(loosen), "app.egress.loosen"},
	} {
		az, a := newAuthorizer()
		_, saved := specgate.Check(ctx, az, user(c.who), specgate.Change{AppID: appID, Policy: allowlist(""), Pinned: pinned, Next: c.next})
		require.Equal(t, errs.PermVerbRequired, errs.CodeOf(saved), c.who)
		require.NotEmpty(t, a.denied, "a save's refusal is audited")

		az, a = newAuthorizer()
		_, dry := specgate.Check(ctx, specgate.Quiet(az), user(c.who), specgate.Change{AppID: appID, Policy: allowlist(""), Pinned: pinned, Next: c.next})
		require.Equal(t, errs.CodeOf(saved), errs.CodeOf(dry), c.who)
		require.Equal(t, errs.As(saved).Message, errs.As(dry).Message, c.who)
		require.Contains(t, errs.As(dry).Message, c.verb)
		require.Empty(t, a.denied, "%s: a dry run audits nothing", c.who)
	}
}

func withLimits(r spec.Resources, workloads ...spec.Workload) *spec.AppSpec {
	return &spec.AppSpec{AppID: appID, Resources: r, Workloads: workloads}
}

var hostDefaults = spec.Resources{CPUMillis: 1000, MemoryBytes: 512 << 20, DiskBytes: 5 << 30}

func checkLimits(t *testing.T, who string, c specgate.Change) (*auditor, error) {
	t.Helper()
	az, a := newAuthorizer()
	c.AppID = appID
	_, err := specgate.Check(context.Background(), az, user(who), c)
	return a, err
}

// TestR241_ChangingLimitsNeedsTheOverrideVerb asserts R-241 and R-241a in the
// gate every spec write goes through: raising the app's memory, or giving one
// workload a limit of its own, needs app.resources.override, which Operator
// lacks and Owner holds; the refusal names the verb and is audited.
func TestR241_ChangingLimitsNeedsTheOverrideVerb(t *testing.T) {
	pinned := withLimits(hostDefaults, spec.Workload{Name: "web"})
	raised := hostDefaults
	raised.MemoryBytes = 2 << 30
	changes := map[string]*spec.AppSpec{
		"app memory":     withLimits(raised, spec.Workload{Name: "web"}),
		"workload limit": withLimits(hostDefaults, spec.Workload{Name: "web", Resources: &spec.ResourceLimits{MemoryBytes: 256 << 20}}),
	}
	for name, next := range changes {
		a, err := checkLimits(t, "operator", specgate.Change{Pinned: pinned, Next: next, HostResources: hostDefaults})
		require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err), name)
		e := errs.As(err)
		require.Contains(t, e.Message, "app.resources.override", name)
		require.Contains(t, e.Message, "resource limits", name)
		require.NotEmpty(t, e.Remedy, name)
		require.Equal(t, []authz.Verb{authz.AppResourceOverride}, a.denied, "%s: the refusal is audited", name)

		a, err = checkLimits(t, "owner", specgate.Change{Pinned: pinned, Next: next, HostResources: hostDefaults})
		require.NoError(t, err, name)
		require.Empty(t, a.denied, name)
	}
}

// TestR241_UnchangedLimitsAskNothing asserts that a revision which leaves the
// limits as they are needs no app.resources.override: the first one, written
// with the host's defaults; an ordinary edit; one keeping limits the newest
// revision already sets; and one going back to the host's defaults.
func TestR241_UnchangedLimitsAskNothing(t *testing.T) {
	raised := hostDefaults
	raised.MemoryBytes = 2 << 30
	raised.Overridden = true
	cases := map[string]specgate.Change{
		"first revision": {Next: withLimits(hostDefaults, spec.Workload{Name: "web"}), HostResources: hostDefaults},
		"ordinary edit": {
			Pinned: withLimits(raised, spec.Workload{Name: "web", Image: "nginx:1"}),
			Next:   withLimits(raised, spec.Workload{Name: "web", Image: "nginx:2"}),
		},
		"kept from the newest revision": {
			Pinned: withLimits(hostDefaults), Latest: withLimits(raised), Next: withLimits(raised),
			HostResources: hostDefaults,
		},
		"back to the host's defaults": {
			Pinned: withLimits(raised), Next: withLimits(hostDefaults), HostResources: hostDefaults,
		},
	}
	for name, c := range cases {
		a, err := checkLimits(t, "editor", c)
		require.NoError(t, err, name)
		require.Empty(t, a.denied, name)
	}

	// The host's defaults with a workload limited on its own is an override.
	_, err := checkLimits(t, "editor", specgate.Change{
		Pinned: withLimits(raised), HostResources: hostDefaults,
		Next: withLimits(hostDefaults, spec.Workload{Name: "web", Resources: &spec.ResourceLimits{CPUMillis: 100}}),
	})
	require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err))
}
