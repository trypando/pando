package planner_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	servicesdocker "github.com/trypando/pando/internal/adapter/services/docker"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// --- adapter doubles -------------------------------------------------------

type fakeRuntime struct {
	caps     api.RuntimeCapabilities
	capacity api.Capacity
	// largestFit is what LargestFitFor answers for every bundle.
	largestFit *api.Fit
	unhealthy  error
}

func (f *fakeRuntime) Kind() string                                     { return "fake" }
func (f *fakeRuntime) Category() api.Category                           { return api.CategoryRuntime }
func (f *fakeRuntime) Configure(context.Context, json.RawMessage) error { return nil }
func (f *fakeRuntime) HealthCheck(context.Context) error                { return f.unhealthy }

func (f *fakeRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return f.caps, nil
}
func (f *fakeRuntime) Capacity(context.Context) (api.Capacity, error) { return f.capacity, nil }
func (f *fakeRuntime) LargestFitFor(context.Context, string) (*api.Fit, error) {
	return f.largestFit, nil
}
func (f *fakeRuntime) InUse(context.Context) (api.InUse, error) { return api.InUse{}, nil }

func (f *fakeRuntime) Apply(context.Context, api.BundlePlan) (api.BundleHandle, error) {
	return api.BundleHandle{}, nil
}
func (f *fakeRuntime) Usage(context.Context, api.BundleRef) (api.BundleUsage, error) {
	return api.BundleUsage{}, nil
}
func (f *fakeRuntime) Observe(context.Context, api.BundleRef) (api.ObservedBundle, error) {
	return api.ObservedBundle{}, nil
}
func (f *fakeRuntime) Stop(context.Context, api.BundleRef) error                        { return nil }
func (f *fakeRuntime) Destroy(context.Context, api.BundleRef, api.DestroyOptions) error { return nil }
func (f *fakeRuntime) CreateVolume(context.Context, api.VolumeRequest) (api.VolumeHandle, error) {
	return api.VolumeHandle{}, nil
}
func (f *fakeRuntime) DestroyVolume(context.Context, api.VolumeHandle) error { return nil }
func (f *fakeRuntime) SnapshotVolume(context.Context, api.VolumeHandle, io.Writer) error {
	return nil
}
func (f *fakeRuntime) RestoreVolume(context.Context, api.VolumeHandle, io.Reader) error { return nil }
func (f *fakeRuntime) ImportImage(context.Context, io.Reader) (string, error) {
	return "imported:latest", nil
}
func (f *fakeRuntime) Logs(context.Context, api.WorkloadRef, api.LogOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeRuntime) Exec(context.Context, api.WorkloadRef, api.ExecRequest) (api.ExecSession, error) {
	return nil, nil
}
func (f *fakeRuntime) Upstream(context.Context, api.WorkloadRef, int) (api.Upstream, error) {
	return api.Upstream{}, nil
}
func (f *fakeRuntime) Trial(context.Context, api.TrialRequest) (api.TrialResult, error) {
	return api.TrialResult{}, nil
}

type fakeRouting struct {
	caps      api.RoutingCapabilities
	unhealthy error
}

func (f *fakeRouting) Kind() string                                     { return "fake" }
func (f *fakeRouting) Category() api.Category                           { return api.CategoryRouting }
func (f *fakeRouting) Configure(context.Context, json.RawMessage) error { return nil }
func (f *fakeRouting) HealthCheck(context.Context) error                { return f.unhealthy }
func (f *fakeRouting) Capabilities(context.Context) (api.RoutingCapabilities, error) {
	return f.caps, nil
}
func (f *fakeRouting) Ensure(context.Context, api.RouteRequest) (api.RouteHandle, error) {
	return api.RouteHandle{}, nil
}
func (f *fakeRouting) Remove(context.Context, api.RouteHandle) error { return nil }
func (f *fakeRouting) Observe(context.Context, api.RouteHandle) (api.RouteState, error) {
	return api.RouteState{}, nil
}

type fakeBuilder struct{ caps api.BuilderCapabilities }

func (f *fakeBuilder) Kind() string                                     { return "fake" }
func (f *fakeBuilder) Category() api.Category                           { return api.CategoryBuilder }
func (f *fakeBuilder) Configure(context.Context, json.RawMessage) error { return nil }
func (f *fakeBuilder) HealthCheck(context.Context) error                { return nil }
func (f *fakeBuilder) Capabilities(context.Context) (api.BuilderCapabilities, error) {
	return f.caps, nil
}
func (f *fakeBuilder) Bid(context.Context, api.SourceView) (api.Bid, error) {
	return api.Bid{}, nil
}
func (f *fakeBuilder) Build(context.Context, api.BuildRequest) (api.BuildResult, error) {
	return api.BuildResult{}, nil
}
func (f *fakeBuilder) Forget(context.Context, string) error { return nil }

type fixedAllocations struct{ alloc planner.Allocation }

func (f fixedAllocations) AllocatedOn(context.Context, string, string) (planner.Allocation, error) {
	return f.alloc, nil
}

// --- fixtures --------------------------------------------------------------

func capableRuntime() *fakeRuntime {
	return &fakeRuntime{
		caps: api.RuntimeCapabilities{
			IsolationClass:            spec.IsolationContainer,
			SupportsPersistentVolumes: true,
			SupportsExec:              true,
			SupportsMultipleWorkloads: true,
			SupportsPrivateNetwork:    true,
			SupportsResourceLimits:    true,

			// Takes a built image directly, as single-host Docker does.
			ImageDelivery: []api.ImageDelivery{api.ImageDeliveryImport},

			// R-222: a runtime that can bound a workload's logs, which is what
			// lets an install enforce an aggregate budget (O-16).
			LogRetention: api.LogRetentionCapability{
				SupportsSizeCap: true,
				MinBytes:        2 << 20,
			},
		},
		capacity: api.Capacity{
			TotalCPUMillis:   8000,
			TotalMemoryBytes: 16 << 30,
			TotalDiskBytes:   500 << 30,
			Reported:         time.Now(),
		},
	}
}

func capableRouting() *fakeRouting {
	return &fakeRouting{caps: api.RoutingCapabilities{
		Modes:       []api.RoutingMode{spec.RoutingPort, spec.RoutingSubdomain},
		DefaultMode: spec.RoutingPort,
	}}
}

func capableBuilder() *fakeBuilder {
	return &fakeBuilder{caps: api.BuilderCapabilities{
		IsolationClass: spec.IsolationContainer,
		Strategies:     []api.BuildStrategy{spec.BuildDockerfile, spec.BuildCompose},
		SupportsCache:  true,
	}}
}

func registry(t *testing.T, runtime api.Adapter, routing api.Adapter, builder api.Adapter) *api.Registry {
	t.Helper()
	r := api.NewRegistry()
	require.NoError(t, r.Register("rt_docker", runtime))
	require.NoError(t, r.Register("rte_loopback", routing))
	if builder != nil {
		require.NoError(t, r.Register("bld_buildkit", builder))
	}
	require.NoError(t, r.Register("svcs_docker", servicesdocker.New()))
	require.NoError(t, r.SetDefault(api.CategoryServices, "svcs_docker"))
	return r
}

// registryWithoutServices is the same install with nothing that can provision.
func registryWithoutServices(t *testing.T) *api.Registry {
	t.Helper()
	r := api.NewRegistry()
	require.NoError(t, r.Register("rt_docker", capableRuntime()))
	require.NoError(t, r.Register("rte_loopback", capableRouting()))
	require.NoError(t, r.Register("bld_buildkit", capableBuilder()))
	return r
}

func plannableSpec() *spec.AppSpec {
	return &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		AppID:         "app_01HQ8",
		Revision:      1,
		Source:        spec.Source{Type: spec.SourceGit, URL: "https://github.com/acme/notes", Ref: "main"},
		Build:         spec.Build{Strategy: spec.BuildDockerfile, AdapterRef: "bld_buildkit"},
		Workloads:     []spec.Workload{{Name: "web", Primary: true, Exposed: true}},
		Routing:       spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPort, Port: 8080},
		Runtime:       spec.RuntimeRef{AdapterRef: "rt_docker", IsolationFloor: spec.IsolationContainer},
		Deploy:        spec.Deploy{Strategy: spec.DeployRecreate},
		Resources:     spec.Resources{CPUMillis: 500, MemoryBytes: 512 << 20, DiskBytes: 1 << 30},
	}
}

func newPlanner(t *testing.T) *planner.Planner {
	t.Helper()
	return planner.New(
		registry(t, capableRuntime(), capableRouting(), capableBuilder()),
		policy.Static(policy.Default()),
		fixedAllocations{},
	)
}

// --- the happy path --------------------------------------------------------

func TestPlanSucceedsAndCreatesNothing(t *testing.T) {
	plan, err := newPlanner(t).Check(context.Background(), plannableSpec())
	require.NoError(t, err)

	require.Equal(t, "app_01HQ8", plan.AppID)
	for _, check := range []string{"spec_valid", "policy", "adapters", "capabilities", "isolation", "slots", "capacity"} {
		require.Equal(t, "ok", plan.Checks[check], "check %s should have run", check)
	}

	// R-026: every bundle is private, and it is a field rather than an
	// assumption so an adapter that cannot do it fails loudly.
	require.True(t, plan.Bundle.Network.Private)
	require.Len(t, plan.Bundle.Workloads, 1)

	// Env is empty at plan time: resolving it means reading secrets, which is a
	// side effect and belongs after the plan boundary.
	require.Empty(t, plan.Bundle.Workloads[0].Env)
}

// TestR131_ProvisioningWithNoProvisionerIsRefusedAtPlanTime asserts R-131.
//
// The plan boundary's whole value: an app that chose "provision one" on an
// install with no provisioner learns so before a build runs and before the
// running version is stopped, not after.
func TestR131_ProvisioningWithNoProvisionerIsRefusedAtPlanTime(t *testing.T) {
	s := plannableSpec()
	s.Slots = []spec.Slot{{
		Key: "DATABASE_URL", Type: spec.SlotPostgres, Required: true,
		Resolution: &spec.Resolution{Mode: spec.ResolutionProvisioned},
	}}

	p := planner.New(registryWithoutServices(t), policy.Static(policy.Default()), fixedAllocations{})
	_, err := p.Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.PlanAdapterNotConfigured, errs.CodeOf(err))

	e := errs.As(err)
	require.Contains(t, e.Message, "PostgreSQL", "the message names what is missing")
	require.Contains(t, e.Remedy, "DATABASE_URL", "R-105: the remedy names the slot to act on")

	// The same spec plans cleanly where a provisioner is configured.
	_, err = newPlanner(t).Check(context.Background(), s)
	require.NoError(t, err)
}

// TestR131_AnUnprovisionableTypeIsRefused asserts that Pando says so rather
// than pretending: S3 and SMTP are slot types Pando recognizes and deliberately
// does not stand up (R-010).
func TestR131_AnUnprovisionableTypeIsRefused(t *testing.T) {
	s := plannableSpec()
	s.Slots = []spec.Slot{{
		Key: "S3_BUCKET", Type: spec.SlotS3, Required: true,
		Resolution: &spec.Resolution{Mode: spec.ResolutionProvisioned},
	}}

	_, err := newPlanner(t).Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.PlanAdapterNotConfigured, errs.CodeOf(err))
}

// --- the four errors phase 3's Done when names ----------------------------

// TestR132_UnfilledRequiredSlotBlocksDeploy asserts R-132.
func TestR132_UnfilledRequiredSlotBlocksDeploy(t *testing.T) {
	s := plannableSpec()
	s.Slots = []spec.Slot{{Key: "REDIS_URL", Type: spec.SlotRedis, Required: true}}

	_, err := newPlanner(t).Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.PlanSlotUnfilled, errs.CodeOf(err))

	e := errs.As(err)
	require.Contains(t, e.Message, "Redis", "the message names what is missing")
	require.NotEmpty(t, e.Remedy, "R-132 promises a way forward")
	require.Contains(t, e.Remedy, "REDIS_URL")

	slots, ok := e.Details["slots"].([]map[string]string)
	require.True(t, ok)
	require.Len(t, slots, 1)
	require.Equal(t, "REDIS_URL", slots[0]["key"])

	// Filling it clears the block.
	s.Slots[0].Resolution = &spec.Resolution{Mode: spec.ResolutionProvisioned}
	_, err = newPlanner(t).Check(context.Background(), s)
	require.NoError(t, err)
}

// Every unfilled slot is named, not just the first: filling slots one deploy
// attempt at a time is the experience R-132 exists to prevent.
func TestR132_EveryUnfilledSlotIsNamed(t *testing.T) {
	s := plannableSpec()
	s.Slots = []spec.Slot{
		{Key: "REDIS_URL", Type: spec.SlotRedis, Required: true},
		{Key: "DATABASE_URL", Type: spec.SlotPostgres, Required: true},
		{Key: "LOG_LEVEL", Type: spec.SlotUnknown, Required: false},
	}

	_, err := newPlanner(t).Check(context.Background(), s)
	require.Error(t, err)

	slots := errs.As(err).Details["slots"].([]map[string]string)
	require.Len(t, slots, 2, "both required slots, and not the optional one")
}

// TestR254_CapabilityUnsupportedBlocksDeploy asserts R-254.
func TestR254_CapabilityUnsupportedBlocksDeploy(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*spec.AppSpec)
		runtime func() *fakeRuntime
		routing func() *fakeRouting
		detail  string
	}{
		"routing mode not advertised": {
			mutate:  func(s *spec.AppSpec) { s.Routing.Mode = spec.RoutingPath; s.Routing.PathPrefix = "/notes" },
			runtime: capableRuntime,
			routing: capableRouting,
		},
		"no private network": {
			mutate: func(*spec.AppSpec) {},
			runtime: func() *fakeRuntime {
				rt := capableRuntime()
				rt.caps.SupportsPrivateNetwork = false
				return rt
			},
			routing: capableRouting,
			detail:  "private_network",
		},
		"no persistent volumes": {
			mutate: func(s *spec.AppSpec) {
				s.Volumes = []spec.Volume{{ID: "vol_1", Name: "data", Declared: spec.VolumeFromUser}}
			},
			runtime: func() *fakeRuntime {
				rt := capableRuntime()
				rt.caps.SupportsPersistentVolumes = false
				return rt
			},
			routing: capableRouting,
			detail:  "persistent_volumes",
		},
		"start-then-swap unsupported": {
			mutate: func(s *spec.AppSpec) { s.Deploy.Strategy = spec.DeployStartThenSwap },
			runtime: func() *fakeRuntime {
				rt := capableRuntime()
				rt.caps.SupportsStartThenSwap = false
				return rt
			},
			routing: capableRouting,
			detail:  "start_then_swap",
		},
		"multiple workloads unsupported": {
			mutate: func(s *spec.AppSpec) {
				s.Workloads = append(s.Workloads, spec.Workload{Name: "worker"})
			},
			runtime: func() *fakeRuntime {
				rt := capableRuntime()
				rt.caps.SupportsMultipleWorkloads = false
				return rt
			},
			routing: capableRouting,
		},
	}

	for name, tc := range cases {
		s := plannableSpec()
		tc.mutate(s)

		p := planner.New(registry(t, tc.runtime(), tc.routing(), capableBuilder()),
			policy.Static(policy.Default()), fixedAllocations{})

		_, err := p.Check(context.Background(), s)
		require.Error(t, err, name)
		require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err), name)
		require.NotEmpty(t, errs.As(err).Remedy, name)
		if tc.detail != "" {
			require.Equal(t, tc.detail, errs.As(err).Details["capability"], name)
		}
	}
}

// TestR242_CapacityWouldOversubscribeBlocksDeploy asserts R-242.
//
// Details carry requested, allocated, and the adapter's own total: a capacity
// refusal that does not show its arithmetic is not actionable.
func TestR242_CapacityWouldOversubscribeBlocksDeploy(t *testing.T) {
	s := plannableSpec()
	s.Resources.MemoryBytes = 8 << 30

	p := planner.New(
		registry(t, capableRuntime(), capableRouting(), capableBuilder()),
		policy.Static(policy.Default()),
		fixedAllocations{alloc: planner.Allocation{MemoryBytes: 12 << 30}},
	)

	_, err := p.Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))

	e := errs.As(err)
	require.Equal(t, "memory", e.Details["resource"])
	require.NotEmpty(t, e.Details["requested"])
	require.NotEmpty(t, e.Details["already_allocated"])
	require.NotEmpty(t, e.Details["total"])
	require.NotEmpty(t, e.Details["available"])
	require.Contains(t, e.Remedy, "Lower what it asks for")
}

// TestR242_OversubscriptionIsRefusedByDefault asserts R-242's default holds
// for CPU as well as memory: neither policy field set, both refused.
func TestR242_OversubscriptionIsRefusedByDefault(t *testing.T) {
	cases := map[string]struct {
		request func(*spec.AppSpec)
		alloc   planner.Allocation
	}{
		"CPU":    {func(s *spec.AppSpec) { s.Resources.CPUMillis = 4000 }, planner.Allocation{CPUMillis: 6000}},
		"memory": {func(s *spec.AppSpec) { s.Resources.MemoryBytes = 8 << 30 }, planner.Allocation{MemoryBytes: 12 << 30}},
	}
	for resource, tc := range cases {
		s := plannableSpec()
		tc.request(s)
		p := planner.New(
			registry(t, capableRuntime(), capableRouting(), capableBuilder()),
			policy.Static(policy.Default()),
			fixedAllocations{alloc: tc.alloc},
		)
		_, err := p.Check(context.Background(), s)
		require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err), resource)
		require.Equal(t, resource, errs.As(err).Details["resource"], resource)
	}
}

// TestR242_PolicyMayAllowCPUAndMemoryOversubscription asserts the amendment:
// each of CPU and memory may be oversubscribed when host policy allows that
// one, and allowing one does not allow the other.
func TestR242_PolicyMayAllowCPUAndMemoryOversubscription(t *testing.T) {
	over := plannableSpec()
	over.Resources.CPUMillis = 4000
	over.Resources.MemoryBytes = 8 << 30
	alloc := planner.Allocation{CPUMillis: 6000, MemoryBytes: 12 << 30}

	check := func(doc policy.Document) error {
		p := planner.New(
			registry(t, capableRuntime(), capableRouting(), capableBuilder()),
			policy.Static(doc), fixedAllocations{alloc: alloc})
		_, err := p.Check(context.Background(), over)
		return err
	}

	cpuOnly := policy.Default()
	cpuOnly.AllowCPUOversubscription = true
	err := check(cpuOnly)
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	require.Equal(t, "memory", errs.As(err).Details["resource"], "allowing CPU does not allow memory")

	memoryOnly := policy.Default()
	memoryOnly.AllowMemoryOversubscription = true
	err = check(memoryOnly)
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	require.Equal(t, "CPU", errs.As(err).Details["resource"], "allowing memory does not allow CPU")

	both := policy.Default()
	both.AllowCPUOversubscription, both.AllowMemoryOversubscription = true, true
	require.NoError(t, check(both))
}

// TestR242_DiskIsNeverOversubscribed asserts the amendment's limit: no policy
// lets the apps on a runtime ask for more disk than it has.
func TestR242_DiskIsNeverOversubscribed(t *testing.T) {
	s := plannableSpec()
	s.Resources.DiskBytes = 300 << 30

	doc := policy.Default()
	doc.AllowCPUOversubscription, doc.AllowMemoryOversubscription = true, true

	p := planner.New(
		registry(t, capableRuntime(), capableRouting(), capableBuilder()),
		policy.Static(doc),
		fixedAllocations{alloc: planner.Allocation{DiskBytes: 300 << 30}},
	)
	_, err := p.Check(context.Background(), s)
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	require.Equal(t, "disk space", errs.As(err).Details["resource"])
}

// TestR242_AnUnreadablePolicyRefusesRatherThanOversubscribing asserts that
// the planner never guesses at host policy: whichever of its reads fails —
// the oversubscription allowance among them — the plan is refused with that
// failure, not made as if the policy allowed it.
func TestR242_AnUnreadablePolicyRefusesRatherThanOversubscribing(t *testing.T) {
	over := plannableSpec()
	over.Resources.CPUMillis = 4000
	over.Resources.MemoryBytes = 8 << 30
	alloc := planner.Allocation{CPUMillis: 6000, MemoryBytes: 12 << 30}
	allowed := policy.Default()
	allowed.AllowCPUOversubscription, allowed.AllowMemoryOversubscription = true, true
	unreadable := errors.New("policy unreadable")

	check := func(failAt int) (int, error) {
		reads := 0
		ev := policy.New(func(context.Context) (policy.Document, error) {
			reads++
			if reads == failAt {
				return policy.Document{}, unreadable
			}
			return allowed, nil
		})
		p := planner.New(registry(t, capableRuntime(), capableRouting(), capableBuilder()), ev, fixedAllocations{alloc: alloc})
		_, err := p.Check(context.Background(), over)
		return reads, err
	}

	reads, err := check(0)
	require.NoError(t, err, "readable, the policy allows the oversubscription")
	require.Positive(t, reads)
	for failAt := 1; failAt <= reads; failAt++ {
		_, err := check(failAt)
		require.ErrorIs(t, err, unreadable, "read %d of %d failing refuses the plan", failAt, reads)
	}
}

// Replanning an app must not count its own current allocation against itself.
func TestCapacityExcludesTheAppBeingPlanned(t *testing.T) {
	s := plannableSpec()
	s.Resources.MemoryBytes = 8 << 30

	// The allocations source is asked to exclude this app, and does.
	p := planner.New(
		registry(t, capableRuntime(), capableRouting(), capableBuilder()),
		policy.Static(policy.Default()),
		fixedAllocations{alloc: planner.Allocation{MemoryBytes: 4 << 30}},
	)
	_, err := p.Check(context.Background(), s)
	require.NoError(t, err, "8 GiB requested plus 4 GiB elsewhere fits in 16 GiB")
}

// TestR024_NoAdapterMeetsPolicyBlocksDeploy asserts R-024 and R-114.
func TestR024_NoAdapterMeetsPolicyBlocksDeploy(t *testing.T) {
	hardened := policy.Default()
	hardened.MinRuntimeIsolation = spec.IsolationVM

	p := planner.New(
		registry(t, capableRuntime(), capableRouting(), capableBuilder()),
		policy.Static(hardened),
		fixedAllocations{},
	)

	_, err := p.Check(context.Background(), plannableSpec())
	require.Error(t, err)
	require.Equal(t, errs.PlanNoAdapterMeetsPolicy, errs.CodeOf(err))

	e := errs.As(err)
	require.Equal(t, int(spec.IsolationVM), e.Details["required_floor"], "details name the required floor")
	require.Equal(t, int(spec.IsolationContainer), e.Details["adapter_class"])
	require.NotEmpty(t, e.Details["configured_runtimes"], "and each configured adapter's class")
}

// R-114: the build floor is independent of the runtime floor, and is reported
// separately — an operator who set one and not the other deserves to be told
// which.
func TestR114_BuildIsolationFloorIsIndependent(t *testing.T) {
	hardened := policy.Default()
	hardened.MinBuildIsolation = spec.IsolationVM
	hardened.MinRuntimeIsolation = spec.IsolationContainer

	p := planner.New(
		registry(t, capableRuntime(), capableRouting(), capableBuilder()),
		policy.Static(hardened),
		fixedAllocations{},
	)

	_, err := p.Check(context.Background(), plannableSpec())
	require.Error(t, err)
	require.Equal(t, errs.PlanNoAdapterMeetsPolicy, errs.CodeOf(err))
	require.Equal(t, "bld_buildkit", errs.As(err).Details["adapter_ref"],
		"the builder is named, not the runtime")
}

// --- the rest of the order -------------------------------------------------

// R-092 is checked before anything would clone, so a blocked source produces
// zero disk writes.
func TestR092_BlockedSourceFailsBeforeAnythingElse(t *testing.T) {
	restricted := policy.Default()
	restricted.SourceAllowlist = []string{"github.corp.com"}

	p := planner.New(
		registry(t, capableRuntime(), capableRouting(), capableBuilder()),
		policy.Static(restricted),
		fixedAllocations{},
	)

	// Give the spec other problems too. The source check must still be what
	// fires, because it is the one that has to happen before a clone.
	s := plannableSpec()
	s.Slots = []spec.Slot{{Key: "REDIS_URL", Type: spec.SlotRedis, Required: true}}

	_, err := p.Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.PolicySourceNotAllowed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "github.com")

	// An allowed source proceeds past the check.
	restricted.SourceAllowlist = []string{"github.com"}
	p = planner.New(registry(t, capableRuntime(), capableRouting(), capableBuilder()),
		policy.Static(restricted), fixedAllocations{})
	_, err = p.Check(context.Background(), plannableSpec())
	require.NoError(t, err)
}

func TestUnconfiguredAdapterIsNamed(t *testing.T) {
	s := plannableSpec()
	s.Runtime.AdapterRef = "rt_incus"

	_, err := newPlanner(t).Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.PlanAdapterNotConfigured, errs.CodeOf(err))
	require.Equal(t, "rt_incus", errs.As(err).Details["adapter_ref"])
	require.NotEmpty(t, errs.As(err).Details["configured"], "the alternatives are listed")
}

// R-254: the planner refuses to plan against an unhealthy adapter rather than
// failing mid-deploy.
func TestUnhealthyAdapterRefusesToPlan(t *testing.T) {
	rt := capableRuntime()
	rt.unhealthy = errors.New("dial unix /var/run/docker.sock: connect: connection refused")

	p := planner.New(registry(t, rt, capableRouting(), capableBuilder()),
		policy.Static(policy.Default()), fixedAllocations{})

	_, err := p.Check(context.Background(), plannableSpec())
	require.Error(t, err)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Equal(t, "rt_docker", errs.As(err).Details["adapter_ref"])

	// The driver's own message is logged, not returned.
	b, marshalErr := json.Marshal(errs.As(err))
	require.NoError(t, marshalErr)
	require.NotContains(t, string(b), "docker.sock")
}

// An invalid spec fails at step 1, before any adapter is consulted.
func TestInvalidSpecFailsFirst(t *testing.T) {
	s := plannableSpec()
	s.Workloads[0].Primary = false
	s.Runtime.AdapterRef = "rt_nonexistent"

	_, err := newPlanner(t).Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.ValidPrimaryWorkload, errs.CodeOf(err),
		"validation runs before adapter resolution")
}

func TestBuildStrategyUnsupportedByBuilder(t *testing.T) {
	s := plannableSpec()
	s.Build.Strategy = spec.BuildBuildpack

	_, err := newPlanner(t).Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.Equal(t, "bld_buildkit", errs.As(err).Details["adapter_ref"])
}

// TestR024_SourceThatMustBeBuiltNeedsABuilder asserts R-024: there is no "just
// build it here" fallback, so an install with no builder configured refuses at
// plan time rather than at step 9 of a deploy.
func TestR024_SourceThatMustBeBuiltNeedsABuilder(t *testing.T) {
	s := plannableSpec()
	s.Build.AdapterRef = ""

	// A registry with no builder at all.
	r := api.NewRegistry()
	require.NoError(t, r.Register("rt_docker", capableRuntime()))
	require.NoError(t, r.Register("rte_loopback", capableRouting()))
	p := planner.New(r, policy.Static(policy.Default()), fixedAllocations{})

	_, err := p.Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.PlanNoAdapterMeetsPolicy, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "nothing configured to build it")

	// A prebuilt image needs no builder and plans fine.
	s.Source = spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/notes:v1"}
	s.Build.Strategy = spec.BuildPrebuilt
	_, err = p.Check(context.Background(), s)
	require.NoError(t, err)
}

// TestR105_AnUnsupportedBuildMethodNamesWhatIsAvailable asserts R-105 on the
// refusal a real app actually hit.
//
// It used to read: `"bld_buildkit" cannot build this app the way it is set up.
// Choose a different build method, or a different builder.` That names the
// builder when the problem is the build method, and offers a choice without
// saying what the choices are — so the only way forward was to read the
// adapter's source.
func TestR105_AnUnsupportedBuildMethodNamesWhatIsAvailable(t *testing.T) {
	s := plannableSpec()
	// Static rather than compose: a compose spec needs every workload to carry
	// an image or a build, so an invalid one is refused before it reaches the
	// capability check. This asserts the capability message, not validation.
	s.Build.Strategy = spec.BuildStatic

	// A builder that does what the shipped one does, and no more. The fixture
	// above declares compose as well, which no real builder implements — so
	// this path had no unit coverage at all until an app hit it.
	p := planner.New(
		registry(t, capableRuntime(), capableRouting(), &fakeBuilder{caps: api.BuilderCapabilities{
			IsolationClass: spec.IsolationContainer,
			Strategies:     []api.BuildStrategy{spec.BuildDockerfile},
		}}),
		policy.Static(policy.Default()),
		fixedAllocations{},
	)

	_, err := p.Check(context.Background(), s)
	require.Error(t, err)
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))

	e := errs.As(err)
	require.Contains(t, e.Message, "static", "it names the method that was asked for")
	require.Contains(t, e.Message, "dockerfile", "and the one this installation can do")
	require.Contains(t, e.Remedy, "dockerfile")

	supported, ok := e.Details["supported"].([]string)
	require.True(t, ok)
	require.Equal(t, []string{"dockerfile"}, supported)
}

func (f *fakeRuntime) ApplyEdge(context.Context, api.EdgePlan) error { return nil }
func (f *fakeRuntime) ObserveEdge(context.Context, string) (api.EdgeState, error) {
	return api.EdgeState{}, nil
}
func (f *fakeRuntime) RemoveEdge(context.Context, string) error { return nil }
func (f *fakeRuntime) Edges(context.Context) ([]string, error)  { return nil, nil }
func (f *fakeRuntime) EdgeVolumes(context.Context) ([]api.VolumeHandle, error) {
	return nil, nil
}
func (f *fakeRouting) Edge(context.Context, api.EdgeRequest) (api.EdgePlan, bool, error) {
	return api.EdgePlan{}, false, nil
}

// fitRuntime answers LargestFitFor per app: the room where that app may go,
// with what it already holds there counted as free, as a runtime on several
// machines does.
type fitRuntime struct {
	*fakeRuntime
	fits map[string]*api.Fit
}

func (f fitRuntime) LargestFitFor(_ context.Context, appID string) (*api.Fit, error) {
	return f.fits[appID], nil
}

// TestR242_AnAppNoSinglePlaceHasRoomForIsRefusedAtPlanTime asserts R-242 on
// a runtime spread over several machines: the totals have room, no one place
// does, and the refusal comes from the planner — before anything is built or
// started — with a message that says why (R-105).
func TestR242_AnAppNoSinglePlaceHasRoomForIsRefusedAtPlanTime(t *testing.T) {
	rt := capableRuntime()
	rt.capacity = api.Capacity{TotalCPUMillis: 12000, TotalMemoryBytes: 24 << 30}
	s := plannableSpec()
	s.Resources.MemoryBytes = 4 << 30

	p := planner.New(
		registry(t, fitRuntime{rt, map[string]*api.Fit{s.AppID: {CPUMillis: 4000, MemoryBytes: 5 << 29}}},
			capableRouting(), capableBuilder()),
		policy.Static(policy.Default()), fixedAllocations{})
	_, err := p.Check(context.Background(), s)
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	e := errs.As(err)
	require.Equal(t, "memory", e.Details["resource"])
	require.Contains(t, e.Message, "No single place")
	require.Contains(t, e.Message, "all run in one place")
	require.NotEmpty(t, e.Remedy)

	// Memory oversubscription allowed by policy lifts this check too.
	doc := policy.Default()
	doc.AllowMemoryOversubscription = true
	p = planner.New(
		registry(t, fitRuntime{rt, map[string]*api.Fit{s.AppID: {CPUMillis: 4000, MemoryBytes: 5 << 29}}},
			capableRouting(), capableBuilder()),
		policy.Static(doc), fixedAllocations{})
	_, err = p.Check(context.Background(), s)
	require.NoError(t, err)
}

// TestR242_AWorkloadLargerThanTheLargestFitIsRefused asserts R-242 with one
// answer for every app, as a cluster gives (scale/6): room in total is not
// room in one place. The refusal names the largest space there is, an app
// that fits is planned, and a policy that allows memory oversubscription
// leaves the decision to the runtime.
func TestR242_AWorkloadLargerThanTheLargestFitIsRefused(t *testing.T) {
	rt := capableRuntime()
	rt.largestFit = &api.Fit{CPUMillis: 4000, MemoryBytes: 2 << 30}

	s := plannableSpec()
	s.Resources.MemoryBytes = 4 << 30
	p := planner.New(
		registry(t, rt, capableRouting(), capableBuilder()),
		policy.Static(policy.Default()),
		fixedAllocations{},
	)
	_, err := p.Check(context.Background(), s)
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	e := errs.As(err)
	require.Equal(t, "memory", e.Details["resource"])
	require.Contains(t, e.Message, "2.0 GiB")
	require.Contains(t, e.Remedy, "Lower what")

	s.Resources.MemoryBytes = 1 << 30
	_, err = p.Check(context.Background(), s)
	require.NoError(t, err, "a workload that fits on one machine is planned")

	s.Resources.MemoryBytes = 4 << 30
	doc := policy.Default()
	doc.AllowMemoryOversubscription = true
	p = planner.New(
		registry(t, rt, capableRouting(), capableBuilder()),
		policy.Static(doc),
		fixedAllocations{},
	)
	_, err = p.Check(context.Background(), s)
	require.NoError(t, err)
}

// TestR242_ARedeployThatFitsWhereTheAppRunsIsAllowed asserts the other half:
// the runtime counts what the app already holds as free where it runs, so an
// app on a full machine that fits there in place is not refused, while a new
// app of the same size is.
func TestR242_ARedeployThatFitsWhereTheAppRunsIsAllowed(t *testing.T) {
	rt := capableRuntime()
	rt.capacity = api.Capacity{TotalCPUMillis: 12000, TotalMemoryBytes: 24 << 30}
	running := plannableSpec()
	running.Resources.MemoryBytes = 2 << 30
	fresh := plannableSpec()
	fresh.AppID = "app_01HQ9"
	fresh.Resources.MemoryBytes = 2 << 30

	fits := map[string]*api.Fit{
		// 512 MB free on its machine, plus the 2 GB it holds there.
		running.AppID: {CPUMillis: 1000, MemoryBytes: 5 << 29},
		// The roomiest machine for a new app: 1 GB free.
		fresh.AppID: {CPUMillis: 1000, MemoryBytes: 1 << 30},
	}
	p := planner.New(registry(t, fitRuntime{rt, fits}, capableRouting(), capableBuilder()),
		policy.Static(policy.Default()), fixedAllocations{})
	_, err := p.Check(context.Background(), running)
	require.NoError(t, err)
	_, err = p.Check(context.Background(), fresh)
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
}
