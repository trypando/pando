package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	registryoci "github.com/trypando/pando/internal/adapter/imageregistry/oci"
	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

var pulling = api.RuntimeCapabilities{ImageDelivery: []api.ImageDelivery{api.ImageDeliveryRegistry}}

func installRegistry(t *testing.T, always bool) imageregistry.Provider {
	t.Helper()
	a := registryoci.New()
	raw, err := json.Marshal(map[string]any{"url": "https://registry.internal:5000", "username": "pando", "always": always,
		"credentials": map[string]string{"password": "registry-password-9"}})
	require.NoError(t, err)
	require.NoError(t, a.Configure(context.Background(), raw))
	return imageregistry.Static(imageregistry.Of("reg_1", a))
}

// TestR120_ABuiltImageIsPinnedByDigest asserts R-120 for a build pushed to the
// install's registry: the builder is asked to push to the app's repository
// under the deployment's tag, and what the deploy runs and records is
// repository@digest. A later push to the same tag changes nothing a
// deployment names.
func TestR120_ABuiltImageIsPinnedByDigest(t *testing.T) {
	const pushed = "registry.internal:5000/apps/app_01hq8@sha256:aaa"
	b := &recordingBuilder{pushes: true, result: api.BuildResult{ImageRef: pushed, Digest: "sha256:aaa"}}
	rt := &importingRuntime{caps: pulling}
	r := buildRunner(t, b, rt).WithBuildRegistry(installRegistry(t, false))

	var log strings.Builder
	image, err := r.build(context.Background(), buildApp(), &source.Checkout{}, &log, nil, "app_01HQ8", "dep_01HQ9")
	require.NoError(t, err)
	require.Equal(t, pushed, image, "what runs is the digest, not the tag")
	require.Empty(t, rt.imported, "nothing was streamed to the runtime")
	require.Nil(t, b.asked.ImageSink)
	require.NotNil(t, b.asked.Push)
	require.Equal(t, "registry.internal:5000/apps/app_01hq8", b.asked.Push.Repository)
	require.Equal(t, "dep_01hq9", b.asked.Push.Tag)
	require.Equal(t, "pando", b.asked.Push.Auth.Username, "the push credential travels with this build only")
	require.Contains(t, log.String(), "Pushed to registry.internal:5000 at aaa")

	// A compose service has a repository of its own.
	wb := &spec.WorkloadBuild{Dockerfile: "api/Dockerfile"}
	b.result = api.BuildResult{ImageRef: "registry.internal:5000/apps/app_01hq8/api@sha256:bbb", Digest: "sha256:bbb"}
	_, err = r.build(context.Background(), buildApp(), &source.Checkout{}, &log, wb, "app_01HQ8/api", "dep_01HQ9")
	require.NoError(t, err)
	require.Equal(t, "registry.internal:5000/apps/app_01hq8/api", b.asked.Push.Repository)

	// A push the builder could not pin is not run by its tag instead.
	b.result = api.BuildResult{ImageRef: "registry.internal:5000/apps/app_01hq8:dep_01hq9"}
	_, err = r.build(context.Background(), buildApp(), &source.Checkout{}, &log, nil, "app_01HQ8", "dep_01HQ9")
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
}

// The single host imports even with a registry configured (O-34), unless the
// install sends every build through it.
func TestASingleHostImportsUnlessTheRegistryIsAlwaysUsed(t *testing.T) {
	both := api.RuntimeCapabilities{ImageDelivery: []api.ImageDelivery{api.ImageDeliveryImport, api.ImageDeliveryRegistry}}

	b := &recordingBuilder{pushes: true, result: api.BuildResult{ImageRef: "pando/app:dep_1"}}
	rt := &importingRuntime{caps: both, id: "sha256:imported"}
	image, err := buildRunner(t, b, rt).WithBuildRegistry(installRegistry(t, false)).
		build(context.Background(), buildApp(), &source.Checkout{}, &strings.Builder{}, nil, "app_01HQ8", "dep_1")
	require.NoError(t, err)
	require.Equal(t, "sha256:imported", image)
	require.Nil(t, b.asked.Push)
	require.Equal(t, "dep_1", b.asked.Tag, "named by its deployment (R-146)")

	b = &recordingBuilder{pushes: true, result: api.BuildResult{ImageRef: "registry.internal:5000/apps/app_01hq8@sha256:ccc", Digest: "sha256:ccc"}}
	rt = &importingRuntime{caps: both}
	image, err = buildRunner(t, b, rt).WithBuildRegistry(installRegistry(t, true)).
		build(context.Background(), buildApp(), &source.Checkout{}, &strings.Builder{}, nil, "app_01HQ8", "dep_1")
	require.NoError(t, err)
	require.Equal(t, "registry.internal:5000/apps/app_01hq8@sha256:ccc", image)
	require.Empty(t, rt.imported)
}

// TestR254_ARuntimeThatPullsWithNoRegistryIsRefusedBeforeBuilding asserts
// R-254 at the deploy: no registry, nothing built.
func TestR254_ARuntimeThatPullsWithNoRegistryIsRefusedBeforeBuilding(t *testing.T) {
	b := &recordingBuilder{pushes: true}
	_, err := buildRunner(t, b, &importingRuntime{caps: pulling}).
		build(context.Background(), buildApp(), &source.Checkout{}, &strings.Builder{}, nil, "app_01HQ8", "dep_1")
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "image_registry")
	require.Empty(t, b.asked.Strategy, "nothing was built")
}

// TestR194_TheRegistryCredentialNeverReachesALogOrAWorkload asserts R-194 for
// the install registry's credential: it is given to the pull of a built image
// and to nothing else — not another workload's pull, not any workload's
// environment, and not the deploy log.
func TestR194_TheRegistryCredentialNeverReachesALogOrAWorkload(t *testing.T) {
	const built = "registry.internal:5000/apps/app_01hq8@sha256:aaa"
	r := &Runner{buildRegistry: installRegistry(t, false)}
	bundle := api.BundlePlan{Workloads: []api.WorkloadPlan{
		{Name: "web", Image: built, Env: map[string]secret.Value{"PORT": secret.New("8080")}},
		{Name: "cache", Image: "redis:7"},
	}}
	r.withBuiltImageAuth(context.Background(), nil, &bundle)
	require.NotNil(t, bundle.Workloads[0].PullAuth)
	require.Equal(t, "registry-password-9", bundle.Workloads[0].PullAuth.Password.Reveal())
	require.Nil(t, bundle.Workloads[1].PullAuth, "a published image is not given the install's credential")

	for _, w := range bundle.Workloads {
		for _, v := range w.Env {
			require.NotContains(t, v.Reveal(), "registry-password-9")
		}
	}
	raw, err := json.Marshal(bundle)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "registry-password-9", "a logged plan shows [redacted]")

	require.Nil(t, (&Runner{}).builtImageAuth(context.Background(), nil, built), "no registry, no credential")
}

// TestR025_TheRegistryCredentialIsOnlyForTheAppsOwnBuilds asserts R-025 for
// the install registry: an app whose spec names another app's build, in the
// install registry, is not given the install's credential to pull it.
func TestR025_TheRegistryCredentialIsOnlyForTheAppsOwnBuilds(t *testing.T) {
	const own = "registry.internal:5000/apps/app_01hq8@sha256:aaa"
	const other = "registry.internal:5000/apps/app_01other:dep_9"
	r := &Runner{buildRegistry: installRegistry(t, false)}
	s := &spec.AppSpec{
		Source:    spec.Source{Type: spec.SourceImage, Image: other},
		Workloads: []spec.Workload{{Name: "web"}, {Name: "side", Image: "registry.internal:5000/apps/app_01third/db:x"}},
	}
	bundle := api.BundlePlan{Workloads: []api.WorkloadPlan{
		{Name: "web", Image: "registry.internal:5000/apps/app_01other@sha256:bbb"},
		{Name: "side", Image: "registry.internal:5000/apps/app_01third/db:x"},
		{Name: "built", Image: own},
	}}
	r.withBuiltImageAuth(context.Background(), s, &bundle)
	require.Nil(t, bundle.Workloads[0].PullAuth, "an image app naming another app's build, pinned, gets no credential")
	require.Nil(t, bundle.Workloads[1].PullAuth, "a workload naming another app's build gets no credential")
	require.NotNil(t, bundle.Workloads[2].PullAuth, "the app's own build still gets it")
	require.Nil(t, r.builtImageAuth(context.Background(), s, other))

	require.True(t, s.NamesImage("registry.internal:5000/apps/app_01other"))
	require.False(t, (*spec.AppSpec)(nil).NamesImage(own))
	require.False(t, s.NamesImage(""))

	// A workload with a build of its own runs what was built, even pushed to
	// the repository its image names.
	built := &spec.AppSpec{Workloads: []spec.Workload{{Name: "web", Image: "registry.internal:5000/apps/app_01hq8", Build: &spec.WorkloadBuild{}}}}
	require.False(t, built.NamesImage(own))
}
