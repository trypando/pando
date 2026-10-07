package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAFetchWorksUnderTheConfiguredWorkDirAndFallsBackWhenItCannotBeMade
// asserts where a clone or an unpacked upload is made (issue #72): under
// server.work_dir, created if missing, and under the system temporary
// directory when that directory cannot be made rather than failing the fetch.
func TestAFetchWorksUnderTheConfiguredWorkDirAndFallsBackWhenItCannotBeMade(t *testing.T) {
	t.Parallel()

	work := filepath.Join(t.TempDir(), "not-yet", "work")
	dir, err := Sources{WorkDir: work}.tempDir("pando-src-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.Equal(t, work, filepath.Dir(dir), "made under the work dir, which was created for it")

	// A file where the directory should be: MkdirAll cannot make it.
	blocked := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))
	dir, err = Sources{WorkDir: filepath.Join(blocked, "work")}.tempDir("pando-src-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.True(t, strings.HasPrefix(dir, filepath.Clean(os.TempDir())), "fell back to %s, got %s", os.TempDir(), dir)
	require.DirExists(t, dir)
}
