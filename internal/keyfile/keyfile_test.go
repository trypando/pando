package keyfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR190_AKeyIsWrittenWholeAndPrivate asserts R-190: a key is on disk, whole
// and 0600, by the time Create returns, with no temporary file left beside it.
func TestR190_AKeyIsWrittenWholeAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.key")
	key := bytes.Repeat([]byte{7}, 32)

	require.NoError(t, Create(path, key))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, key, got)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the temporary file is removed")
}

func TestAnExistingKeyIsNeverReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.key")
	require.NoError(t, os.WriteFile(path, []byte("first"), 0o600))

	err := Create(path, []byte("second"))
	require.True(t, errors.Is(err, os.ErrExist), "got %v", err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("first"), got)
}

func TestAMissingDirectoryIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "secrets.key")
	require.Error(t, Create(path, []byte("key")))
}

func TestSyncingAMissingDirectoryIsReported(t *testing.T) {
	require.Error(t, syncDir(filepath.Join(t.TempDir(), "absent")))
}

// TestR415_AJoinReplacesAKeyWholeAndPrivate asserts R-415: a host joining a
// cluster takes the cluster's key in place of its own, 0600, with no
// temporary file left beside it.
func TestR415_AJoinReplacesAKeyWholeAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.key")
	require.NoError(t, os.WriteFile(path, []byte("first"), 0o644))

	require.NoError(t, Replace(path, []byte("second")))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("second"), got)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	require.Error(t, Replace(filepath.Join(dir, "absent", "k"), []byte("x")))
}
