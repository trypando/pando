package deploy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/secret"
)

// TestR120_AnImageAppRunsItsPinnedDigest asserts R-120 for images: the
// workloads that run the app's image are given its pinned reference, so the
// runtime pulls the digest the revision recorded rather than whatever the tag
// names today (issue #41). A sidecar with an image of its own is not.
func TestR120_AnImageAppRunsItsPinnedDigest(t *testing.T) {
	digest := "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	s := &spec.AppSpec{
		Source: spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:latest", Digest: digest},
		Workloads: []spec.Workload{
			{Name: "web", Image: "ghcr.io/acme/web:latest", Primary: true},
			{Name: "cache", Image: "redis:7"},
		},
	}
	pinned := oci.Reference(s.Source)
	require.Equal(t, "ghcr.io/acme/web@"+digest, pinned)

	per := pinWorkloads(s, nil, pinned)
	require.Equal(t, pinned, workloadImage(s.Workloads[0], per, pinned))
	require.Equal(t, "redis:7", workloadImage(s.Workloads[1], per, pinned))
}

// The app's registry credential goes to the workloads pulling the app's image
// and to nothing else, so a token for one registry is never sent to another.
func TestTheRegistryCredentialGoesOnlyToTheAppsImage(t *testing.T) {
	bundle := api.BundlePlan{Workloads: []api.WorkloadPlan{
		{Name: "web", Image: "ghcr.io/acme/web@sha256:aa"},
		{Name: "cache", Image: "redis:7"},
	}}
	auth := &api.RegistryAuth{Registry: "ghcr.io", Username: "ben", Password: secret.New("tok")}
	withPullAuth(&bundle, "ghcr.io/acme/web@sha256:aa", auth)

	require.Same(t, auth, bundle.Workloads[0].PullAuth)
	require.Nil(t, bundle.Workloads[1].PullAuth)
	require.Empty(t, bundle.Workloads[0].Env, "the credential is how the image is fetched, never something the app is given")
}

func TestADigestIsShownTheWayDockerShowsIt(t *testing.T) {
	require.Equal(t, "ab12ab12ab12", shortDigest("sha256:ab12ab12ab12ab12ab12"))
	require.Equal(t, "abc", shortDigest("abc"))
}
