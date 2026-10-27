//go:build integration

package state_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// TestR417_AStalledUploadHoldsNoDatabaseConnection asserts that an upload
// still arriving holds no connection from the pool. The body used to be
// streamed into the database inside one transaction, so a client sending
// slowly held a connection for as long as it took, and a few dozen such
// uploads held every connection Pando has.
func TestR417_AStalledUploadHoldsNoDatabaseConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := seedApp(t, db, seedUser(t, db, "ada").ID)
	packed := uploadArchive(t, "index.js", []byte("hi"))

	host := source.Sources{Uploads: state.NewUploads(db), WorkDir: t.TempDir()}
	body, client := io.Pipe()
	// A failed assertion leaves the client stalled: ending it lets the
	// store return its connection before the database is closed.
	t.Cleanup(func() { _ = client.CloseWithError(io.ErrClosedPipe) })
	done := make(chan error, 1)
	go func() { done <- host.StoreUpload(ctx, appID, body) }()

	_, err := client.Write(packed[:len(packed)/2])
	require.NoError(t, err)
	// The body is half sent and the client has stopped: nothing may be
	// holding a connection for it.
	for range 20 {
		require.Zero(t, db.Stat().AcquiredConns(), "a stalled upload holds a database connection")
		time.Sleep(10 * time.Millisecond)
	}

	_, err = client.Write(packed[len(packed)/2:])
	require.NoError(t, err)
	require.NoError(t, client.Close())
	require.NoError(t, <-done)

	co, err := fetchUpload(t, source.Sources{Uploads: state.NewUploads(db)}, appID)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(co.Dir, "index.js"))
	entries, err := os.ReadDir(host.WorkDir)
	require.NoError(t, err)
	require.Empty(t, entries, "the spooled body is removed")
}

// TestR417_AnOldUploadFileNeverReplacesANewerUpload asserts that moving an
// archive file from before R-417 into the database only ever fills an app
// that has no upload there: it used to check, then replace, so an upload
// stored between the two was overwritten by the older file.
func TestR417_AnOldUploadFileNeverReplacesANewerUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := seedApp(t, db, seedUser(t, db, "ada").ID)
	uploads := state.NewUploads(db)

	dir := t.TempDir()
	require.NoError(t, source.Sources{UploadDir: dir}.StoreUpload(ctx, appID, bytes.NewReader(uploadArchive(t, "a.txt", []byte("old")))))
	require.NoError(t, uploads.PutUpload(ctx, appID, bytes.NewReader(uploadArchive(t, "a.txt", []byte("new")))))

	added, err := uploads.AddUpload(ctx, appID, bytes.NewReader(uploadArchive(t, "a.txt", []byte("older"))))
	require.NoError(t, err)
	require.False(t, added, "an app with an upload is not given another")

	host := source.Sources{Uploads: uploads, UploadDir: dir}
	moved, err := host.MigrateUploads(ctx)
	require.NoError(t, err)
	require.Zero(t, moved)
	require.NoFileExists(t, filepath.Join(dir, appID+".tar.gz"), "the older file is removed")

	co, err := fetchUpload(t, host, appID)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(co.Dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "new", string(body))
}

// TestR417_AnOldUploadFileForADeletedAppIsDiscarded asserts that an archive
// file whose app no longer exists is removed rather than refused by the
// foreign key and tried again at every start.
func TestR417_AnOldUploadFileForADeletedAppIsDiscarded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	gone := id.New(id.App)

	dir := t.TempDir()
	require.NoError(t, source.Sources{UploadDir: dir}.StoreUpload(ctx, gone, bytes.NewReader(uploadArchive(t, "a.txt", []byte("x")))))

	host := source.Sources{Uploads: state.NewUploads(db), UploadDir: dir}
	moved, err := host.MigrateUploads(ctx)
	require.NoError(t, err)
	require.Zero(t, moved)
	require.NoFileExists(t, filepath.Join(dir, gone+".tar.gz"))

	moved, err = host.MigrateUploads(ctx)
	require.NoError(t, err)
	require.Zero(t, moved)
}
