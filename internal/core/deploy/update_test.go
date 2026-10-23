package deploy

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// pushImage publishes a new random build under ref and returns its digest.
func pushImage(t *testing.T, ref string) string {
	t.Helper()
	img, err := random.Image(32, 1)
	require.NoError(t, err)
	parsed, err := name.ParseReference(ref, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(parsed, img))
	var digest v1.Hash
	digest, err = img.Digest()
	require.NoError(t, err)
	return digest.String()
}

func testRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// TestR143a_ACheckFindsANewBuildOfTheSameTag asserts R-143a: a check compares
// the digest the tag names now with the one the pinned revision runs.
func TestR143a_ACheckFindsANewBuildOfTheSameTag(t *testing.T) {
	host := testRegistry(t)
	image := host + "/acme/web:latest"
	first := pushImage(t, image)
	images := &oci.Images{Inspector: oci.Inspector{Insecure: true}}
	s := &spec.AppSpec{Source: spec.Source{Type: spec.SourceImage, Image: image, Digest: first}}

	got, err := checkImage(context.Background(), images, "app_1", s)
	require.NoError(t, err)
	require.Equal(t, ImageCheck{Image: image, Current: first, Latest: first}, got)

	second := pushImage(t, image)
	got, err = checkImage(context.Background(), images, "app_1", s)
	require.NoError(t, err)
	require.Equal(t, ImageCheck{Image: image, Current: first, Latest: second, Available: true}, got)
	require.Equal(t, first, s.Source.Digest, "a check writes nothing")
}

// Only an app that runs a published image has a tag to check.
func TestR143a_OnlyAnImageAppCanBeChecked(t *testing.T) {
	images := &oci.Images{Inspector: oci.Inspector{Insecure: true}}
	_, err := checkImage(context.Background(), images, "app_1",
		&spec.AppSpec{Source: spec.Source{Type: spec.SourceGit, URL: "https://github.com/acme/web"}})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))

	_, err = checkImage(context.Background(), nil, "app_1",
		&spec.AppSpec{Source: spec.Source{Type: spec.SourceImage, Image: "ghcr.io/acme/web:1"}})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err), "an install that cannot read registries says so")
}

// TestR143a_AnUpgradePinsTheBuildThatWasChecked asserts R-143a: the upgrade
// pins the digest the person saw, and is refused if the tag has moved since.
func TestR143a_AnUpgradePinsTheBuildThatWasChecked(t *testing.T) {
	host := testRegistry(t)
	image := host + "/acme/web:latest"
	first := pushImage(t, image)
	images := &oci.Images{Inspector: oci.Inspector{Insecure: true}}
	s := &spec.AppSpec{Source: spec.Source{Type: spec.SourceImage, Image: image, Digest: first}}

	_, err := upgradedSpec(context.Background(), images, "app_1", s, first)
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err), "nothing newer to upgrade to")

	second := pushImage(t, image)
	upgraded, err := upgradedSpec(context.Background(), images, "app_1", s, second)
	require.NoError(t, err)
	require.Equal(t, second, upgraded.Source.Digest)
	require.Equal(t, image, upgraded.Source.Image, "the tag asked for is kept beside the digest")
	require.Equal(t, first, s.Source.Digest, "the revision it came from is not changed; revisions are append-only")

	pushImage(t, image)
	_, err = upgradedSpec(context.Background(), images, "app_1", s, second)
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err), "the tag moved after the check")
	require.Contains(t, err.Error(), "changed since")

	_, err = upgradedSpec(context.Background(), images, "app_1", s, "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "the digest from the check is required")
}

// An app that was never deployed has no pinned image to check or upgrade.
func TestR143a_AnAppNeverDeployedHasNothingToCheck(t *testing.T) {
	r := &Runner{}
	_, err := r.CheckImage(context.Background(), state.App{ID: "app_1"})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "has not been deployed yet")

	_, err = r.UpgradeImage(context.Background(), state.App{ID: "app_1"}, "sha256:b2", "usr_1")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
}

// A tag the registry does not have is a refusal, for the check and the upgrade alike.
func TestR143a_ATagTheRegistryLacksIsRefused(t *testing.T) {
	host := testRegistry(t)
	images := &oci.Images{Inspector: oci.Inspector{Insecure: true}}
	s := &spec.AppSpec{Source: spec.Source{Type: spec.SourceImage, Image: host + "/acme/web:nope", Digest: "sha256:a1"}}

	_, err := checkImage(context.Background(), images, "app_1", s)
	require.Error(t, err)
	_, err = upgradedSpec(context.Background(), images, "app_1", s, "sha256:b2")
	require.Error(t, err)

	_, err = upgradedSpec(context.Background(), images, "app_1",
		&spec.AppSpec{Source: spec.Source{Type: spec.SourceGit, URL: "https://github.com/acme/web"}}, "sha256:b2")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err), "only an image app is upgraded")
}
