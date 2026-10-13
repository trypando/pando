package deploy

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// steeringBuilder reports whether it was asked to steer, and when it was,
// returns a plan of its own as the one it built from.
type steeringBuilder struct {
	barrierBuilder
	asked []bool
}

func (b *steeringBuilder) Build(_ context.Context, req api.BuildRequest) (api.BuildResult, error) {
	b.asked = append(b.asked, req.Steer)
	result := api.BuildResult{ImageRef: "pando/app:" + req.Tag}
	if req.Steer {
		result.GeneratedFiles = map[string]string{".pando-plan/railpack-plan.json": "steered"}
	}
	return result, nil
}

// TestR020_OnlyDetectionsBuildIsSteeredAndItsPlanIsKept asserts R-020: a
// deploy builds the plan its spec carries and nothing else, and detection's
// build, which may steer, leaves the plan it built from in the spec it built.
func TestR020_OnlyDetectionsBuildIsSteeredAndItsPlanIsKept(t *testing.T) {
	t.Parallel()
	b := &steeringBuilder{}
	reg := api.NewRegistry()
	require.NoError(t, reg.Register("bld", b))
	require.NoError(t, reg.Register("rt", namingRuntime{}))
	r := &Runner{registry: reg}
	app := func() *spec.AppSpec {
		return &spec.AppSpec{
			AppID: "app_01HQ8",
			Build: spec.Build{
				AdapterRef: "bld", Strategy: spec.BuildBuildpack,
				GeneratedFiles: map[string]string{".pando-plan/railpack-plan.json": "proposed"},
			},
			Runtime:   spec.RuntimeRef{AdapterRef: "rt"},
			Workloads: []spec.Workload{{Name: "web", Primary: true}},
		}
	}

	deployed := app()
	_, _, err := r.BuildImages(context.Background(), deployed, &source.Checkout{}, io.Discard, "dep-1")
	require.NoError(t, err)
	require.Equal(t, "proposed", deployed.Build.GeneratedFiles[".pando-plan/railpack-plan.json"])

	detected := app()
	_, _, err = r.BuildImages(detect.WithSteer(context.Background()), detected, &source.Checkout{}, io.Discard, "detect-1")
	require.NoError(t, err)
	require.Equal(t, "steered", detected.Build.GeneratedFiles[".pando-plan/railpack-plan.json"])
	require.Equal(t, []bool{false, true}, b.asked)
}
