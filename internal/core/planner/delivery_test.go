package planner_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/errs"
)

type installRegistry struct {
	configured, always bool
}

func (r installRegistry) Configured() bool { return r.configured }
func (r installRegistry) Always() bool     { return r.always }
func (r installRegistry) Host() string     { return "registry.internal:5000" }

var (
	imports  = api.RuntimeCapabilities{ImageDelivery: []api.ImageDelivery{api.ImageDeliveryImport}}
	pulls    = api.RuntimeCapabilities{ImageDelivery: []api.ImageDelivery{api.ImageDeliveryRegistry}}
	both     = api.RuntimeCapabilities{ImageDelivery: []api.ImageDelivery{api.ImageDeliveryImport, api.ImageDeliveryRegistry}}
	pushes   = api.BuilderCapabilities{SupportsPush: true}
	noPush   = api.BuilderCapabilities{}
	none     = installRegistry{}
	has      = installRegistry{configured: true}
	hasAlway = installRegistry{configured: true, always: true}
)

// TestR254_HowABuildReachesTheRuntimeIsDecidedFromData asserts R-254 for image
// delivery: the runtime's ImageDelivery, the builder's SupportsPush and the
// install's registry decide it, with import preferred unless the install sends
// every build through its registry (O-34).
func TestR254_HowABuildReachesTheRuntimeIsDecidedFromData(t *testing.T) {
	for name, tc := range map[string]struct {
		rc   api.RuntimeCapabilities
		bc   api.BuilderCapabilities
		reg  planner.InstallRegistry
		want api.ImageDelivery
	}{
		"single host, no registry":         {both, noPush, none, api.ImageDeliveryImport},
		"single host, a registry":          {both, pushes, has, api.ImageDeliveryImport},
		"single host, registry always":     {both, pushes, hasAlway, api.ImageDeliveryRegistry},
		"always, but the builder cannot":   {both, noPush, hasAlway, api.ImageDeliveryImport},
		"a runtime that only pulls":        {pulls, pushes, has, api.ImageDeliveryRegistry},
		"import only, registry configured": {imports, pushes, hasAlway, api.ImageDeliveryImport},
		"no registry interface at all":     {imports, noPush, nil, api.ImageDeliveryImport},
	} {
		got, err := planner.ChooseDelivery("rt", tc.rc, "bld", tc.bc, tc.reg)
		require.NoError(t, err, name)
		require.Equal(t, tc.want, got, name)
	}
}

// TestR254_NoWayToDeliverABuildIsAPlanTimeRefusal asserts R-254: a runtime,
// builder and install registry that cannot meet are refused at plan time, with
// a message that names what is missing (R-105), before anything is built.
func TestR254_NoWayToDeliverABuildIsAPlanTimeRefusal(t *testing.T) {
	_, err := planner.ChooseDelivery("rt_k8s", pulls, "bld", pushes, none)
	e := errs.As(err)
	require.NotNil(t, e)
	require.Equal(t, errs.PlanCapabilityUnsupported, e.Code)
	require.Contains(t, e.Message, "pulls every image from a registry")
	require.Contains(t, e.Remedy, "PANDO_REGISTRY_URL")

	_, err = planner.ChooseDelivery("rt_k8s", pulls, "bld_kaniko", noPush, has)
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "bld_kaniko")

	_, err = planner.ChooseDelivery("rt_odd", api.RuntimeCapabilities{}, "bld", pushes, has)
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "published image")

	// Through the planner: a runtime that only pulls, on an install with no
	// registry, refuses the plan; with one, it plans and says where the build
	// goes.
	rt := capableRuntime()
	rt.caps.ImageDelivery = []api.ImageDelivery{api.ImageDeliveryRegistry}
	pushing := capableBuilder()
	pushing.caps.SupportsPush = true
	p := planner.New(registry(t, rt, capableRouting(), pushing), policy.Static(policy.Default()), fixedAllocations{})
	_, err = p.Check(context.Background(), plannableSpec())
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))

	plan, err := p.WithInstallRegistry(has).Check(context.Background(), plannableSpec())
	require.NoError(t, err)
	require.Equal(t, "ok", plan.Checks["image_delivery"])
	require.Contains(t, plan.Notes, "The built image is pushed to the install's registry, registry.internal:5000, and the runtime pulls it by digest.")

	// A published image needs no delivery at all.
	image := plannableSpec()
	image.Source.Type = "image"
	image.Source.Image = "nginx:1.27"
	image.Build.Strategy = "prebuilt"
	image.Build.AdapterRef = ""
	plan, err = planner.New(registry(t, rt, capableRouting(), pushing), policy.Static(policy.Default()), fixedAllocations{}).
		Check(context.Background(), image)
	require.NoError(t, err)
	require.Empty(t, plan.Checks["image_delivery"])
}
