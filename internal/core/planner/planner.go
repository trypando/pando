// Package planner turns a spec plus policy plus adapter capabilities into a
// plan, or into a readable plan-time error.
//
// Everything here is side-effect-free. That boundary is what makes a plan-time
// failure meaningful rather than a label on a mid-deploy crash: steps 1-7 of the
// deployment pipeline create nothing, clone nothing, and start nothing, so the
// console can call Plan on every spec edit without consequence.
//
// If a check needs a side effect to run, it belongs after the boundary and it is
// not a plan-time check.
package planner

import (
	"context"
	"fmt"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Allocation is what other apps already hold, for the capacity check.
type Allocation struct {
	CPUMillis   int
	MemoryBytes int64
	DiskBytes   int64

	// LogBytes is the sum of every app's log cap on this runtime (R-224).
	//
	// Committed, not measured. O-16 resolved to bounding what an install
	// promises rather than watching what accumulates: if every app's logs are
	// capped and the caps sum under the budget, the total cannot exceed it.
	// Measuring would mean acting after the disk was already filling, and the
	// only remedy then is recreating containers — which the reconciler may not
	// do because an unrelated app turned chatty.
	LogBytes int64
}

// Allocations reports current commitments per runtime adapter.
type Allocations interface {
	// AllocatedOn returns what is already committed on a runtime adapter,
	// excluding the app being planned — replanning an app must not count its
	// own current allocation against itself.
	AllocatedOn(ctx context.Context, adapterRef, excludeAppID string) (Allocation, error)
}

// Planner produces bundle plans.
type Planner struct {
	registry    *api.Registry
	policy      *policy.Evaluator
	allocations Allocations

	// inventory is every live app, used only to preview a policy before it is
	// saved (design 05 §3). Optional: a planner without it plans exactly the
	// same and refuses to preview.
	inventory Inventory

	// images reads an image app's image for the platform check (image.go).
	// Optional: without it an image's platforms are left to the pull.
	images ImageReader

	// installRegistry is the install's image registry, for how a build
	// reaches a runtime that pulls (delivery.go). Optional: without it the
	// install has none.
	installRegistry InstallRegistries
}

// WithInventory enables policy preview.
func (p *Planner) WithInventory(inv Inventory) *Planner {
	p.inventory = inv
	return p
}

func New(registry *api.Registry, pol *policy.Evaluator, allocations Allocations) *Planner {
	return &Planner{registry: registry, policy: pol, allocations: allocations}
}

// Plan is the result of a successful plan.
type Plan struct {
	AppID  string            `json:"app_id"`
	Bundle api.BundlePlan    `json:"-"`
	Notes  []string          `json:"notes,omitempty"`
	Checks map[string]string `json:"checks"`

	// Egress is the rules the app would run with, merged, with where each
	// part came from and what every loosening needs (R-188).
	Egress policy.EffectiveEgress `json:"egress"`
}

// Check runs steps 1-7 and returns the plan, or the first blocking error.
//
// The order matches design 05 §3 exactly, and it matters: the source allowlist
// is checked before anything would clone, capabilities before capacity, and
// capacity last because it is the only check whose answer depends on other apps.
func (p *Planner) Check(ctx context.Context, s *spec.AppSpec) (*Plan, error) {
	plan := &Plan{AppID: s.AppID, Checks: map[string]string{}}

	// 1. Validate the spec.
	if err := spec.Validate(s); err != nil {
		return nil, err
	}
	plan.Checks["spec_valid"] = "ok"

	// 2. Host policy, including the source allowlist (R-092) — before clone.
	if p.policy != nil {
		if err := p.policy.AllowsSource(ctx, s.Source); err != nil {
			return nil, err
		}
	}
	plan.Checks["policy"] = "ok"

	// 3. Resolve adapters and health-check them.
	runtime, routing, err := p.resolveAdapters(ctx, s)
	if err != nil {
		return nil, err
	}
	plan.Checks["adapters"] = "ok"

	// 4. Capability checks (R-254).
	runtimeCaps, _, err := p.checkCapabilities(ctx, s, runtime, routing)
	if err != nil {
		return nil, err
	}
	plan.Checks["capabilities"] = "ok"

	// 4a. An image app's image runs on this runtime (issue #41) — a capability
	// question too, asked of the registry instead of the adapter.
	imageNotes, err := p.checkImage(ctx, s, runtimeCaps)
	if err != nil {
		return nil, err
	}
	plan.Notes = append(plan.Notes, imageNotes...)
	if s.Source.Type == spec.SourceImage && p.images != nil {
		plan.Checks["image"] = "ok"
	}

	// 4b. Egress (R-183, R-186). After capabilities because the refusal for a
	// runtime that cannot restrict egress is a capability refusal.
	eff, err := p.Egress(ctx, s)
	if err != nil {
		return nil, err
	}
	if err := checkEgress(s, eff, runtimeCaps); err != nil {
		return nil, err
	}
	plan.Egress = eff
	plan.Notes = append(plan.Notes, egressNotes(eff)...)
	plan.Checks["egress"] = "ok"

	// 5. Isolation floors (R-024, R-114).
	if err := p.checkIsolation(ctx, s, runtimeCaps); err != nil {
		return nil, err
	}
	plan.Checks["isolation"] = "ok"

	// 5a. How the build reaches the runtime (R-254, issue #72 PR 5): after
	// isolation, which names a builder that is not configured.
	delivery, reg, err := p.checkDelivery(ctx, s, runtimeCaps)
	if err != nil {
		return nil, err
	}
	if delivery != "" {
		plan.Checks["image_delivery"] = "ok"
		if note := deliveryNote(delivery, reg); note != "" {
			plan.Notes = append(plan.Notes, note)
		}
	}

	// 6. Every required slot resolved (R-132).
	if err := p.checkSlots(s); err != nil {
		return nil, err
	}
	plan.Checks["slots"] = "ok"

	// 7. Capacity (R-242).
	if err := p.checkCapacity(ctx, s, runtime); err != nil {
		return nil, err
	}
	plan.Checks["capacity"] = "ok"

	// 8. Log retention (R-222–R-224, O-16).
	if err := p.checkLogRetention(ctx, s, runtimeCaps); err != nil {
		return nil, err
	}
	plan.Checks["log_retention"] = "ok"

	plan.Bundle = p.bundlePlan(s, eff.Rules)
	return plan, nil
}

// Egress resolves an app's egress rules against host policy as it stands
// (R-182). The deploy runner asks this too, so what the plan showed is what
// the runtime is given.
func (p *Planner) Egress(ctx context.Context, s *spec.AppSpec) (policy.EffectiveEgress, error) {
	doc := policy.Default()
	if p.policy != nil {
		var err error
		if doc, err = p.policy.Document(ctx); err != nil {
			return policy.EffectiveEgress{}, err
		}
	}
	return doc.EgressFor(s.Egress), nil
}

// checkEgress refuses a plan whose egress cannot be honored: a loosening
// policy forbids (R-183), or restrictions the runtime cannot enforce (R-186).
func checkEgress(s *spec.AppSpec, eff policy.EffectiveEgress, caps api.RuntimeCapabilities) error {
	if eff.Forbidden() {
		entries := make([]string, 0, len(eff.Loosenings))
		for _, l := range eff.Loosenings {
			entries = append(entries, l.Message)
		}
		return errs.Newf(errs.PlanEgressLooseningForbidden,
			"This app loosens the installation's egress rules, and this installation does not let any app do that. %s",
			strings.Join(entries, " ")).
			WithRemedy("Remove the loosening from the app's egress settings, or ask an administrator to allow apps to loosen egress rules in host policy.").
			WithDetail("loosenings", eff.Loosenings)
	}
	if eff.Restricted && !caps.SupportsEgressRestriction {
		return errs.Newf(errs.PlanCapabilityUnsupported,
			"This app's egress rules limit where it may connect, and %q cannot enforce egress rules. Pando will not run the app with its rules ignored.",
			s.Runtime.AdapterRef).
			WithRemedy("Deploy to a runtime that can enforce egress rules, or remove the restrictions: the installation's egress mode and private-address block, and the app's own list.").
			WithDetail("adapter_ref", s.Runtime.AdapterRef).
			WithDetail("capability", "egress_restriction")
	}
	return nil
}

// egressNotes are what somebody reading the plan should know about its
// egress. Notes, never blockers.
func egressNotes(eff policy.EffectiveEgress) []string {
	var out []string
	if eff.Restricted {
		// R-187: said wherever a restriction is in effect.
		out = append(out, "This app's outbound connections are limited. Only HTTP and HTTPS through Pando's egress gateway leave it: its workloads are given HTTP_PROXY and HTTPS_PROXY, and anything that does not use them cannot connect out.")
	}
	if eff.NeedsApproval() {
		out = append(out, "This app loosens the installation's egress rules, so its deploy needs approval.")
	}
	out = append(out, eff.Unused...)
	return out
}

func (p *Planner) resolveAdapters(ctx context.Context, s *spec.AppSpec) (api.RuntimeAdapter, api.RoutingAdapter, error) {
	runtime, ok := p.registry.Runtime(s.Runtime.AdapterRef)
	if !ok {
		return nil, nil, errs.Newf(errs.PlanAdapterNotConfigured,
			"This app is set to run on %q, which is not configured on this installation.", s.Runtime.AdapterRef).
			WithDetail("adapter_ref", s.Runtime.AdapterRef).
			WithDetail("configured", p.registry.ByCategory(api.CategoryRuntime)).
			WithRemedy("Choose one of the configured runtimes, or ask an administrator to add this one.")
	}

	routing, ok := p.registry.Routing(s.Routing.AdapterRef)
	if !ok {
		return nil, nil, errs.Newf(errs.PlanAdapterNotConfigured,
			"This app is set to be reached through %q, which is not configured on this installation.", s.Routing.AdapterRef).
			WithDetail("adapter_ref", s.Routing.AdapterRef).
			WithDetail("configured", p.registry.ByCategory(api.CategoryRouting)).
			WithRemedy("Choose one of the configured routing options, or ask an administrator to add this one.")
	}

	// Refusing to plan against an unhealthy adapter is what keeps a broken
	// adapter from becoming a half-finished deploy (R-254).
	for ref, adapter := range map[string]api.Adapter{
		s.Runtime.AdapterRef: runtime,
		s.Routing.AdapterRef: routing,
	} {
		if err := adapter.HealthCheck(ctx); err != nil {
			return nil, nil, errs.Wrap(errs.AdapterUnavailable,
				fmt.Sprintf("Pando cannot reach %q right now, so it will not start a deploy it could not finish.", ref),
				err).
				WithDetail("adapter_ref", ref).
				WithRemedy("Check that the service is running, then try again.")
		}
	}
	return runtime, routing, nil
}

func (p *Planner) checkCapabilities(ctx context.Context, s *spec.AppSpec, runtime api.RuntimeAdapter, routing api.RoutingAdapter) (api.RuntimeCapabilities, api.RoutingCapabilities, error) {
	runtimeCaps, err := runtime.Capabilities(ctx)
	if err != nil {
		return api.RuntimeCapabilities{}, api.RoutingCapabilities{}, errs.Wrap(errs.AdapterUnavailable,
			"Pando could not read what the runtime supports.", err)
	}
	routingCaps, err := routing.Capabilities(ctx)
	if err != nil {
		return api.RuntimeCapabilities{}, api.RoutingCapabilities{}, errs.Wrap(errs.AdapterUnavailable,
			"Pando could not read what the routing option supports.", err)
	}

	unsupported := func(message, remedy string, details map[string]any) error {
		e := errs.New(errs.PlanCapabilityUnsupported, message).WithRemedy(remedy)
		for k, v := range details {
			e = e.WithDetail(k, v)
		}
		return e
	}

	if !routingCaps.Supports(s.Routing.Mode) {
		supported := make([]string, 0, len(routingCaps.Modes))
		for _, m := range routingCaps.Modes {
			supported = append(supported, string(m))
		}
		return runtimeCaps, routingCaps, unsupported(
			fmt.Sprintf("%q cannot serve this app the way it is set up.", s.Routing.AdapterRef),
			"Choose a different way for people to reach this app, or a different routing option.",
			map[string]any{
				"adapter_ref": s.Routing.AdapterRef,
				"requested":   string(s.Routing.Mode),
				"supported":   supported,
			})
	}

	// R-026: an adapter that cannot provide a private network would place this
	// app's workloads where other apps could reach them.
	if !runtimeCaps.SupportsPrivateNetwork {
		return runtimeCaps, routingCaps, unsupported(
			fmt.Sprintf("%q cannot keep this app's parts on a private network, which Pando requires.", s.Runtime.AdapterRef),
			"Use a different runtime.",
			map[string]any{"adapter_ref": s.Runtime.AdapterRef, "capability": "private_network"})
	}

	if len(s.Workloads) > 1 && !runtimeCaps.SupportsMultipleWorkloads {
		return runtimeCaps, routingCaps, unsupported(
			fmt.Sprintf("This app has %d parts, and %q can only run one.", len(s.Workloads), s.Runtime.AdapterRef),
			"Use a runtime that can run multiple workloads.",
			map[string]any{"adapter_ref": s.Runtime.AdapterRef, "workloads": len(s.Workloads)})
	}

	if runtimeCaps.MaxWorkloadsPerBundle > 0 && len(s.Workloads) > runtimeCaps.MaxWorkloadsPerBundle {
		return runtimeCaps, routingCaps, unsupported(
			fmt.Sprintf("This app has %d parts, and %q supports at most %d.",
				len(s.Workloads), s.Runtime.AdapterRef, runtimeCaps.MaxWorkloadsPerBundle),
			"Reduce the number of workloads, or use a different runtime.",
			map[string]any{"adapter_ref": s.Runtime.AdapterRef, "max": runtimeCaps.MaxWorkloadsPerBundle})
	}

	if len(s.Volumes) > 0 && !runtimeCaps.SupportsPersistentVolumes {
		return runtimeCaps, routingCaps, unsupported(
			fmt.Sprintf("This app keeps data in storage, and %q cannot provide any.", s.Runtime.AdapterRef),
			"Use a runtime that supports persistent storage, or remove the storage from this app.",
			map[string]any{"adapter_ref": s.Runtime.AdapterRef, "capability": "persistent_volumes"})
	}

	if carriesFiles(s) && !runtimeCaps.SupportsCarriedFiles {
		return runtimeCaps, routingCaps, unsupported(
			fmt.Sprintf("This app carries a configuration file into one of its parts, and %q cannot place one.", s.Runtime.AdapterRef),
			"Use a runtime that can, or build the file into the image and remove it from the app's configuration.",
			map[string]any{"adapter_ref": s.Runtime.AdapterRef, "capability": "carried_files"})
	}

	if s.Deploy.Strategy == spec.DeployStartThenSwap && !runtimeCaps.SupportsStartThenSwap {
		return runtimeCaps, routingCaps, unsupported(
			fmt.Sprintf("This app is set to start the new version before stopping the old one, and %q cannot do that.", s.Runtime.AdapterRef),
			"Switch this app to the standard deploy strategy, or use a different runtime.",
			map[string]any{"adapter_ref": s.Runtime.AdapterRef, "capability": "start_then_swap"})
	}

	if s.Resources.Overridden && !runtimeCaps.SupportsResourceLimits {
		return runtimeCaps, routingCaps, unsupported(
			fmt.Sprintf("This app sets its own resource limits, and %q cannot enforce them.", s.Runtime.AdapterRef),
			"Remove the resource limits, or use a runtime that can apply them.",
			map[string]any{"adapter_ref": s.Runtime.AdapterRef, "capability": "resource_limits"})
	}

	return runtimeCaps, routingCaps, nil
}

// checkIsolation enforces the policy floors (R-024, R-114).
//
// Build and runtime floors are checked separately and reported separately,
// because they are different requirements and an operator who set one and not
// the other deserves to be told which.
// carriesFiles reports whether any workload has configuration to place.
func carriesFiles(s *spec.AppSpec) bool {
	for _, w := range s.Workloads {
		if len(w.Files) > 0 {
			return true
		}
	}
	return false
}

// checkRuntimeFloor is the runtime half of checkIsolation (R-114): every app
// runs somewhere, so it applies to an image app as much as a built one.
func (p *Planner) checkRuntimeFloor(ctx context.Context, s *spec.AppSpec, runtimeCaps api.RuntimeCapabilities) error {
	if p.policy == nil {
		return nil
	}
	_, runtimeFloor, err := p.policy.IsolationFloors(ctx)
	if err != nil {
		return err
	}

	// The spec's own floor is a request; policy's is a requirement. The
	// effective floor is the higher of the two — policy can only tighten.
	if s.Runtime.IsolationFloor > runtimeFloor {
		runtimeFloor = s.Runtime.IsolationFloor
	}

	if runtimeCaps.IsolationClass < runtimeFloor {
		return errs.Newf(errs.PlanNoAdapterMeetsPolicy,
			"This installation requires apps to run with stronger separation than %q provides.", s.Runtime.AdapterRef).
			WithDetail("adapter_ref", s.Runtime.AdapterRef).
			WithDetail("required_floor", int(runtimeFloor)).
			WithDetail("adapter_class", int(runtimeCaps.IsolationClass)).
			WithDetail("configured_runtimes", p.adapterClasses(ctx)).
			WithRemedy("Use a runtime that provides stronger isolation, or ask an administrator about the installation's requirements.")
	}
	return nil
}

func (p *Planner) checkIsolation(ctx context.Context, s *spec.AppSpec, runtimeCaps api.RuntimeCapabilities) error {
	if p.policy == nil {
		return nil
	}
	buildFloor, _, err := p.policy.IsolationFloors(ctx)
	if err != nil {
		return err
	}
	if err := p.checkRuntimeFloor(ctx, s, runtimeCaps); err != nil {
		return err
	}

	// R-024: builds never execute on the host, and there is no "just build it
	// here" fallback. An app whose source must be built therefore needs a
	// configured builder, and the absence of one is a plan-time failure rather
	// than something discovered when the deploy reaches step 9.
	//
	// A prebuilt image needs no builder, which is why this is keyed on the
	// source rather than on the field being set.
	if s.Build.AdapterRef == "" && needsBuild(s) {
		return errs.New(errs.PlanNoAdapterMeetsPolicy,
			"This app has to be built from source, and this installation has nothing configured to build it.").
			WithDetail("source_type", string(s.Source.Type)).
			WithDetail("configured_builders", p.registry.ByCategory(api.CategoryBuilder)).
			WithRemedy("Ask an administrator to configure a builder, or point this app at a prebuilt image instead.")
	}

	if s.Build.AdapterRef != "" {
		builder, ok := p.registry.Builder(s.Build.AdapterRef)
		if !ok {
			return errs.Newf(errs.PlanAdapterNotConfigured,
				"This app is set to be built by %q, which is not configured on this installation.", s.Build.AdapterRef).
				WithDetail("adapter_ref", s.Build.AdapterRef).
				WithRemedy("Choose a configured builder, or ask an administrator to add this one.")
		}
		caps, err := builder.Capabilities(ctx)
		if err != nil {
			return errs.Wrap(errs.AdapterUnavailable, "Pando could not read what the builder supports.", err)
		}

		effective := buildFloor
		if s.Build.IsolationFloor > effective {
			effective = s.Build.IsolationFloor
		}
		if caps.IsolationClass < effective {
			return errs.Newf(errs.PlanNoAdapterMeetsPolicy,
				"This installation requires builds to run with stronger separation than %q provides.", s.Build.AdapterRef).
				WithDetail("adapter_ref", s.Build.AdapterRef).
				WithDetail("required_floor", int(effective)).
				WithDetail("adapter_class", int(caps.IsolationClass)).
				WithRemedy("Use a builder that provides stronger isolation, or ask an administrator about the installation's requirements.")
		}
		if !caps.Supports(s.Build.Strategy) {
			// Names the method and what this installation can actually do.
			//
			// It used to say only that the builder "cannot build this app the
			// way it is set up", which points at the builder when the problem
			// is the spec's build method, and offers "choose a different build
			// method" without saying which ones exist. Somebody reading it has
			// no way to act on it — the R-105 standard is a message that can be
			// acted on, or pasted into an assistant, without further context.
			supported := make([]string, 0, len(caps.Strategies))
			for _, st := range caps.Strategies {
				supported = append(supported, string(st))
			}
			return errs.Newf(errs.PlanCapabilityUnsupported,
				"This app is set to build with %q, and %q cannot do that. It builds: %s.",
				s.Build.Strategy, s.Build.AdapterRef, strings.Join(supported, ", ")).
				WithDetail("adapter_ref", s.Build.AdapterRef).
				WithDetail("requested", string(s.Build.Strategy)).
				WithDetail("supported", supported).
				WithRemedy(fmt.Sprintf(
					"Change this app's build method to one of: %s. Detection will work it out again if you re-run it from the app's configuration.",
					strings.Join(supported, ", ")))
		}
	}
	return nil
}

// checkSlots implements R-132.
//
// Details name every unfilled slot, not just the first: someone filling slots
// one deploy attempt at a time is the experience this requirement exists to
// prevent.
func (p *Planner) checkSlots(s *spec.AppSpec) error {
	var unfilled []map[string]string
	var firstDisplay string
	for _, slot := range s.Slots {
		if slot.Required && slot.Resolution == nil {
			if firstDisplay == "" {
				firstDisplay = slot.Type.DisplayName()
			}
			unfilled = append(unfilled, map[string]string{
				"key":  slot.Key,
				"type": string(slot.Type),
			})
		}
	}
	if len(unfilled) == 0 {
		// A slot chosen as "provision one" with nothing on this install that
		// can provision it is refused here, not at deploy.
		//
		// This is the plan boundary doing its job: the alternative is a deploy
		// that builds an image, stops the running app and then discovers there
		// is no Postgres provisioner — a plan-time answer costs nothing and a
		// deploy-time one costs the app's uptime (R-132).
		return p.checkProvisionable(s)
	}

	message := fmt.Sprintf("This app needs a %s, and one hasn't been chosen yet.", firstDisplay)
	remedy := fmt.Sprintf(
		"Choose how to fill the %s slot: provision one inside this app, connect to an existing one, or paste a connection string.",
		unfilled[0]["key"])
	if len(unfilled) > 1 {
		message = fmt.Sprintf("This app needs %d things that haven't been chosen yet.", len(unfilled))
		remedy = "Fill each slot: provision one inside this app, connect to an existing one, or paste a connection string."
	}

	return errs.New(errs.PlanSlotUnfilled, message).
		WithRemedy(remedy).
		WithDetail("slots", unfilled)
}

// checkProvisionable refuses a provisioned slot no configured adapter can fill.
func (p *Planner) checkProvisionable(s *spec.AppSpec) error {
	var unsupported []map[string]string
	for _, slot := range s.Slots {
		if slot.Resolution == nil || slot.Resolution.Mode != spec.ResolutionProvisioned {
			continue
		}
		if _, _, ok := p.registry.ServicesFor(slot.Type); !ok {
			unsupported = append(unsupported, map[string]string{
				"key": slot.Key, "type": string(slot.Type),
			})
		}
	}
	if len(unsupported) == 0 {
		return nil
	}

	first := spec.SlotType(unsupported[0]["type"])
	return errs.Newf(errs.PlanAdapterNotConfigured,
		"Nothing on this installation can provision a %s.", first.DisplayName()).
		WithRemedy(fmt.Sprintf(
			"Connect %s to an instance you already run, or paste a connection string.",
			unsupported[0]["key"])).
		WithDetail("slots", unsupported)
}

// checkCapacity implements R-242.
//
// Capacity is adapter-reported (R-243): core does not read /proc and has no
// concept of a host. Details carry requested, allocated, and the adapter's own
// total, because a capacity refusal that does not show its arithmetic is not
// actionable.
func (p *Planner) checkCapacity(ctx context.Context, s *spec.AppSpec, runtime api.RuntimeAdapter) error {
	if p.allocations == nil {
		return nil
	}

	capacity, err := runtime.Capacity(ctx)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Pando could not read how much room is left.", err)
	}
	allocated, err := p.allocations.AllocatedOn(ctx, s.Runtime.AdapterRef, s.AppID)
	if err != nil {
		return err
	}

	requested := Allocation{
		CPUMillis:   s.Resources.CPUMillis,
		MemoryBytes: s.Resources.MemoryBytes,
		DiskBytes:   s.Resources.DiskBytes,
	}

	over := func(kind string, req, alloc, total int64, format func(int64) string) error {
		if total <= 0 || req+alloc <= total {
			return nil
		}
		return errs.Newf(errs.CapacityWouldOversubscribe,
			"There is not enough %s left on this installation to start this app.", kind).
			WithDetail("resource", kind).
			WithDetail("requested", format(req)).
			WithDetail("already_allocated", format(alloc)).
			WithDetail("total", format(total)).
			WithDetail("available", format(total-alloc)).
			WithRemedy(fmt.Sprintf(
				"This app asks for %s and only %s is free. Lower what it asks for, or stop another app.",
				format(req), format(max64(total-alloc, 0))))
	}

	// Host policy may allow CPU and memory to be oversubscribed, each on its
	// own (R-242 as amended). Read only here: the default refuses, and an
	// install that cannot read its policy refuses too rather than guessing.
	allowCPU, allowMemory := false, false
	if p.policy != nil {
		doc, err := p.policy.Document(ctx)
		if err != nil {
			return err
		}
		allowCPU, allowMemory = doc.AllowCPUOversubscription, doc.AllowMemoryOversubscription
	}

	if !allowCPU {
		if err := over("CPU", int64(requested.CPUMillis), int64(allocated.CPUMillis), int64(capacity.TotalCPUMillis), formatMillis); err != nil {
			return err
		}
	}
	if !allowMemory {
		if err := over("memory", requested.MemoryBytes, allocated.MemoryBytes, capacity.TotalMemoryBytes, formatBytes); err != nil {
			return err
		}
	}
	// Disk is never oversubscribed, whatever policy says: it is not a share
	// the kernel hands out, and a full disk stops every app and Pando too.
	if err := over("disk space", requested.DiskBytes, allocated.DiskBytes, capacity.TotalDiskBytes, formatBytes); err != nil {
		return err
	}
	return p.checkLargestFit(ctx, s, runtime, allowCPU, allowMemory)
}

// checkLargestFit refuses an app no single place on the runtime has room
// for, though the totals do: on several machines, room spread over them
// cannot be combined, because an app's workloads all run in one place.
//
// The runtime answers for this app (LargestFitFor), counting what the app
// already holds as free where it runs, so a redeploy that fits in place is
// never refused. Core learns a number, not where it is (R-251). What is
// compared is what the workloads are limited to, summed, which is what the
// runtime reserves.
func (p *Planner) checkLargestFit(ctx context.Context, s *spec.AppSpec, runtime api.RuntimeAdapter, allowCPU, allowMemory bool) error {
	fit, err := runtime.LargestFitFor(ctx, s.AppID)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Pando could not read how much room is left.", err)
	}
	if fit == nil {
		return nil
	}
	var need api.Fit
	for _, w := range p.bundlePlan(s, api.EgressRules{}).Workloads {
		need.CPUMillis += w.Resources.CPUMillis
		need.MemoryBytes += w.Resources.MemoryBytes
	}
	refuse := func(kind, asked, free string) error {
		return errs.Newf(errs.CapacityWouldOversubscribe,
			"No single place this app's runtime can start it has enough %s free. The app's workloads ask for %s in all, and they all run in one place, where the most free is %s.",
			kind, asked, free).
			WithDetail("resource", kind).
			WithDetail("requested", asked).
			WithDetail("largest_free", free).
			WithRemedy("Lower what the app's workloads ask for, or stop or delete another app on this runtime to free room.")
	}
	if !allowMemory && need.MemoryBytes > fit.MemoryBytes {
		return refuse("memory", formatBytes(need.MemoryBytes), formatBytes(fit.MemoryBytes))
	}
	if !allowCPU && need.CPUMillis > fit.CPUMillis {
		return refuse("CPU", formatMillis(int64(need.CPUMillis)), formatMillis(int64(fit.CPUMillis)))
	}
	return nil
}

// bundlePlan builds the shape of what would be applied.
//
// Env is deliberately left empty here: resolving it means reading secrets, which
// is a side effect and belongs after the plan boundary (design 05 §3, step 11).
// The planner proves a deploy *could* work; it does not assemble the values.
func (p *Planner) bundlePlan(s *spec.AppSpec, rules api.EgressRules) api.BundlePlan {
	plan := api.BundlePlan{
		BundleID: s.AppID,
		Network: api.NetworkPlan{
			Private: true, // R-026, always.
			Egress:  rules,
		},
		Labels: map[string]string{"pando.app": s.AppID},
	}

	for _, v := range s.Volumes {
		plan.Volumes = append(plan.Volumes, api.VolumePlan{VolumeID: v.ID, Name: v.Name})
	}

	for _, w := range s.Workloads {
		wp := api.WorkloadPlan{
			Name: w.Name,
			// R-222: every workload is capped, so a chatty app cannot fill a
			// disk shared with twenty others.
			LogBytes:   s.Retention.LogBytes,
			Image:      w.Image,
			Command:    w.Command,
			Entrypoint: w.Entrypoint,
			WorkingDir: w.WorkingDir,
			DependsOn:  w.DependsOn,
			Exposed:    w.Exposed,
			Resources: api.ResourcePlan{
				CPUMillis:   s.Resources.CPUMillis,
				MemoryBytes: s.Resources.MemoryBytes,
			},
		}
		if w.Resources != nil {
			wp.Resources = api.ResourcePlan{CPUMillis: w.Resources.CPUMillis, MemoryBytes: w.Resources.MemoryBytes}
		}
		for _, m := range w.Mounts {
			wp.Mounts = append(wp.Mounts, api.MountPlan{VolumeID: m.VolumeID, Path: m.Path, ReadOnly: m.ReadOnly})
		}
		for _, port := range w.Ports {
			wp.Ports = append(wp.Ports, api.PortPlan{Number: port.Number, Protocol: port.Protocol})
		}
		plan.Workloads = append(plan.Workloads, wp)
	}
	return plan
}

func (p *Planner) adapterClasses(ctx context.Context) []map[string]any {
	var out []map[string]any
	for _, ref := range p.registry.ByCategory(api.CategoryRuntime) {
		rt, ok := p.registry.Runtime(ref)
		if !ok {
			continue
		}
		caps, err := rt.Capabilities(ctx)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{"adapter_ref": ref, "isolation_class": int(caps.IsolationClass)})
	}
	return out
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func formatMillis(m int64) string {
	if m%1000 == 0 {
		return fmt.Sprintf("%d CPU", m/1000)
	}
	return fmt.Sprintf("%.1f CPU", float64(m)/1000)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// needsBuild reports whether this app's source has to be turned into an image.
//
// A prebuilt image is run as it is; anything else is built, and R-024 says that
// build has to happen inside a builder adapter.
func needsBuild(s *spec.AppSpec) bool {
	if s.Build.Strategy == spec.BuildPrebuilt {
		return false
	}
	return s.Source.Type != spec.SourceImage
}

// checkLogRetention enforces R-222's per-app cap and R-224's aggregate.
//
// Both at plan time, which is O-16's resolution. The per-app half is a
// capability question: a runtime that cannot cap logs says so, and an install
// that has set an aggregate budget cannot honor it on such a runtime — so the
// deploy is refused with the reason rather than accepted and quietly unbounded.
//
// The aggregate half is a sum of commitments. It is scoped to the runtime
// adapter because that is where the logs physically are: a clustered runtime's
// logs are not on this host, and counting them against this host's disk would
// refuse deploys to protect a disk they do not touch.
func (p *Planner) checkLogRetention(ctx context.Context, s *spec.AppSpec, caps api.RuntimeCapabilities) error {
	if p.policy == nil {
		return nil
	}
	doc, err := p.policy.Document(ctx)
	if err != nil {
		return err
	}
	if doc.MaxLogDiskBytes <= 0 {
		return nil // No aggregate budget set, so nothing to enforce against.
	}

	if !caps.LogRetention.SupportsSizeCap {
		return errs.Newf(errs.PlanCapabilityUnsupported,
			"This installation limits how much disk app logs may use, and %q cannot limit an app's logs.",
			s.Runtime.AdapterRef).
			WithRemedy("Deploy to a runtime that can cap logs, or remove the log disk limit from the installation's policy.")
	}

	want := s.Retention.LogBytes
	if want <= 0 {
		return errs.New(errs.PlanCapabilityUnsupported,
			"This installation limits how much disk app logs may use, so every app needs a log limit of its own.").
			WithRemedy("Set retention.log_bytes in the app's configuration.")
	}
	if min := caps.LogRetention.MinBytes; min > 0 && want < min {
		return errs.Newf(errs.ValidInvalid,
			"This app asks for a %s log limit, and %q cannot go below %s.",
			humanBytes(want), s.Runtime.AdapterRef, humanBytes(min)).
			WithRemedy("Raise retention.log_bytes for this app.")
	}

	if p.allocations == nil {
		return nil
	}
	committed, err := p.allocations.AllocatedOn(ctx, s.Runtime.AdapterRef, s.AppID)
	if err != nil {
		return err
	}

	if total := committed.LogBytes + want; total > doc.MaxLogDiskBytes {
		return errs.Newf(errs.CapacityWouldOversubscribe,
			"Deploying this app would commit %s to app logs, and this installation allows %s.",
			humanBytes(total), humanBytes(doc.MaxLogDiskBytes)).
			WithRemedy("Lower this app's log limit, lower another app's, or raise the installation's limit.").
			WithDetail("committed_bytes", committed.LogBytes).
			WithDetail("requested_bytes", want).
			WithDetail("limit_bytes", doc.MaxLogDiskBytes)
	}
	return nil
}

// humanBytes renders a size in the shortest honest unit, for a message someone
// has to act on.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
