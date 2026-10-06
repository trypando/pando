package planner_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

type fakeImages struct {
	insp oci.Inspection
	err  error
	ref  string
}

func (f *fakeImages) Inspect(_ context.Context, _, ref string, _ oci.Platform) (oci.Inspection, error) {
	f.ref = ref
	return f.insp, f.err
}

func imagePlanner(t *testing.T, platform string, images planner.ImageReader) *planner.Planner {
	t.Helper()
	rt := capableRuntime()
	rt.caps.Platform = platform
	return planner.New(registry(t, rt, capableRouting(), capableBuilder()),
		policy.Static(policy.Default()), fixedAllocations{}).WithImages(images)
}

func imageAppSpec() *spec.AppSpec {
	s := plannableSpec()
	s.Source = spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1", Digest: "sha256:aa"}
	s.Build = spec.Build{Strategy: spec.BuildPrebuilt}
	s.Workloads[0].Image = "ghcr.io/acme/web:1"
	s.Workloads[0].Build = nil
	return s
}

// An image with no build for the runtime's platform is a plan-time error, so
// nothing is pulled and the running version is not stopped for it (issue #41).
func TestAnImageWithNoBuildForTheRuntimeIsRefusedAtPlanTime(t *testing.T) {
	images := &fakeImages{insp: oci.Inspection{Platforms: []oci.Platform{{OS: "linux", Architecture: "amd64"}}}}

	_, err := imagePlanner(t, "linux/arm64", images).Check(context.Background(), imageAppSpec())
	require.Equal(t, errs.PlanImagePlatformUnsupported, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "linux/amd64")
	require.NotEmpty(t, errs.As(err).Remedy)
	require.Equal(t, "ghcr.io/acme/web@sha256:aa", images.ref, "the pinned digest is what is checked, not the tag")

	plan, err := imagePlanner(t, "linux/amd64", images).Check(context.Background(), imageAppSpec())
	require.NoError(t, err)
	require.Equal(t, "ok", plan.Checks["image"])
}

// TestR203_AnImageVolumeWithNoStorageIsNotedAtPlanTime asserts R-203's warning
// for images whose spec has lost the storage the image declares: a note, never
// a blocker (warnings are never blockers).
func TestR203_AnImageVolumeWithNoStorageIsNotedAtPlanTime(t *testing.T) {
	images := &fakeImages{insp: oci.Inspection{
		Platforms: []oci.Platform{{OS: "linux", Architecture: "amd64"}},
		Config:    &oci.Config{Volumes: []string{"/data", "/srv/uploads"}},
	}}
	s := imageAppSpec()
	s.Volumes = []spec.Volume{{ID: "data", Name: "data", Declared: spec.VolumeFromImage}}
	s.Workloads[0].Mounts = []spec.Mount{{VolumeID: "data", Path: "/data"}}

	plan, err := imagePlanner(t, "linux/amd64", images).Check(context.Background(), s)
	require.NoError(t, err)
	require.Len(t, plan.Notes, 1)
	require.Contains(t, plan.Notes[0], "/srv/uploads")
	require.NotContains(t, plan.Notes[0], "/data,")
}

// A registry that cannot be read leaves the check to the pull.
func TestAnUnreadableRegistryDoesNotBlockThePlan(t *testing.T) {
	images := &fakeImages{err: errs.New(errs.AdapterFailed, "Pando could not read the image.")}
	plan, err := imagePlanner(t, "linux/amd64", images).Check(context.Background(), imageAppSpec())
	require.NoError(t, err)
	require.NotEmpty(t, plan.Notes)
}
