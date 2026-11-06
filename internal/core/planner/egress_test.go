package planner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

func egressRuntime() *fakeRuntime {
	rt := capableRuntime()
	rt.caps.SupportsEgressRestriction = true
	return rt
}

func plannerWith(t *testing.T, doc policy.Document, rt *fakeRuntime) *planner.Planner {
	t.Helper()
	return planner.New(registry(t, rt, capableRouting(), capableBuilder()), policy.Static(doc), fixedAllocations{})
}

func allowlistPolicy(rule policy.EgressLoosening) policy.Document {
	doc := policy.Default()
	doc.EgressMode = spec.EgressAllowlist
	doc.EgressList = []string{"api.github.com"}
	doc.EgressLoosening = rule
	return doc
}

func hasNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// TestR183_AForbiddenLooseningBlocksThePlan asserts R-183: under a policy
// that forbids loosening, a deploy carrying one is refused at plan time, and
// the refusal names the entries.
func TestR183_AForbiddenLooseningBlocksThePlan(t *testing.T) {
	s := plannableSpec()
	s.Egress = spec.Egress{Add: []string{"api.openai.example"}}

	_, err := plannerWith(t, allowlistPolicy(policy.EgressLooseningForbidden), egressRuntime()).Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.PlanEgressLooseningForbidden, errs.CodeOf(err))
	e := errs.As(err)
	require.Contains(t, e.Message, "api.openai.example", "R-183: the refusal says which entries")
	require.NotEmpty(t, e.Remedy)
	require.NotNil(t, e.Details["loosenings"])

	// The same spec plans under verb and approval: neither is a plan-time
	// refusal.
	for _, rule := range []policy.EgressLoosening{policy.EgressLooseningVerb, policy.EgressLooseningApproval} {
		plan, err := plannerWith(t, allowlistPolicy(rule), egressRuntime()).Check(context.Background(), s)
		require.NoError(t, err, string(rule))
		require.Equal(t, rule, plan.Egress.Gate)
	}

	// Tightening is never refused, whatever policy says.
	s.Egress = spec.Egress{Remove: []string{"api.github.com"}, BlockPrivate: ptrBool(true)}
	_, err = plannerWith(t, allowlistPolicy(policy.EgressLooseningForbidden), egressRuntime()).Check(context.Background(), s)
	require.NoError(t, err)
}

// TestR186_RestrictedRulesNeedARuntimeThatEnforcesThem asserts R-186: a
// runtime that cannot enforce egress rules refuses a plan that restricts
// anything, and plans an unrestricted app exactly as before.
func TestR186_RestrictedRulesNeedARuntimeThatEnforcesThem(t *testing.T) {
	cannot := capableRuntime() // SupportsEgressRestriction false

	// The installation restricts.
	_, err := plannerWith(t, allowlistPolicy(""), cannot).Check(context.Background(), plannableSpec())
	require.Error(t, err)
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	e := errs.As(err)
	require.Equal(t, "egress_restriction", e.Details["capability"])
	require.Contains(t, e.Message, "rt_docker")
	require.NotEmpty(t, e.Remedy)

	// The app restricts itself.
	s := plannableSpec()
	s.Egress = spec.Egress{BlockPrivate: ptrBool(true)}
	_, err = plannerWith(t, policy.Default(), cannot).Check(context.Background(), s)
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))

	// Nothing restricted: plans, with nothing in the app's path.
	plan, err := plannerWith(t, policy.Default(), cannot).Check(context.Background(), plannableSpec())
	require.NoError(t, err)
	require.False(t, plan.Egress.Restricted)
	require.Empty(t, plan.Bundle.Network.Egress.Layers)
	require.False(t, plan.Bundle.Network.Egress.BlockPrivate)
	require.False(t, hasNote(plan.Notes, "HTTP_PROXY"), "no gateway, so no note about one")

	// A denylist with everything removed restricts nothing either.
	doc := policy.Default()
	doc.EgressMode = spec.EgressDenylist
	doc.EgressList = []string{"evil.example"}
	s = plannableSpec()
	s.Egress = spec.Egress{Remove: []string{"evil.example"}}
	_, err = plannerWith(t, doc, cannot).Check(context.Background(), s)
	require.NoError(t, err)
}

// TestR188_ThePlanCarriesTheMergedRules asserts R-187 and R-188: the plan
// shows the rules the app would run with, merged, with where each part came
// from, says that only proxied traffic leaves, and gives the runtime the
// same rules it shows.
func TestR188_ThePlanCarriesTheMergedRules(t *testing.T) {
	s := plannableSpec()
	s.Egress = spec.Egress{
		Add:    []string{"api.openai.example"},
		Remove: []string{"not-on-the-list.example"},
		Mode:   spec.EgressDenylist, List: []string{"api.github.com:80"},
	}

	plan, err := plannerWith(t, allowlistPolicy(policy.EgressLooseningApproval), egressRuntime()).Check(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, "ok", plan.Checks["egress"])

	require.Equal(t, spec.EgressAllowlist, plan.Egress.Mode)
	require.Equal(t, []policy.EgressEntry{
		{Entry: "api.github.com", From: "install"},
		{Entry: "api.openai.example", From: "app"},
	}, plan.Egress.List)
	require.Equal(t, spec.EgressDenylist, plan.Egress.AppMode)
	require.Len(t, plan.Egress.Loosenings, 1)
	require.Equal(t, policy.EgressLooseningApproval, plan.Egress.Gate)
	require.True(t, plan.Egress.Restricted)

	require.True(t, hasNote(plan.Notes, "HTTP_PROXY"), "R-187: said wherever a restriction is in effect")
	require.True(t, hasNote(plan.Notes, "needs approval"), "R-188: what the loosening needs")
	require.True(t, hasNote(plan.Notes, "not-on-the-list.example"), "an entry that changes nothing is a note, never a blocker")

	require.Equal(t, plan.Egress.Rules, plan.Bundle.Network.Egress, "what is shown is what is enforced")
}

// TestR183_APolicyThatForbidsARunningLooseningIsPreviewed asserts R-183 with
// O-10: an administrator about to forbid loosening is told which apps run
// with one, in the words their next deploy will be refused with.
func TestR183_APolicyThatForbidsARunningLooseningIsPreviewed(t *testing.T) {
	loosens := appNamed(t, "notes", "https://github.com/acme/notes", false)
	loosens.Spec.Egress = spec.Egress{Add: []string{"api.openai.example"}}
	plain := appNamed(t, "wiki", "https://github.com/acme/wiki", false)

	p := planner.New(
		registry(t, egressRuntime(), capableRouting(), capableBuilder()),
		policy.Static(policy.Default()), fixedAllocations{},
	).WithInventory(staticInventory{loosens, plain})

	violations, err := p.PreviewPolicy(context.Background(), allowlistPolicy(policy.EgressLooseningForbidden))
	require.NoError(t, err)
	require.Len(t, violations, 1)
	require.Equal(t, "notes", violations[0].AppName)
	require.Equal(t, string(errs.PlanEgressLooseningForbidden), violations[0].Code)
	require.Contains(t, violations[0].Message, "api.openai.example")

	// Approval is not a block, so it is not listed.
	violations, err = p.PreviewPolicy(context.Background(), allowlistPolicy(policy.EgressLooseningApproval))
	require.NoError(t, err)
	require.Empty(t, violations)
}

// TestR186_APolicyThatRestrictsOnARuntimeThatCannotIsPreviewed asserts R-186
// with O-10: turning on an egress list where the runtime cannot enforce one
// names every app it would stop deploying.
func TestR186_APolicyThatRestrictsOnARuntimeThatCannotIsPreviewed(t *testing.T) {
	inv := staticInventory{appNamed(t, "notes", "https://github.com/acme/notes", false)}

	violations, err := previewer(t, inv).PreviewPolicy(context.Background(), allowlistPolicy(""))
	require.NoError(t, err)
	require.Len(t, violations, 1)
	require.Equal(t, string(errs.PlanCapabilityUnsupported), violations[0].Code)
}

func ptrBool(v bool) *bool { return &v }

// TestR097_ATrialIsHeldToTheAppsEgressWithoutItsLoosenings asserts R-097 as
// amended: a trial runs under the rules the app would run with, resolved
// against host policy, and a loosening the spec asks for is not granted to it
// (R-183) — the installation's rules stand instead.
func TestR097_ATrialIsHeldToTheAppsEgressWithoutItsLoosenings(t *testing.T) {
	ctx := context.Background()
	p := plannerWith(t, allowlistPolicy(policy.EgressLooseningVerb), egressRuntime())

	// No spec yet: the installation's rules.
	install, err := p.TrialEgress(ctx, nil)
	require.NoError(t, err)
	require.True(t, install.Restricted())

	// A spec that tightens is tried as tightened.
	tight := plannableSpec()
	tight.Egress = spec.Egress{BlockPrivate: ptrBool(true)}
	got, err := p.TrialEgress(ctx, tight)
	require.NoError(t, err)
	want, err := p.Egress(ctx, tight)
	require.NoError(t, err)
	require.Equal(t, want.Rules, got)
	require.True(t, got.BlockPrivate)

	// A spec that loosens is tried under the installation's rules.
	loose := plannableSpec()
	loose.Egress = spec.Egress{Add: []string{"api.openai.example"}}
	got, err = p.TrialEgress(ctx, loose)
	require.NoError(t, err)
	require.Equal(t, install, got)

	// Open egress, the default, restricts nothing.
	open, err := plannerWith(t, policy.Default(), egressRuntime()).TrialEgress(ctx, nil)
	require.NoError(t, err)
	require.False(t, open.Restricted())
}

// A policy that cannot be read is an error, never rules that restrict nothing.
func TestR097_TrialEgressFailsWhenPolicyCannotBeRead(t *testing.T) {
	p := planner.New(nil, policy.New(func(context.Context) (policy.Document, error) {
		return policy.Document{}, errs.New(errs.Internal, "unreadable")
	}), fixedAllocations{})
	_, err := p.TrialEgress(context.Background(), nil)
	require.Error(t, err)
}

// A policy that stops being readable between the spec's rules and the
// installation's is an error too, never the spec's loosened rules.
func TestR097_TrialEgressFailsWhenTheInstallationsRulesCannotBeRead(t *testing.T) {
	doc := allowlistPolicy(policy.EgressLooseningVerb)
	reads := 0
	p := planner.New(nil, policy.New(func(context.Context) (policy.Document, error) {
		reads++
		if reads > 1 {
			return policy.Document{}, errs.New(errs.Internal, "unreadable")
		}
		return doc, nil
	}), fixedAllocations{})
	loose := plannableSpec()
	loose.Egress = spec.Egress{Add: []string{"api.openai.example"}}
	_, err := p.TrialEgress(context.Background(), loose)
	require.Error(t, err)
}

// TestR431_APlanDeclaringAWebhookRouteIsRefusedWhenPolicyTurnsThemOff asserts
// the refusal arrives at plan time.
func TestR431_APlanDeclaringAWebhookRouteIsRefusedWhenPolicyTurnsThemOff(t *testing.T) {
	s := plannableSpec()
	s.WebhookRoutes = []spec.WebhookRoute{{Name: "noodle", Method: "POST", Path: "/hook",
		Verification: spec.WebhookVerification{Kind: spec.WebhookHeader, Header: "X-Token"}}}
	doc := policy.Default()
	doc.DisableWebhookRoutes = true

	_, err := plannerWith(t, doc, egressRuntime()).Check(context.Background(), s)
	require.Equal(t, errs.PlanWebhookRoutesDisabled, errs.CodeOf(err))
}
