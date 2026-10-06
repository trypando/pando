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
	LiveApps(ctx context.Context) ([]InventoryApp, error)
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

	apps, err := p.inventory.LiveApps(ctx)
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

	// Capabilities are cached across apps. Twenty apps on one runtime is one
	// Capabilities call, not twenty: this runs while an admin waits on a form.
	caps := map[string]api.RuntimeCapabilities{}

	violations := make([]PolicyViolation, 0)
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
		rt, found := p.registry.Runtime(s.Runtime.AdapterRef)
		if !found {
			return PolicyViolation{}, false
		}
		c, err := rt.Capabilities(ctx)
		if err != nil {
			return PolicyViolation{}, false
		}
		caps[s.Runtime.AdapterRef] = c
		runtimeCaps = c
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

func asViolation(app InventoryApp, err error) PolicyViolation {
	e := errs.As(err)
	return PolicyViolation{
		AppID: app.AppID, AppName: app.Name,
		Code: string(e.Code), Message: e.Message, Remedy: e.Remedy,
	}
}
