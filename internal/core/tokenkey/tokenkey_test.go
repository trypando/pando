package tokenkey_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/tokenkey"
	"github.com/trypando/pando/internal/errs"
)

// TestR063_TheTokenKeyIsCreatedOnceAndKeptPrivate asserts the key API tokens
// are stored under is generated on first use, 0600, and read back unchanged
// on every later start.
func TestR063_TheTokenKeyIsCreatedOnceAndKeptPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "token.key")

	first, err := tokenkey.LoadOrCreate(path)
	require.NoError(t, err)
	require.Len(t, first, tokenkey.Size)

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	again, err := tokenkey.LoadOrCreate(path)
	require.NoError(t, err)
	require.Equal(t, first, again)
}

// TestReplicasStartingTogetherEndWithOneTokenKey asserts that several
// replicas creating the key on one shared volume at once all end with the
// same key (issue #72).
func TestReplicasStartingTogetherEndWithOneTokenKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.key")

	const n = 16
	keys := make([][]byte, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := tokenkey.LoadOrCreate(path)
			require.NoError(t, err)
			keys[i] = k
		}()
	}
	wg.Wait()
	for _, k := range keys[1:] {
		require.Equal(t, keys[0], k)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary files are left beside the key")
}

func TestATokenKeyOfTheWrongSizeIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.key")
	require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))

	_, err := tokenkey.LoadOrCreate(path)
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, path)
	require.NotEmpty(t, e.Remedy)
}
