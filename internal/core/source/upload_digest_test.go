package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

// archive is a gzipped tar holding one file.
func archive(t *testing.T, name, body string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}))
	_, err := tw.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return &buf
}

// TestR312_AnUploadIsKnownByItsArchive asserts the half of R-312 that concerns
// uploads: an upload has no commit, so the archive's digest is what tells a
// redeploy of the same upload from a new one (issue #84). Without it, every
// deploy of an uploaded app scanned again.
func TestR312_AnUploadIsKnownByItsArchive(t *testing.T) {
	s := Sources{UploadDir: t.TempDir()}
	src := spec.Source{Type: spec.SourceUpload, UploadID: "app_01UP"}
	fetch := func() *Checkout {
		t.Helper()
		co, err := s.Fetch(ForApp(context.Background(), "app_01UP"), src)
		require.NoError(t, err)
		t.Cleanup(co.Close)
		return co
	}

	_, err := s.StoreUpload("app_01UP", archive(t, "index.js", "console.log(1)"))
	require.NoError(t, err)
	first := fetch()
	require.Empty(t, first.Commit, "an upload has no commit, and none is invented (R-120)")
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, first.Digest)
	require.Equal(t, first.Digest, first.Identity())

	require.Equal(t, first.Digest, fetch().Digest, "the same archive, deployed again, is the same source")

	_, err = s.StoreUpload("app_01UP", archive(t, "index.js", "console.log(2)"))
	require.NoError(t, err)
	require.NotEqual(t, first.Digest, fetch().Digest, "a new upload is a new source")
}

// TestR120_ACommitNamesACheckoutBeforeAnyDigest asserts that a checkout with a
// commit is known by it: the digest is only for sources that have none.
func TestR120_ACommitNamesACheckoutBeforeAnyDigest(t *testing.T) {
	require.Equal(t, "3f9a2c1d", (&Checkout{Commit: "3f9a2c1d", Digest: "sha256:ab"}).Identity())
	require.Empty(t, (&Checkout{}).Identity())
}
