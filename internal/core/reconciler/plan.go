package reconciler

import (
	"context"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/deploy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/secret"
)

// PlanShape builds what should be running, without resolving any secret.
//
// The reconciler compares shape — which workloads, which images, which ports
// and mounts — and that comparison runs every fifteen seconds for every app. It
// must never be a reason to decrypt anything, so environment is deliberately
// absent here and drift in it is detected by fingerprint instead (R-193).
//
// Secrets are resolved only when there is something to apply, which is the
// uncommon case.
func PlanShape(s *spec.AppSpec, image string, perWorkload map[string]state.WorkloadImage, rules api.EgressRules) api.BundlePlan {
	refs := make(map[string]string, len(perWorkload))
	for name, ran := range perWorkload {
		refs[name] = ran.Ref
	}

	plan, err := deploy.BundlePlanShape(s, image, refs, rules)
	if err != nil {
		return api.BundlePlan{}
	}
	return plan
}

// ServiceShapes adds an app's provisioned services to what should be running.
//
// An interface rather than the Runner so a Reconciler can be built without a
// deploy Runner, which the unit tests do. Nil means an install with no
// provisioner, where every app's shape is already complete.
type ServiceShapes interface {
	ServiceShapes(ctx context.Context, s *spec.AppSpec) (api.BundlePlan, error)
}

// Environments resolves the environment each workload runs with, keyed by
// workload name. Asked only when a correction is about to be applied, which is
// the uncommon case: comparing never decrypts anything (R-193).
type Environments interface {
	Environments(ctx context.Context, s *spec.AppSpec) (map[string]map[string]secret.Value, error)
}

// EnvHash is the fingerprint of the environment an app should be running with.
func EnvHash(s *spec.AppSpec, versions map[string]int) string {
	return deploy.EnvFingerprint(s, versions)
}
