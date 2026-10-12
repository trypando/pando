package deploy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// pruningRuntime has the images in has, and no others.
type pruningRuntime struct {
	namingRuntime
	has map[string]bool
}

func (p pruningRuntime) HasImage(_ context.Context, ref string) (bool, error) { return p.has[ref], nil }

type fixedBuild struct{ built *detect.Built }

func (f fixedBuild) Built(context.Context, string) (*detect.Built, error) { return f.built, nil }

// TestR311_ADetectionImageGoneFromTheRuntimeIsBuiltAgain asserts R-311's reuse
// rule holds only while the image exists: one detection imported and something
// since removed is built again, rather than run by an ID the runtime tried to
// pull as docker.io/library/sha256:… (issue #90). An image still there, or one
// in a registry, is reused as before.
func TestR311_ADetectionImageGoneFromTheRuntimeIsBuiltAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &spec.AppSpec{
		AppID:     "app_01HQ8",
		Build:     spec.Build{AdapterRef: "bld", Strategy: spec.BuildDockerfile, Dockerfile: "Dockerfile"},
		Runtime:   spec.RuntimeRef{AdapterRef: "rt"},
		Workloads: []spec.Workload{{Name: "web", Primary: true}},
	}
	checkout := &source.Checkout{Commit: "abc123"}
	built := func(image string) *detect.Built {
		return &detect.Built{Inputs: spec.BuildInputs(s), Source: "abc123", Image: image}
	}
	runner := func(has map[string]bool, b *detect.Built) *Runner {
		reg := api.NewRegistry()
		require.NoError(t, reg.Register("rt", pruningRuntime{has: has}))
		return (&Runner{registry: reg}).WithDetectionBuilds(fixedBuild{b})
	}

	_, reuse := runner(map[string]bool{"sha256:aaa": true}, built("sha256:aaa")).detectionBuild(ctx, s, checkout)
	require.True(t, reuse, "the image is still there")

	_, reuse = runner(map[string]bool{}, built("sha256:aaa")).detectionBuild(ctx, s, checkout)
	require.False(t, reuse, "the image was pruned, so the deploy builds")

	_, reuse = runner(map[string]bool{}, built("registry.local/app@sha256:bbb")).detectionBuild(ctx, s, checkout)
	require.True(t, reuse, "an image in a registry is not the runtime's to lose")
}
