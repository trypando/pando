package atomicfile_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/atomicfile"
)

// TestAFileIsWrittenWholeAndOnlyWhenItChanges asserts the two properties a
// watched directory needs: the content lands whole, with the mode asked, and
// an unchanged file is not touched.
func TestAFileIsWrittenWholeAndOnlyWhenItChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "a.yml")
	require.NoError(t, atomicfile.WriteIfChanged(path, []byte("one"), 0o600))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "one", string(got))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
	require.NoError(t, atomicfile.WriteIfChanged(path, []byte("one"), 0o600))
	info, _ = os.Stat(path)
	require.WithinDuration(t, old, info.ModTime(), time.Second, "unchanged is left alone")

	require.NoError(t, atomicfile.WriteIfChanged(path, []byte("two"), 0o600))
	got, _ = os.ReadFile(path)
	require.Equal(t, "two", string(got))
	_, err = os.Stat(path + ".tmp")
	require.True(t, os.IsNotExist(err), "no temporary file is left")
}

// TestR417_ConcurrentWritersNeverLeaveAPartialFile asserts that writers of one
// path at once — route sync, the reconciler and a deploy each write an app's
// route — each land whole. With one fixed temporary name, one writer
// truncated the file another was about to rename into place, so a watcher
// read half a route, and a rename found its file already moved.
func TestR417_ConcurrentWritersNeverLeaveAPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yml")
	const size = 1 << 20
	bodies := make([][]byte, 4)
	for i := range bodies {
		bodies[i] = bytes.Repeat([]byte{byte('a' + i)}, size)
	}

	stop := make(chan struct{})
	var bad atomic.Int32
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if len(got) != size || !bytes.Equal(got, bytes.Repeat(got[:1], size)) {
				bad.Add(1)
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, 4*20)
	for i := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range 20 {
				// Alternating, so no write is skipped as unchanged.
				errs <- atomicfile.WriteIfChanged(path, bodies[(i+n)%len(bodies)], 0o644)
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-readerDone
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Zero(t, bad.Load(), "a reader saw a partial file")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file is left")
}

// TestAFileThatCannotBeWrittenIsAnError asserts each failure is returned:
// a directory that cannot be made, one that cannot be written, and a path
// that is a directory.
func TestAFileThatCannotBeWrittenIsAnError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	require.Error(t, atomicfile.WriteIfChanged(filepath.Join(file, "a"), []byte("x"), 0o644))

	ro := filepath.Join(dir, "ro")
	require.NoError(t, os.Mkdir(ro, 0o500))
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	require.Error(t, atomicfile.WriteIfChanged(filepath.Join(ro, "a"), []byte("x"), 0o644))

	target := filepath.Join(dir, "target")
	require.NoError(t, os.MkdirAll(filepath.Join(target, "child"), 0o755))
	require.Error(t, atomicfile.WriteIfChanged(target, []byte("x"), 0o644), "a directory is not replaced")
	_, err := os.Stat(target + ".tmp")
	require.True(t, os.IsNotExist(err))
}
