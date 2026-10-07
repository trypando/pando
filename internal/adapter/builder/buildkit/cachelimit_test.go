package buildkit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// oldCache writes an app's cache of the given size, last used long enough ago
// that eviction may take it, with or without the marker a build touches.
func oldCache(t *testing.T, root, app string, bytes int, marker bool) {
	t.Helper()
	dir := filepath.Join(root, app, "web", "blobs", "sha256")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "layer"), make([]byte, bytes), 0o644))
	old := time.Now().Add(-48 * time.Hour)
	if marker {
		m := filepath.Join(root, app, lastUsedMarker)
		require.NoError(t, os.WriteFile(m, nil, 0o644))
		require.NoError(t, os.Chtimes(m, old, old))
	}
	require.NoError(t, os.Chtimes(filepath.Join(root, app), old, old))
}

// TestR224_TheBuildCacheTotalIsConfigurable asserts R-224's total: the
// default when unset, the configured one otherwise, and none at all when it
// is negative.
func TestR224_TheBuildCacheTotalIsConfigurable(t *testing.T) {
	require.Equal(t, defaultLocalCacheMaxBytes, (&Adapter{}).localCacheLimit())
	require.Equal(t, int64(500), (&Adapter{config: Config{LocalCacheMaxBytes: 500}}).localCacheLimit())

	root := t.TempDir()
	t.Setenv("PANDO_BUILD_CACHE_DIR", root)
	oldCache(t, root, "app_a", 400, true)
	oldCache(t, root, "app_b", 400, true)

	(&Adapter{config: Config{LocalCacheMaxBytes: -1}}).trimLocalCaches()
	require.DirExists(t, filepath.Join(root, "app_a"), "a negative total leaves the caches unbounded")
	require.DirExists(t, filepath.Join(root, "app_b"))

	(&Adapter{config: Config{LocalCacheMaxBytes: 500}}).trimLocalCaches()
	left := 0
	for _, app := range []string{"app_a", "app_b"} {
		if _, err := os.Stat(filepath.Join(root, app)); err == nil {
			left++
		}
	}
	require.Equal(t, 1, left, "one cache goes to bring the total under 500 bytes")
}

// A cache from before builds left a marker is ordered by its directory's own
// time, and anything at the root that is not an app's directory is ignored.
func TestR224_ACacheWithoutAMarkerIsAgedByItsDirectory(t *testing.T) {
	root := t.TempDir()
	oldCache(t, root, "app_unmarked", 400, false)
	oldCache(t, root, "app_marked", 400, true)
	recent := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(root, "app_marked", lastUsedMarker), recent, recent))
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray-file"), make([]byte, 4096), 0o644))

	removed, err := evictCaches(root, 500, time.Now())
	require.NoError(t, err)
	require.Equal(t, []string{"app_unmarked"}, removed, "the older one goes; a stray file is neither counted nor removed")
	require.FileExists(t, filepath.Join(root, "stray-file"))
}

func TestEvictingCachesThatAreNotThere(t *testing.T) {
	removed, err := evictCaches(filepath.Join(t.TempDir(), "never-built"), 0, time.Now())
	require.NoError(t, err, "no build yet, so nothing to evict")
	require.Empty(t, removed)

	file := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	_, err = evictCaches(file, 0, time.Now())
	require.Error(t, err, "a root that cannot be read is reported")
}

// A cache directory that cannot be made is not a reason to fail a build.
func TestTouchingACacheThatCannotBeMadeDoesNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(root, nil, 0o644))
	t.Setenv("PANDO_BUILD_CACHE_DIR", root)
	touchCache("app_x", time.Now())
	_, err := os.Stat(filepath.Join(root, "app_x"))
	require.Error(t, err)
}
