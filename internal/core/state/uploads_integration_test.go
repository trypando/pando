//go:build integration

package state_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

func uploadArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}))
	_, err := tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func fetchUpload(t *testing.T, s source.Sources, appID string) (*source.Checkout, error) {
	t.Helper()
	co, err := s.Fetch(source.ForApp(context.Background(), appID), spec.Source{Type: spec.SourceUpload, UploadID: appID})
	if co != nil {
		t.Cleanup(co.Close)
	}
	return co, err
}

// TestR417_AnUploadWrittenOnOneReplicaIsReadOnAnother asserts R-417: two
// replicas, each with its own store on one database and its own directory,
// and an upload taken by one is deployed by the other. The archive is larger
// than one chunk, so it is read back across several.
func TestR417_AnUploadWrittenOnOneReplicaIsReadOnAnother(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := seedApp(t, db, seedUser(t, db, "ada").ID)

	big := make([]byte, 9<<20)
	_, err := rand.Read(big)
	require.NoError(t, err)
	packed := uploadArchive(t, "blob.bin", big)

	one := source.Sources{Uploads: state.NewUploads(db), UploadDir: t.TempDir()}
	two := source.Sources{Uploads: state.NewUploads(db), UploadDir: t.TempDir()}
	require.NoError(t, one.StoreUpload(ctx, appID, bytes.NewReader(packed)))

	entries, err := os.ReadDir(one.UploadDir)
	require.NoError(t, err)
	require.Empty(t, entries, "the replica that took the upload wrote nothing to its directory")

	co, err := fetchUpload(t, two, appID)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(co.Dir, "blob.bin"))
	require.NoError(t, err)
	require.Equal(t, big, got)

	// A second upload replaces the first whole.
	require.NoError(t, two.StoreUpload(ctx, appID, bytes.NewReader(uploadArchive(t, "small.txt", []byte("v2")))))
	co, err = fetchUpload(t, one, appID)
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(co.Dir, "blob.bin"))
	require.FileExists(t, filepath.Join(co.Dir, "small.txt"))

	counts, err := state.NewBundleSource(db).Counts(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, counts["uploads_in_database"], "the bundle's manifest counts what its dump carries (R-212)")

	require.NoError(t, one.DiscardUpload(ctx, appID))
	_, err = fetchUpload(t, two, appID)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}

// TestR417_AnOldUploadFileIsMovedIntoTheDatabaseOnFirstRead asserts that an
// install from before R-417 keeps working: its file is read, moved into the
// database, and from then on read by every replica.
func TestR417_AnOldUploadFileIsMovedIntoTheDatabaseOnFirstRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := seedApp(t, db, seedUser(t, db, "ada").ID)

	dir := t.TempDir()
	require.NoError(t, source.Sources{UploadDir: dir}.StoreUpload(ctx, appID, bytes.NewReader(uploadArchive(t, "a.txt", []byte("old")))))

	uploads := state.NewUploads(db)
	host := source.Sources{Uploads: uploads, UploadDir: dir}
	_, err := fetchUpload(t, host, appID)
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(dir, appID+".tar.gz"))

	other := source.Sources{Uploads: uploads, UploadDir: t.TempDir()}
	co, err := fetchUpload(t, other, appID)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(co.Dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "old", string(body))
}

// An upload whose stored bytes no longer match its digest is refused, not
// deployed.
func TestR417_AnUploadThatDoesNotMatchItsDigestIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := seedApp(t, db, seedUser(t, db, "ada").ID)
	uploads := state.NewUploads(db)
	require.NoError(t, uploads.PutUpload(ctx, appID, bytes.NewReader(uploadArchive(t, "a.txt", []byte("x")))))
	_, err := db.Exec(ctx, `UPDATE app_uploads SET sha256 = repeat('a', 64) WHERE app_id = $1`, appID)
	require.NoError(t, err)

	rc, ok, err := uploads.OpenUpload(ctx, appID)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = io.ReadAll(rc)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "digest")
	require.NoError(t, rc.Close())

	_, err = fetchUpload(t, source.Sources{Uploads: uploads}, appID)
	require.Error(t, err)

	has, err := uploads.HasUpload(ctx, appID)
	require.NoError(t, err)
	require.True(t, has)
	_, ok, err = uploads.OpenUpload(ctx, "app_01NONE")
	require.NoError(t, err)
	require.False(t, ok)
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

// What the store cannot do it says, and a body that fails mid-upload leaves
// the previous upload in place.
func TestR417_AnUploadStoreFailureIsSaid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := seedApp(t, db, seedUser(t, db, "ada").ID)
	uploads := state.NewUploads(db)
	require.NoError(t, uploads.PutUpload(ctx, appID, bytes.NewReader(uploadArchive(t, "a.txt", []byte("kept")))))

	require.ErrorIs(t, uploads.PutUpload(ctx, appID, brokenBody{}), io.ErrClosedPipe,
		"the body's own error, so a request over its limit is still told apart")
	co, err := fetchUpload(t, source.Sources{Uploads: uploads}, appID)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(co.Dir, "a.txt"))

	require.Error(t, uploads.PutUpload(ctx, "app_01NOSUCHAPP", bytes.NewReader([]byte("x"))), "an upload belongs to an app")

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, uploads.PutUpload(canceled, appID, bytes.NewReader(nil)))
	_, _, err = uploads.OpenUpload(canceled, appID)
	require.Error(t, err)
	_, err = uploads.HasUpload(canceled, appID)
	require.Error(t, err)
	require.Error(t, uploads.DeleteUpload(canceled, appID))
}
