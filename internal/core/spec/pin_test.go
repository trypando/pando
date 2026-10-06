package spec_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

func imageSpec(image, digest string) *spec.AppSpec {
	return &spec.AppSpec{Source: spec.Source{Type: spec.SourceImage, Image: image, Digest: digest}}
}

// TestR120_AnEditedImageDoesNotKeepTheOldDigest asserts that a digest pins
// only the reference it was resolved from: changing the tag and leaving the
// digest would otherwise go on running the old image under the new name.
func TestR120_AnEditedImageDoesNotKeepTheOldDigest(t *testing.T) {
	next := imageSpec("ghcr.io/acme/web:2", "sha256:aa")
	spec.DropStalePin(imageSpec("ghcr.io/acme/web:1", "sha256:aa"), next)
	require.Empty(t, next.Source.Digest)

	// The same image keeps its pin.
	same := imageSpec("ghcr.io/acme/web:1", "sha256:aa")
	spec.DropStalePin(imageSpec("ghcr.io/acme/web:1", "sha256:aa"), same)
	require.Equal(t, "sha256:aa", same.Source.Digest)

	// A digest changed alongside the image is a pin somebody chose.
	chosen := imageSpec("ghcr.io/acme/web:2", "sha256:bb")
	spec.DropStalePin(imageSpec("ghcr.io/acme/web:1", "sha256:aa"), chosen)
	require.Equal(t, "sha256:bb", chosen.Source.Digest)
}
