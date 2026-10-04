package update_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/update"
)

// The registry half of R-357: Pando finds the signature cosign attached to
// the version's digest, and refuses an image with none, or with one that is
// not for it.
func TestR357_AnImageWithoutItsOwnSignatureIsRefused(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true)))
	t.Cleanup(srv.Close)
	repo := strings.TrimPrefix(srv.URL, "http://") + "/trypando/pando"

	img, err := random.Image(64, 1)
	require.NoError(t, err)
	tag, err := name.NewTag(repo + ":0.4.0")
	require.NoError(t, err)
	require.NoError(t, remote.Write(tag, img))

	v := &update.ImageVerifier{
		Repository: repo,
		Trusted: func(context.Context) (root.TrustedMaterial, error) {
			return trusted(t), nil
		},
	}

	_, err = v.Verify(context.Background(), "0.4.0")
	require.ErrorContains(t, err, "has no signature attached")

	// A genuine signature, but for 0.3.0's digest rather than this image's.
	digest, err := img.Digest()
	require.NoError(t, err)
	size, err := img.Size()
	require.NoError(t, err)
	mt, err := img.MediaType()
	require.NoError(t, err)
	sig := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	sig = mutate.ConfigMediaType(sig, "application/vnd.dev.sigstore.bundle.v0.3+json")
	sig, err = mutate.AppendLayers(sig, static.NewLayer(read(t, "image-0.3.0.sigstore.json"), "application/vnd.dev.sigstore.bundle.v0.3+json"))
	require.NoError(t, err)
	attached := mutate.Subject(sig, v1.Descriptor{MediaType: mt, Digest: digest, Size: size}).(v1.Image)
	sigDigest, err := attached.Digest()
	require.NoError(t, err)
	require.NoError(t, remote.Write(tag.Context().Digest(sigDigest.String()), attached))

	_, err = v.Verify(context.Background(), "0.4.0")
	require.ErrorContains(t, err, "does not verify")

	_, err = v.Verify(context.Background(), "0.9.9")
	require.ErrorContains(t, err, "could not find")
}
