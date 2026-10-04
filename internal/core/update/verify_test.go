package update_test

import (
	"os"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/update"
)

// The testdata is what Pando 0.3.0's release actually published: the image's
// index digest and the cosign bundle attached to it on Docker Hub,
// checksums.txt and its bundle from the GitHub release, and Sigstore's trusted
// root at the time. Verified offline, so this tests the identities and formats
// against the real release workflow's output rather than against a fixture
// shaped like what someone expected it to be.
func trusted(t *testing.T) root.TrustedMaterial {
	t.Helper()
	raw, err := os.ReadFile("testdata/trusted_root.json")
	require.NoError(t, err)
	tr, err := root.NewTrustedRootFromJSON(raw)
	require.NoError(t, err)
	return tr
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return b
}

// TestR357_AReleasedImageVerifiesAndAnythingElseDoesNot asserts R-357.
func TestR357_AReleasedImageVerifiesAndAnythingElseDoesNot(t *testing.T) {
	tm := trusted(t)
	digest, err := v1.NewHash(strings.TrimSpace(string(read(t, "image-0.3.0.digest"))))
	require.NoError(t, err)
	sig := read(t, "image-0.3.0.sigstore.json")

	require.NoError(t, update.VerifyImageBundle(tm, sig, digest))

	other, err := v1.NewHash("sha256:" + strings.Repeat("0", 64))
	require.NoError(t, err)
	require.Error(t, update.VerifyImageBundle(tm, sig, other), "the signature is for one digest only")

	// The release workflow's signature is not the image workflow's.
	require.Error(t, update.VerifyImageBundle(tm, read(t, "checksums.txt.sigstore.json"), digest))

	require.Error(t, update.VerifyImageBundle(tm, []byte(`{}`), digest))
}

// TestR363_ARelease_sChecksumsVerifyAgainstTheReleaseWorkflow asserts the
// verification half of R-363.
func TestR363_AReleasesChecksumsVerifyAgainstTheReleaseWorkflow(t *testing.T) {
	tm := trusted(t)
	sums := read(t, "checksums.txt")
	sig := read(t, "checksums.txt.sigstore.json")

	require.NoError(t, update.VerifyChecksums(tm, sums, sig))

	tampered := append([]byte{}, sums...)
	tampered[0] ^= 1
	require.Error(t, update.VerifyChecksums(tm, tampered, sig), "one changed byte fails")

	// The image workflow's signature is not the release workflow's.
	require.Error(t, update.VerifyChecksums(tm, sums, read(t, "image-0.3.0.sigstore.json")))
}
