package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// failingReader fails part-way, as a bundle cut short does.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("unexpected end of bundle") }

// TestR212_AnUploadThatCannotBeRestoredFailsTheRestoreAndLeavesNoHalfFile
// asserts that a restore which cannot write an uploaded source back says so
// with an error a person can read, and never leaves a half-written archive
// where the next deploy would build from it (O-37).
func TestR212_AnUploadThatCannotBeRestoredFailsTheRestoreAndLeavesNoHalfFile(t *testing.T) {
	t.Parallel()
	const name = "app_01HQ8AAAAAAAAAAAAAAAAAAAAA.tar.gz"
	entry := UploadsPrefix + name

	// The upload directory cannot be made: a file is in the way.
	blocked := filepath.Join(t.TempDir(), "uploads")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))
	err := (&Service{UploadDir: blocked}).restoreUpload(entry, strings.NewReader("source"))
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Equal(t, "Pando could not restore the uploaded source.", errs.As(err).Message)

	// The bundle ends part-way through the archive.
	dir := t.TempDir()
	err = (&Service{UploadDir: dir}).restoreUpload(entry, failingReader{})
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	leftovers, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, leftovers, "no half-written archive, and no .partial")

	// Something that is not a file is where the archive goes.
	dir = t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o700))
	err = (&Service{UploadDir: dir}).restoreUpload(entry, strings.NewReader("source"))
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	_, err = os.Stat(filepath.Join(dir, name+".partial"))
	require.True(t, os.IsNotExist(err), "the partial file is removed")

	// A directory Pando may not write in.
	dir = t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err = (&Service{UploadDir: dir}).restoreUpload(entry, strings.NewReader("source"))
	if os.Geteuid() != 0 { // root writes anywhere
		require.Equal(t, errs.Internal, errs.CodeOf(err))
	}
}

// TestR215_ARestoreStopsAtAnUploadItCannotPutBack asserts that a bundle whose
// uploaded source names a path Pando never writes is refused by the restore
// itself, not only by the helper, and nothing is written outside the directory.
func TestR215_ARestoreStopsAtAnUploadItCannotPutBack(t *testing.T) {
	s, _ := restoring(t)
	s.UploadDir = filepath.Join(t.TempDir(), "uploads")
	storeBundle(t, s, "dr_bad_upload", 1,
		bundleEntry{AdaptersName, "[]"},
		bundleEntry{UploadsPrefix + "../escaped.tar.gz", "not where it says"},
	)
	got, err := s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_bad_upload",
		Passphrase: passphrase, Confirm: true})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "which is not one Pando writes")
	require.Zero(t, got.UploadsApplied)
	_, err = os.Stat(filepath.Join(filepath.Dir(s.UploadDir), "escaped.tar.gz"))
	require.True(t, os.IsNotExist(err), "nothing was written where the entry pointed")
}

// TestR212_UploadsThatCannotBeReadFailTheBundleRatherThanBeingLeftOut asserts
// that a DR bundle is not written without an uploaded source it should hold:
// for an uploaded app, the archive is the only thing it can be rebuilt from.
func TestR212_UploadsThatCannotBeReadFailTheBundleRatherThanBeingLeftOut(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	notADir := filepath.Join(t.TempDir(), "uploads")
	require.NoError(t, os.WriteFile(notADir, nil, 0o600))
	_, err := (&Service{UploadDir: notADir}).addUploads(NewWriter(&bytes.Buffer{}, "dr_bundle", "test", 1))
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Equal(t, "Pando could not list the uploaded source for the backup.", errs.As(err).Message)

	dir := t.TempDir()
	unreadable := filepath.Join(dir, "app_01HQ8AAAAAAAAAAAAAAAAAAAAA.tar.gz")
	require.NoError(t, os.WriteFile(unreadable, []byte("source"), 0o000))
	_, err = (&Service{UploadDir: dir}).addUploads(NewWriter(&bytes.Buffer{}, "dr_bundle", "test", 1))
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "app_01HQ8AAAAAAAAAAAAAAAAAAAAA.tar.gz")
}
