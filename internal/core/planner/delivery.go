package planner

import (
	"context"
	"fmt"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// InstallRegistry is what the planner asks of the install's image registry:
// only whether there is one, and whether builds always go through it (issue
// #72, PR 5). *imageregistry.Registry satisfies it, a nil one included.
type InstallRegistry interface {
	Configured() bool
	Always() bool
	Host() string
}

// InstallRegistries reads the install's registry as it is now: it can be
// changed from the console while Pando runs (*imageregistry.Service).
type InstallRegistries interface {
	CurrentRegistry(ctx context.Context) (InstallRegistry, error)
}

// fixedRegistry is a registry that does not change, for tests.
type fixedRegistry struct{ r InstallRegistry }

func (f fixedRegistry) CurrentRegistry(context.Context) (InstallRegistry, error) { return f.r, nil }

// FixedRegistry answers InstallRegistries with r every time.
func FixedRegistry(r InstallRegistry) InstallRegistries { return fixedRegistry{r} }

// WithInstallRegistry tells the planner about the install's image registry.
// Without it the planner knows of none, and a runtime that only pulls from a
// registry is refused.
func (p *Planner) WithInstallRegistry(r InstallRegistries) *Planner {
	p.installRegistry = r
	return p
}

// ChooseDelivery decides how a built image reaches the runtime, from data
// (R-254): the runtime's ImageDelivery, the builder's SupportsPush, and whether
// the install has a registry.
//
// Import when the runtime takes one and the install does not send every build
// through its registry; otherwise the registry, when the runtime pulls from
// one, one is configured and the builder can push. Anything else is a
// plan-time refusal that names what is missing, rather than a failure after
// the build has run.
func ChooseDelivery(runtimeRef string, rc api.RuntimeCapabilities, builderRef string, bc api.BuilderCapabilities, reg InstallRegistry) (api.ImageDelivery, error) {
	configured := reg != nil && reg.Configured()
	always := configured && reg.Always()
	pulls := rc.Delivers(api.ImageDeliveryRegistry)
	viaRegistry := pulls && configured && bc.SupportsPush

	if rc.Delivers(api.ImageDeliveryImport) && (!always || !viaRegistry) {
		return api.ImageDeliveryImport, nil
	}
	if viaRegistry {
		return api.ImageDeliveryRegistry, nil
	}

	switch {
	case !pulls:
		return "", errs.Newf(errs.PlanCapabilityUnsupported,
			"This app has to be built, and the runtime %q cannot run an image built here: it neither takes a built image directly nor pulls one from a registry.", runtimeRef).
			WithDetail("runtime", runtimeRef).
			WithRemedy("Run this app on a runtime that can run built images, or point it at a published image instead.")
	case !configured:
		return "", errs.Newf(errs.PlanCapabilityUnsupported,
			"This app runs on the runtime %q, which pulls every image from a registry. This install has no registry configured.", runtimeRef).
			WithDetail("runtime", runtimeRef).
			WithDetail("setting", "PANDO_REGISTRY_URL").
			WithRemedy("Set PANDO_REGISTRY_URL to the registry Pando should push built images to, then restart Pando.")
	default:
		return "", errs.Newf(errs.PlanCapabilityUnsupported,
			"This app runs on the runtime %q, which pulls every image from a registry, and the builder %q cannot push to one.", runtimeRef, builderRef).
			WithDetail("runtime", runtimeRef).
			WithDetail("builder", builderRef).
			WithRemedy("Build this app with a builder that can push to a registry, such as BuildKit.")
	}
}

// checkDelivery is ChooseDelivery for an app that is built: a runtime, a
// builder and the install's registry that cannot meet refuse the plan here.
func (p *Planner) checkDelivery(ctx context.Context, s *spec.AppSpec, rc api.RuntimeCapabilities) (api.ImageDelivery, InstallRegistry, error) {
	if !needsBuild(s) || s.Build.AdapterRef == "" {
		return "", nil, nil
	}
	builder, ok := p.registry.Builder(s.Build.AdapterRef)
	if !ok {
		// checkIsolation names a builder that is not configured.
		return "", nil, nil
	}
	bc, err := builder.Capabilities(ctx)
	if err != nil {
		return "", nil, errs.Wrap(errs.AdapterUnavailable, "Pando could not read what the builder supports.", err)
	}
	var reg InstallRegistry
	if p.installRegistry != nil {
		if reg, err = p.installRegistry.CurrentRegistry(ctx); err != nil {
			return "", nil, err
		}
	}
	d, err := ChooseDelivery(s.Runtime.AdapterRef, rc, s.Build.AdapterRef, bc, reg)
	if err != nil {
		return "", nil, err
	}
	return d, reg, nil
}

// deliveryNote says where a build will go, for the plan's notes.
func deliveryNote(d api.ImageDelivery, reg InstallRegistry) string {
	if d != api.ImageDeliveryRegistry {
		return ""
	}
	return fmt.Sprintf("The built image is pushed to the install's registry, %s, and the runtime pulls it by digest.", reg.Host())
}
