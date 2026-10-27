package atomicfile_test

import (
	"os"
	"path/filepath"
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
