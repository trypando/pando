package deploy

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
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

// credStore is an app's sealed registry credential, in memory.
type credStore map[string]secret.Value

func (c credStore) Put(_ context.Context, _, field string, v secret.Value) error {
	c[field] = v
	return nil
}
func (c credStore) Delete(_ context.Context, _, field string) error { delete(c, field); return nil }
func (c credStore) Resolve(context.Context, string) (map[string]secret.Value, error) {
	if len(c) == 0 {
		return nil, nil
	}
	return c, nil
}

type brokenStore struct{ credStore }

func (brokenStore) Resolve(context.Context, string) (map[string]secret.Value, error) {
	return nil, errors.New("the secrets adapter is unavailable")
}

// A deploy of an image app pulls the pinned reference with the app's
// credential, and says which build it is running.
func TestAnImageDeployPullsThePinWithTheAppsCredential(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	s := &spec.AppSpec{
		Source:    spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1", Digest: digest},
		Workloads: []spec.Workload{{Name: "web", Image: "ghcr.io/acme/web:1", Primary: true}},
	}
	images := &oci.Images{Credentials: credStore(oci.Credential{
		Kind: oci.CredentialBasic, Username: "ben", Password: secret.New("tok"),
	}.Fields())}

	var log bytes.Buffer
	image, per, pull, err := imagePull(context.Background(), images, "app_1", s, "", nil, &log)
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/acme/web@"+digest, image)
	require.Equal(t, image, per["web"])
	require.NotNil(t, pull)
	require.Equal(t, "ben", pull.Username)
	require.Equal(t, "ghcr.io", pull.Registry)
	require.Contains(t, log.String(), "cccccccccccc")

	// An app built from source passes through untouched.
	git := &spec.AppSpec{Source: spec.Source{Type: spec.SourceGit, URL: "https://github.com/acme/web"}}
	image, per, pull, err = imagePull(context.Background(), images, "app_1", git, "built:1", map[string]string{"web": "built:1"}, &log)
	require.NoError(t, err)
	require.Equal(t, "built:1", image)
	require.Equal(t, "built:1", per["web"])
	require.Nil(t, pull)

	// A credential that cannot be read stops the deploy before anything runs.
	_, _, _, err = imagePull(context.Background(), &oci.Images{Credentials: brokenStore{}}, "app_1", s, "", nil, &log)
	require.Error(t, err)
}

// TestR120_AnUnpinnedImageIsPinnedBeforeItDeploys asserts R-120 for images at
// deploy time: a revision with a tag and no digest gets the digest the tag
// names now, the way an unpinned branch gets a commit.
func TestR120_AnUnpinnedImageIsPinnedBeforeItDeploys(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	img, err := random.Image(32, 1)
	require.NoError(t, err)
	ref, err := name.ParseReference(host+"/acme/web:1", name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img))
	want, err := img.Digest()
	require.NoError(t, err)

	images := &oci.Images{Inspector: oci.Inspector{Insecure: true}}
	s := &spec.AppSpec{Source: spec.Source{Type: spec.SourceImage, Image: host + "/acme/web:1"}}

	pinned, ok, err := pinnedSpec(context.Background(), images, "app_1", s)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want.String(), pinned.Source.Digest)
	require.Empty(t, s.Source.Digest, "the revision it came from is not changed; spec revisions are append-only")

	// Already pinned, built from source, or nothing to read with: nothing to do.
	for _, already := range []*spec.AppSpec{pinned, {Source: spec.Source{Type: spec.SourceGit}}} {
		_, ok, err = pinnedSpec(context.Background(), images, "app_1", already)
		require.NoError(t, err)
		require.False(t, ok)
	}
	_, ok, err = pinnedSpec(context.Background(), nil, "app_1", s)
	require.NoError(t, err)
	require.False(t, ok)

	// A tag that does not exist is a refusal, not a deploy of something else.
	_, _, err = pinnedSpec(context.Background(), images, "app_1",
		&spec.AppSpec{Source: spec.Source{Type: spec.SourceImage, Image: host + "/acme/web:nope"}})
	require.Error(t, err)
}

func TestADigestIsShownTheWayDockerShowsIt(t *testing.T) {
	require.Equal(t, "ab12ab12ab12", shortDigest("sha256:ab12ab12ab12ab12ab12"))
	require.Equal(t, "abc", shortDigest("abc"))
}
