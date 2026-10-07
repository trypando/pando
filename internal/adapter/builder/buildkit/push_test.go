package buildkit

import (
	"context"
	"encoding/json"
	"testing"

	bkclient "github.com/moby/buildkit/client"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR120_APushIsPinnedByTheDigestTheRegistryReported asserts R-120 for a
// pushed build: what the build returns is repository@digest, read from the
// exporter's response, and a push that reports no digest is a failure rather
// than a tag nobody can pin.
func TestR120_APushIsPinnedByTheDigestTheRegistryReported(t *testing.T) {
	target := &api.PushTarget{Repository: "registry.internal:5000/pando/apps/app_1", Tag: "dep_1"}

	got, err := pushedResult(target, &bkclient.SolveResponse{ExporterResponse: map[string]string{
		digestKey: "sha256:abc",
	}})
	require.NoError(t, err)
	require.Equal(t, "registry.internal:5000/pando/apps/app_1@sha256:abc", got.ImageRef)
	require.Equal(t, "sha256:abc", got.Digest)

	_, err = pushedResult(target, &bkclient.SolveResponse{})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "registry.internal:5000")
}

// TestR194_ThePushCredentialGoesOnlyToItsRegistry asserts R-194 for the
// builder: the push credential is handed to buildkitd only through the
// session, only for the registry being pushed to, and appears nowhere in the
// exporter's attributes, which buildkitd logs and records.
func TestR194_ThePushCredentialGoesOnlyToItsRegistry(t *testing.T) {
	target := &api.PushTarget{
		Repository: "registry.internal:5000/pando/apps/app_1", Tag: "DEP_1",
		Auth: &api.RegistryAuth{Registry: "registry.internal:5000", Username: "pando", Password: secret.New("hunter2-registry")},
	}

	export := pushExport(target)
	require.Equal(t, bkclient.ExporterImage, export.Type)
	require.Equal(t, "registry.internal:5000/pando/apps/app_1:dep_1", export.Attrs["name"])
	require.Equal(t, "true", export.Attrs["push"])
	require.NotContains(t, export.Attrs, "registry.insecure", "plain HTTP only when the operator says so")
	raw, err := json.Marshal(export.Attrs)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "hunter2-registry")

	got := credentialFor(target.Auth, "registry.internal:5000", "registry.internal:5000")
	require.Equal(t, "pando", got.Username)
	require.Equal(t, "hunter2-registry", got.Password)
	require.Empty(t, credentialFor(target.Auth, "registry.internal:5000", "docker.io").Password,
		"a base image's registry is not given the install's credential")
	require.Empty(t, credentialFor(nil, "registry.internal:5000", "registry.internal:5000").Password)

	require.NotNil(t, pushAuth(target))

	target.Insecure = true
	require.Equal(t, "true", pushExport(target).Attrs["registry.insecure"])
}

// A build is given exactly one place to put its image.
func TestABuildWithTwoPlacesForItsImageIsRefused(t *testing.T) {
	a := New()
	a.cli = &bkclient.Client{}
	_, err := a.Build(context.Background(), api.BuildRequest{
		Source: view{root: t.TempDir()}, Strategy: "dockerfile",
		ImageSink: discard{}, Push: &api.PushTarget{Repository: "r/x"},
	})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
