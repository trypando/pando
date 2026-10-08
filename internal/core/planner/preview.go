package planner

import (
	"context"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Inventory is every live app, with what a policy preview needs to judge it.
//
// An interface because the planner does not import state (design 03 §9 keeps
// that boundary in one direction) and because previewing a policy against a
// list is the only thing this needs — not a store.
type Inventory interface {
	// LiveApps lists the live apps f admits. Every app a candidate policy
	// would block must be among them; others may be too.
	LiveApps(ctx context.Context, f InventoryFilter) ([]InventoryApp, error)
}

// InventoryFilter is which live apps a candidate policy could block, worked
// out from the policy and the adapters before any app is read (issue #72).
//
// A preview runs while an administrator edits the policy form, and reading
// every app's pinned spec for it is twenty thousand specs a keystroke. Most of
// a policy cannot block most apps: no allowlist blocks no source, sharing
// allowed blocks no public app, a runtime that meets the floor fails no app's
// floor unless the app asks for more. Each field below names apps one check
// could fail; an app no field names passes every check, so reading only the
// named ones gives the same answer as reading them all. The checks themselves
// still run on every app read — the filter only decides which are read, never
// what is reported.
type InventoryFilter struct {
	// Every live app with a source: a source allowlist is in force.
	All bool

	// Apps shared with anyone on the internet; WithoutPasscode narrows that to
	// those shared without a passcode.
	Anonymous                bool
	AnonymousWithoutPasscode bool

	// EgressRuntimes are the runtimes that cannot enforce egress rules. An app
	// on one is named when the candidate restricts every app's egress
	// (EgressRestrictsAll), or when the app has egress settings of its own.
	EgressRuntimes     []string
	EgressRestrictsAll bool

	// OwnEgress names every app with egress settings of its own: the
	// candidate forbids loosening, and only an app's own settings loosen.
	OwnEgress bool

	// Runtimes and Builders are adapter isolation classes. An app is named
	// when its runtime's class is below the higher of its own runtime floor
	// and MinRuntime, or its builder's is below the higher of its own build
	// floor and MinBuild.
	Runtimes   map[string]spec.IsolationClass
	MinRuntime spec.IsolationClass
	Builders   map[string]spec.IsolationClass
	MinBuild   spec.IsolationClass
}

// Empty reports whether f names no app at all.
func (f InventoryFilter) Empty() bool {
	return !f.All && !f.Anonymous && !f.OwnEgress && len(f.EgressRuntimes) == 0 &&
		len(f.Runtimes) == 0 && len(f.Builders) == 0
}

// InventoryApp is one running app as a policy sees it.
type InventoryApp struct {
	AppID string
	Name  string

	// Spec is the revision the app is pinned to — what its next deploy would
	// actually try to do, not its newest draft.
	Spec *spec.AppSpec

	// AnonymousGrant is whether anyone on the internet can reach this app
	// without signing in (R-075, R-077), and AnonymousPasscode whether they
	// need its passcode to (R-075a).
	AnonymousGrant    bool
	AnonymousPasscode bool
}

// PolicyViolation is one app a candidate policy would block.
type PolicyViolation struct {
	AppID   string `json:"app_id"`
	AppName string `json:"app_name"`

	// Code is the error the app's next deploy would fail with, so the console
	// can show the same wording the developer will eventually see.
	Code    string `json:"code"`
	Message string `json:"message"`
	Remedy  string `json:"remedy,omitempty"`
}

// PreviewPolicy reports which apps a candidate policy would block (design 05 §3).
//
// O-10 resolved this to "report now, block on next deploy": saving a policy
// touches no running app, and each violating app fails at step 2 the next time
// it deploys. That is the right behavior and a bad experience on its own — an
// admin tightening a policy is entitled to know it will block four apps before
// they save, because finding out one deploy at a time is how a policy gets
// rolled back in anger.
//
// Side-effect free, and not only incidentally: this runs the same plan-time
// checks the deploy will run, against a policy that is not saved yet. Nothing
// here may write, and the checks it reuses are the pre-boundary ones for
// exactly that reason.
func (p *Planner) PreviewPolicy(ctx context.Context, candidate policy.Document) ([]PolicyViolation, error) {
	if p.inventory == nil {
		return nil, errs.New(errs.Internal, "Pando cannot list this installation's apps to check them.")
	}

	// Capabilities are read once per adapter, not once per app: this runs
	// while an admin waits on a form.
	caps := p.runtimeCapabilities(ctx)
	filter := p.previewFilter(ctx, candidate, caps)
	violations := make([]PolicyViolation, 0)
	if filter.Empty() {
		return violations, nil
	}

	apps, err := p.inventory.LiveApps(ctx, filter)
	if err != nil {
		return nil, err
	}

	// The candidate, evaluated live. Static rather than the stored loader
	// because the whole question is what a policy that is not saved yet would
	// do.
	under := &Planner{
		registry:    p.registry,
		policy:      policy.Static(candidate),
		allocations: p.allocations,
	}

	for _, app := range apps {
		if app.Spec == nil {
			continue
		}
		if v, ok := under.violation(ctx, app, caps); ok {
			violations = append(violations, v)
		}
	}
	return violations, nil
}

// violation runs the policy-derived checks against one app.
//
// Only the policy-derived ones. An app that would fail its next deploy because
// its builder was uninstalled is already broken and has nothing to do with the
// policy being saved — listing it here would tell an admin their new rule
// breaks an app it does not touch, and the next thing they do is not save the
// rule.
func (p *Planner) violation(ctx context.Context, app InventoryApp, caps map[string]api.RuntimeCapabilities) (PolicyViolation, bool) {
	s := app.Spec

	// The source allowlist (R-092), checked before anything would clone.
	if err := p.policy.AllowsSource(ctx, s.Source); err != nil {
		return asViolation(app, err), true
	}

	// Anyone on the internet, without signing in (R-076). Not a spec check —
	// the grant is the violation, and an app that is reachable anonymously
	// under a policy that forbids it is the case where "report now, block on
	// next deploy" is least comfortable and most worth stating plainly.
	if app.AnonymousGrant {
		if err := p.policy.AllowsAnonymousGrant(ctx, app.AnonymousPasscode); err != nil {
			return asViolation(app, err), true
		}
	}

	// Isolation floors (R-024, R-114). Needs the runtime's live capabilities,
	// so an app on a runtime that is gone or unhealthy is skipped rather than
	// reported: that is a broken app, not a policy consequence.
	runtimeCaps, ok := caps[s.Runtime.AdapterRef]
	if !ok {
		return PolicyViolation{}, false
	}

	// Egress (R-183, R-186): a loosening the candidate forbids, or rules it
	// restricts on a runtime that cannot enforce them. Both are refused at
	// the app's next deploy, with these words. That a policy would make a
	// deploy need approval (R-154) is not here: it blocks nothing, and this
	// list is what the next deploy would fail with.
	eff, err := p.Egress(ctx, s)
	if err == nil {
		if err := checkEgress(s, eff, runtimeCaps); err != nil {
			return asViolation(app, err), true
		}
	}

	// The runtime floor applies to every app, built or not: an image app on
	// a runtime below the floor is refused at its next deploy like any other.
	if err := p.checkRuntimeFloor(ctx, s, runtimeCaps); err != nil {
		if errs.CodeOf(err) == errs.PlanNoAdapterMeetsPolicy {
			return asViolation(app, err), true
		}
		return PolicyViolation{}, false
	}

	if err := p.checkIsolation(ctx, s, runtimeCaps); err != nil {
		// checkIsolation also reports a missing builder, which is not a policy
		// consequence — see the note above.
		if errs.CodeOf(err) == errs.PlanNoAdapterMeetsPolicy && s.Build.AdapterRef != "" {
			return asViolation(app, err), true
		}
		return PolicyViolation{}, false
	}

	return PolicyViolation{}, false
}

// runtimeCapabilities is every configured runtime that answers, by ref. One
// that is gone or does not answer is left out, and violation skips its apps.
func (p *Planner) runtimeCapabilities(ctx context.Context) map[string]api.RuntimeCapabilities {
	out := map[string]api.RuntimeCapabilities{}
	for _, ref := range p.registry.ByCategory(api.CategoryRuntime) {
		rt, ok := p.registry.Runtime(ref)
		if !ok {
			continue
		}
		if c, err := rt.Capabilities(ctx); err == nil {
			out[ref] = c
		}
	}
	return out
}

// previewFilter is which apps the candidate could block, check by check in
// violation's order. Each clause must name every app its check could fail:
// an app left out is an app never examined.
func (p *Planner) previewFilter(ctx context.Context, candidate policy.Document, caps map[string]api.RuntimeCapabilities) InventoryFilter {
	f := InventoryFilter{
		// The source allowlist (R-092) is matched against each source, so an
		// allowlist in force reads every app.
		All: len(candidate.SourceAllowlist) > 0,
	}

	// Public sharing (R-076): only an app shared with everyone can fail it.
	switch candidate.PublicSharingMode() {
	case policy.PublicSharingNone:
		f.Anonymous = true
	case policy.PublicSharingPasscodeOnly:
		f.Anonymous, f.AnonymousWithoutPasscode = true, true
	}

	// Egress (R-183, R-186). Only an app's own settings loosen, so loosening
	// forbidden names the apps that have some. A restriction fails only on a
	// runtime that cannot enforce it: there, an app with settings of its own
	// may restrict itself, and an app without them is restricted exactly when
	// the candidate restricts an app with no settings at all.
	f.OwnEgress = candidate.EgressLooseningRule() == policy.EgressLooseningForbidden
	for ref, c := range caps {
		if !c.SupportsEgressRestriction {
			f.EgressRuntimes = append(f.EgressRuntimes, ref)
		}
	}
	if len(f.EgressRuntimes) > 0 {
		f.EgressRestrictsAll = candidate.EgressFor(spec.Egress{}).Restricted
	}

	// Isolation floors (R-024, R-114), compared per app against the higher of
	// the candidate's floor and the app's own.
	f.MinRuntime, f.MinBuild = candidate.MinRuntimeIsolation, candidate.MinBuildIsolation
	for ref, c := range caps {
		if f.Runtimes == nil {
			f.Runtimes = map[string]spec.IsolationClass{}
		}
		f.Runtimes[ref] = c.IsolationClass
	}
	for _, ref := range p.registry.ByCategory(api.CategoryBuilder) {
		b, ok := p.registry.Builder(ref)
		if !ok {
			continue
		}
		// A builder that does not answer fails as unavailable, which
		// violation does not report.
		c, err := b.Capabilities(ctx)
		if err != nil {
			continue
		}
		if f.Builders == nil {
			f.Builders = map[string]spec.IsolationClass{}
		}
		f.Builders[ref] = c.IsolationClass
	}
	return f
}

func asViolation(app InventoryApp, err error) PolicyViolation {
	e := errs.As(err)
	return PolicyViolation{
		AppID: app.AppID, AppName: app.Name,
		Code: string(e.Code), Message: e.Message, Remedy: e.Remedy,
	}
}
