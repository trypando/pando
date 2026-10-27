package source

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// memUploads is an UploadStore in memory, shared the way one database is.
type memUploads struct {
	mu   sync.Mutex
	by   map[string][]byte
	fail error
	// gone is apps deleted: AddUpload answers NotFound for them.
	gone map[string]bool
	// reading is set while PutUpload reads, which is while a database
	// would hold a connection.
	reading chan struct{}
}

func newMemUploads() *memUploads {
	return &memUploads{by: map[string][]byte{}, gone: map[string]bool{}}
}

func (m *memUploads) PutUpload(_ context.Context, appID string, r io.Reader) error {
	if m.fail != nil {
		return m.fail
	}
	if m.reading != nil {
		close(m.reading)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.by[appID] = b
	return nil
}

func (m *memUploads) AddUpload(_ context.Context, appID string, r io.Reader) (bool, error) {
	if m.fail != nil {
		return false, m.fail
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gone[appID] {
		return false, errs.Newf(errs.NotFound, "The app %s no longer exists.", appID)
	}
	if _, ok := m.by[appID]; ok {
		return false, nil
	}
	m.by[appID] = b
	return true, nil
}

func (m *memUploads) OpenUpload(_ context.Context, appID string) (io.ReadCloser, bool, error) {
	if m.fail != nil {
		return nil, false, m.fail
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.by[appID]
	if !ok {
		return nil, false, nil
	}
	return io.NopCloser(bytes.NewReader(b)), true, nil
}

func (m *memUploads) HasUpload(_ context.Context, appID string) (bool, error) {
	if m.fail != nil {
		return false, m.fail
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.by[appID]
	return ok, nil
}

func (m *memUploads) DeleteUpload(_ context.Context, appID string) error {
	if m.fail != nil {
		return m.fail
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.by, appID)
	return nil
}

func fetchApp(t *testing.T, s Sources, appID string) (*Checkout, error) {
	t.Helper()
	co, err := s.Fetch(ForApp(context.Background(), appID), spec.Source{Type: spec.SourceUpload, UploadID: appID})
	if co != nil {
		t.Cleanup(co.Close)
	}
	return co, err
}

// TestR417_AnUploadIsReadOnAnotherHostWithNoSharedDirectory asserts R-417 with
// an in-memory store: the upload directory of the host that took the upload is
// never written, and the other host reads it.
func TestR417_AnUploadIsReadOnAnotherHostWithNoSharedDirectory(t *testing.T) {
	store := newMemUploads()
	one := Sources{Uploads: store, UploadDir: t.TempDir()}
	two := Sources{Uploads: store, UploadDir: t.TempDir()}

	require.NoError(t, one.StoreUpload(context.Background(), "app_01UP", archive(t, "index.js", "hi")))
	entries, err := os.ReadDir(one.UploadDir)
	require.NoError(t, err)
	require.Empty(t, entries, "nothing is written to the host's directory")

	co, err := fetchApp(t, two, "app_01UP")
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(co.Dir, "index.js"))
	require.NoError(t, err)
	require.Equal(t, "hi", string(body))

	require.NoError(t, two.DiscardUpload(context.Background(), "app_01UP"))
	_, err = fetchApp(t, one, "app_01UP")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "pando deploy")
}

// TestR417_AnOldUploadFileIsMovedIntoTheStoreWhenRead asserts R-417's
// upgrade path: a file from before uploads moved is read, moved, and removed.
func TestR417_AnOldUploadFileIsMovedIntoTheStoreWhenRead(t *testing.T) {
	store := newMemUploads()
	dir := t.TempDir()
	old := Sources{UploadDir: dir}
	require.NoError(t, old.StoreUpload(context.Background(), "app_01OLD", archive(t, "a.txt", "old")))

	s := Sources{Uploads: store, UploadDir: dir}
	co, err := fetchApp(t, s, "app_01OLD")
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(co.Dir, "a.txt"))
	require.NoFileExists(t, filepath.Join(dir, "app_01OLD.tar.gz"))
	has, err := store.HasUpload(context.Background(), "app_01OLD")
	require.NoError(t, err)
	require.True(t, has)
}

func TestR417_MigrateUploadsMovesEveryFileTheStoreLacks(t *testing.T) {
	store := newMemUploads()
	dir := t.TempDir()
	old := Sources{UploadDir: dir}
	for _, id := range []string{"app_01A", "app_01B"} {
		require.NoError(t, old.StoreUpload(context.Background(), id, archive(t, "f", id)))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600))
	require.NoError(t, store.PutUpload(context.Background(), "app_01B", bytes.NewReader([]byte("newer"))))

	s := Sources{Uploads: store, UploadDir: dir}
	moved, err := s.MigrateUploads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, moved, "app_01B's row is newer than its file")
	require.Equal(t, []byte("newer"), store.by["app_01B"])
	require.NoFileExists(t, filepath.Join(dir, "app_01B.tar.gz"))
	require.NoFileExists(t, filepath.Join(dir, "app_01A.tar.gz"))
	require.FileExists(t, filepath.Join(dir, "notes.txt"))

	none, err := Sources{Uploads: store, UploadDir: filepath.Join(dir, "missing")}.MigrateUploads(context.Background())
	require.NoError(t, err)
	require.Zero(t, none)
	none, err = Sources{UploadDir: dir}.MigrateUploads(context.Background())
	require.NoError(t, err)
	require.Zero(t, none)
}

// TestR417_AnUploadReachesTheStoreOnlyOnceItHasArrived asserts that the store,
// which holds a database connection while it reads, is not handed the body
// until the client has sent all of it.
func TestR417_AnUploadReachesTheStoreOnlyOnceItHasArrived(t *testing.T) {
	store := newMemUploads()
	store.reading = make(chan struct{})
	work := t.TempDir()
	s := Sources{Uploads: store, WorkDir: work}

	packed, err := io.ReadAll(archive(t, "index.js", "hi"))
	require.NoError(t, err)
	body, client := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- s.StoreUpload(context.Background(), "app_01SLOW", body) }()

	_, err = client.Write(packed[:len(packed)/2])
	require.NoError(t, err)
	select {
	case <-store.reading:
		t.Fatal("the store was reading while the client was still sending")
	case <-time.After(50 * time.Millisecond):
	}
	_, err = client.Write(packed[len(packed)/2:])
	require.NoError(t, err)
	require.NoError(t, client.Close())
	require.NoError(t, <-done)
	require.Equal(t, packed, store.by["app_01SLOW"])

	entries, err := os.ReadDir(work)
	require.NoError(t, err)
	require.Empty(t, entries, "the spooled body is removed")
}

// TestR417_ABodyThatFailsIsSaidAsItFailed asserts that a body refused part way
// — over the size limit, which the handler answers with its own message —
// reaches the caller as the body's error, and nothing is stored.
func TestR417_ABodyThatFailsIsSaidAsItFailed(t *testing.T) {
	store := newMemUploads()
	work := t.TempDir()
	s := Sources{Uploads: store, WorkDir: filepath.Join(work, "made")}
	over := errors.New("http: request body too large")

	err := s.StoreUpload(context.Background(), "app_01BIG", io.MultiReader(bytes.NewReader([]byte("abc")), iotestErr{over}))
	require.ErrorIs(t, err, over)
	require.Empty(t, store.by)
	entries, err := os.ReadDir(s.WorkDir)
	require.NoError(t, err)
	require.Empty(t, entries)

	blocked := filepath.Join(work, "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))
	err = Sources{Uploads: store, WorkDir: blocked}.StoreUpload(context.Background(), "app_01BIG", bytes.NewReader([]byte("x")))
	require.Equal(t, errs.Internal, errs.CodeOf(err), "a work directory that cannot be made is Pando's failure")

	if os.Geteuid() == 0 {
		return // root writes a read-only directory
	}
	readOnly := filepath.Join(work, "ro")
	require.NoError(t, os.Mkdir(readOnly, 0o500))
	err = Sources{Uploads: store, WorkDir: readOnly}.StoreUpload(context.Background(), "app_01BIG", bytes.NewReader([]byte("x")))
	require.Equal(t, errs.Internal, errs.CodeOf(err), "a work directory that cannot be written is Pando's failure")
}

type iotestErr struct{ err error }

func (e iotestErr) Read([]byte) (int, error) { return 0, e.err }

// TestR417_AnOldUploadFileForADeletedAppIsRemoved asserts that a file whose
// app is gone is removed, not kept to be tried again at every start.
func TestR417_AnOldUploadFileForADeletedAppIsRemoved(t *testing.T) {
	store := newMemUploads()
	store.gone["app_01GONE"] = true
	dir := t.TempDir()
	require.NoError(t, Sources{UploadDir: dir}.StoreUpload(context.Background(), "app_01GONE", archive(t, "f", "x")))

	moved, err := Sources{Uploads: store, UploadDir: dir}.MigrateUploads(context.Background())
	require.NoError(t, err)
	require.Zero(t, moved)
	require.NoFileExists(t, filepath.Join(dir, "app_01GONE.tar.gz"))
}

// TestR417_ReadingAnOldFileKeepsANewerUpload asserts the read path's move: an
// upload stored after the read found none, and before the file was moved, is
// the one read, and the file is removed.
func TestR417_ReadingAnOldFileKeepsANewerUpload(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Sources{UploadDir: dir}.StoreUpload(context.Background(), "app_01RACE", archive(t, "a.txt", "old")))
	store := &racingUploads{memUploads: newMemUploads(), newer: archive(t, "a.txt", "new")}

	co, err := fetchApp(t, Sources{Uploads: store, UploadDir: dir}, "app_01RACE")
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(co.Dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "new", string(body))
	require.NoFileExists(t, filepath.Join(dir, "app_01RACE.tar.gz"))
}

// racingUploads stores newer just before the first AddUpload, as an upload
// on another host would.
type racingUploads struct {
	*memUploads
	newer io.Reader
}

func (r *racingUploads) AddUpload(ctx context.Context, appID string, f io.Reader) (bool, error) {
	if r.newer != nil {
		if err := r.PutUpload(ctx, appID, r.newer); err != nil {
			return false, err
		}
		r.newer = nil
	}
	return r.memUploads.AddUpload(ctx, appID, f)
}

func TestUploadStoreFailuresAreReturned(t *testing.T) {
	store := newMemUploads()
	dir := t.TempDir()
	require.NoError(t, Sources{UploadDir: dir}.StoreUpload(context.Background(), "app_01F", archive(t, "f", "x")))
	store.fail = errs.New(errs.Internal, "the database is down")
	s := Sources{Uploads: store, UploadDir: dir}

	require.Error(t, s.StoreUpload(context.Background(), "app_01F", archive(t, "f", "x")))
	require.Error(t, s.DiscardUpload(context.Background(), "app_01F"))
	_, err := fetchApp(t, s, "app_01F")
	require.Error(t, err)
	_, err = s.MigrateUploads(context.Background())
	require.Error(t, err)
	_, err = s.migrateUpload(context.Background(), "app_01F")
	require.Error(t, err)
	require.FileExists(t, filepath.Join(dir, "app_01F.tar.gz"), "a file that could not be moved is kept")

	require.Equal(t, errs.ValidInvalid, errs.CodeOf(s.StoreUpload(context.Background(), "../x", nil)))
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(s.DiscardUpload(context.Background(), "../x")))
}

// A digest the store refuses at the end of the archive is said as the
// store said it.
func TestReadFailureKeepsTheStoresError(t *testing.T) {
	said := errs.New(errs.Internal, "does not match")
	require.Equal(t, said, readFailure(said))
	require.Equal(t, errs.Internal, errs.CodeOf(readFailure(errors.New("boom"))))
}

func TestAnUploadWithNoDirectoryNeedsOnlyTheStore(t *testing.T) {
	s := Sources{Uploads: newMemUploads()}
	require.NoError(t, s.StoreUpload(context.Background(), "app_01N", archive(t, "f", "x")))
	_, err := fetchApp(t, s, "app_01N")
	require.NoError(t, err)
	require.NoError(t, s.DiscardUpload(context.Background(), "app_01N"))
	_, err = fetchApp(t, s, "app_01N")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}
