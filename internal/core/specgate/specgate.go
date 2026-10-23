// Package specgate decides whether somebody may write a spec revision, for the
// parts of a spec that app.spec.edit alone does not cover: an app's egress
// rules (R-182 – R-184), its resource limits (R-241) and automatic deploys on
// an app whose deploys need approval (R-158).
//
// One function, called by every path that writes a revision from content the
// caller supplied, so that no surface — console, CLI, MCP — can save a spec
// another surface would refuse (R-261). Paths that copy the pinned spec and
// change something else (routing, a slot, a volume, a pinned commit) carry its
// egress, limits and auto-deploy settings unchanged and need nothing from here.
package specgate

import (
	"context"
	"fmt"
	"strings"

	"github.com/trypando/pando/internal/core/approval"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Authorizer is the two questions the gate asks. *authz.Authorizer answers
// both: Allows without auditing, CheckControl auditing a denial (design 06 §6).
type Authorizer interface {
	Allows(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) (bool, error)
	CheckControl(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) error
}

// Previewer answers CheckControl's question without auditing a denial.
// *authz.Authorizer is one, through PreviewControl.
type Previewer interface {
	Allows(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) (bool, error)
	PreviewControl(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) error
}

// Quiet is an Authorizer for a dry run: every question is answered as it
// would be for a real save, and no refusal is written to the audit log,
// because nothing was attempted.
func Quiet(az Previewer) Authorizer { return quiet{az} }

type quiet struct{ az Previewer }

func (q quiet) Allows(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) (bool, error) {
	return q.az.Allows(ctx, p, appID, verb)
}

func (q quiet) CheckControl(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) error {
	return q.az.PreviewControl(ctx, p, appID, verb)
}

// Change is one revision somebody wants to write.
type Change struct {
	AppID string

	// Policy is host policy as it stands now. The gate judges a change by the
	// rules in force when it is saved; the planner judges the deploy again
	// under the rules in force then (R-183).
	Policy policy.Document

	// Pinned is the app's pinned spec, nil for an app that has none yet. A
	// change is measured from it.
	Pinned *spec.AppSpec

	// Next is the revision being written.
	Next *spec.AppSpec

	// Latest is the app's newest revision, or nil. Limits it already sets are
	// not a change somebody makes by keeping them: they were allowed when it
	// was written (R-241).
	Latest *spec.AppSpec

	// HostResources is the installation's default limits (R-240). An app
	// running on exactly these overrides nothing, so the first revision,
	// written with the defaults, asks nothing, and nor does a spec that leaves
	// its limits out after the host's defaults have changed.
	HostResources spec.Resources
}

// Result is what the gate found about a change it allowed.
type Result struct {
	// EgressChanged is whether the change moves the app's egress at all.
	EgressChanged bool `json:"egress_changed"`

	// Loosenings is what the change loosens that the pinned spec did not
	// (policy.NewLoosenings).
	Loosenings []policy.Loosening `json:"loosenings,omitempty"`

	// NeedsApproval is whether those loosenings make the app's next deploy
	// need approval (R-154): policy gates loosening by approval.
	NeedsApproval bool `json:"needs_approval"`
}

// Check refuses a change the principal may not make, and otherwise says what
// it found.
//
//   - Any change to egress needs app.egress.tighten or app.egress.loosen
//     (R-184). Holding app.egress.loosen covers every egress change, so it is
//     asked first and without auditing a denial: somebody who holds loosen and
//     not tighten is not denied anything.
//   - A loosening the pinned spec did not already carry is judged by policy
//     (R-183): forbidden refuses it, verb needs app.egress.loosen, approval
//     lets it through for the deploy to be approved.
//   - Any change to the app's resource limits, or to a workload's own, needs
//     app.resources.override (R-241, R-241a).
//   - Automatic deploys are refused on an app whose deploys need approval
//     whatever they change (R-158).
func Check(ctx context.Context, az Authorizer, p authz.Principal, c Change) (Result, error) {
	var out Result
	if c.Next == nil {
		return out, nil
	}

	var prev spec.Egress
	if c.Pinned != nil {
		prev = c.Pinned.Egress
	}
	out.EgressChanged = !prev.Same(c.Next.Egress)

	if out.EgressChanged {
		loosen, err := az.Allows(ctx, p, c.AppID, authz.AppEgressLoosen)
		if err != nil {
			return out, err
		}

		// R-184: tightening is the lesser verb. CheckControl audits the
		// denial, which is the point of asking it rather than Allows.
		if !loosen {
			if err := az.CheckControl(ctx, p, c.AppID, authz.AppEgressTighten); err != nil {
				return out, withVerbMessage(err,
					"Changing this app's egress rules needs permission to change them. It requires app.egress.tighten on this app, or app.egress.loosen.")
			}
		}

		// Measured under today's policy on both sides: what is new is what the
		// pinned spec does not already loosen. A loosening already there was
		// allowed when it was written, and keeping it is not asking again.
		var before policy.EffectiveEgress
		if c.Pinned != nil {
			before = c.Policy.EgressFor(prev)
		}
		out.Loosenings = policy.NewLoosenings(before, c.Policy.EgressFor(c.Next.Egress))

		if len(out.Loosenings) > 0 {
			switch c.Policy.EgressLooseningRule() {
			case policy.EgressLooseningForbidden:
				return out, forbidden(out.Loosenings)
			case policy.EgressLooseningApproval:
				// Anybody who may change the app's egress may propose a
				// loosening; the deploy that carries it waits for approval.
				out.NeedsApproval = true
			default:
				if !loosen {
					if err := az.CheckControl(ctx, p, c.AppID, authz.AppEgressLoosen); err != nil {
						return out, withVerbMessage(err, fmt.Sprintf(
							"This change loosens the installation's egress rules for this app, which requires app.egress.loosen on this app. %s",
							messages(out.Loosenings)))
					}
				}
			}
		}
	}

	if err := checkResources(ctx, az, p, c); err != nil {
		return out, err
	}
	if err := checkAutoDeploy(c); err != nil {
		return out, err
	}
	return out, nil
}

// checkResources is R-241: changing how much of the host the app, or one of
// its workloads, may take overrides the defaults the host operator chose,
// which app.spec.edit alone does not cover. Limits the pinned spec or the
// newest revision already sets are not a change, and nor are the host's own
// defaults with no workload limited on its own: going back to those
// overrides nothing.
func checkResources(ctx context.Context, az Authorizer, p authz.Principal, c Change) error {
	for _, base := range []*spec.AppSpec{c.Pinned, c.Latest, {Resources: c.HostResources}} {
		if base != nil && !spec.LimitsChanged(base, c.Next) {
			return nil
		}
	}
	if err := az.CheckControl(ctx, p, c.AppID, authz.AppResourceOverride); err != nil {
		return withVerbMessage(err,
			"Changing this app's resource limits needs permission to override the host's defaults (app.resources.override). This change sets the CPU, memory or disk limit of the app or of one of its workloads.")
	}
	return nil
}

// forbidden is the refusal for a loosening policy does not permit, in the
// same words the planner uses for a deploy that carries one (R-183).
func forbidden(ls []policy.Loosening) error {
	return errs.Newf(errs.PlanEgressLooseningForbidden,
		"This change loosens the installation's egress rules for this app, and this installation does not let any app do that. %s",
		messages(ls)).
		WithRemedy("Remove the loosening from the app's egress settings, or ask an administrator to allow apps to loosen egress rules in host policy.").
		WithDetail("loosenings", ls)
}

// checkAutoDeploy is R-158: automatic deploys and approval do not combine.
//
// Asked of the spec being written, whatever the pinned one said: an app that
// already deploys automatically when policy starts requiring approval keeps
// its pinned spec (the auto-deploy job skips it), and the next spec written
// for it says which it wants.
func checkAutoDeploy(c Change) error {
	if !c.Next.Deploy.AutoDeploy.Enabled || !approval.BlocksAutoDeploy(c.Policy, c.AppID, c.Next) {
		return nil
	}
	var why []string
	var admin, own bool
	var reasons []approval.Reason
	for _, r := range approval.Reasons(c.Policy, c.AppID, c.Next, c.Next) {
		if r == approval.ReasonEgressLoosening {
			continue
		}
		reasons = append(reasons, r)
		why = append(why, r.Message())
		switch r {
		case approval.ReasonInstall, approval.ReasonAppPolicy:
			admin = true
		case approval.ReasonAppSpec:
			own = true
		}
	}

	remedy := "Turn off automatic deploys (deploy.auto_deploy.enabled) and deploy by hand; each deploy then waits for approval."
	switch {
	case admin && own:
		remedy += " To deploy automatically instead, stop requiring approval in the app's settings (deploy.require_approval) and ask an administrator to stop requiring it in host policy."
	case admin:
		remedy += " To deploy automatically instead, ask an administrator to stop requiring approval for this app in host policy."
	case own:
		remedy += " To deploy automatically instead, stop requiring approval in the app's settings (deploy.require_approval)."
	}

	return errs.Newf(errs.ValidInvalid,
		"Automatic deploys cannot be turned on for this app, because its deploys need approval. %s Every push would wait for somebody to approve it, so Pando does not combine the two.",
		strings.Join(why, " ")).
		WithRemedy(remedy).
		WithDetail("reasons", reasons)
}

// withVerbMessage replaces the authorizer's general wording on a missing
// verb with one that says which change needed it (R-105). Any other
// refusal — a suspended account, a policy that disables the verb — keeps its
// own words, because they are the true reason.
func withVerbMessage(err error, message string) error {
	if errs.CodeOf(err) != errs.PermVerbRequired {
		return err
	}
	e := errs.As(err)
	out := errs.New(errs.PermVerbRequired, message).
		WithRemedy("Ask the app's owner or an administrator to make this change, or to grant you a role that holds the permission.")
	for k, v := range e.Details {
		out = out.WithDetail(k, v)
	}
	return out
}

func messages(ls []policy.Loosening) string {
	parts := make([]string, 0, len(ls))
	for _, l := range ls {
		parts = append(parts, l.Message)
	}
	return strings.Join(parts, " ")
}
