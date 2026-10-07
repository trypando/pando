package buildkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeBlob(t *testing.T, dir, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	name := hex.EncodeToString(sum[:])
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blobs", "sha256", name), []byte(content), 0o644))
	return name
}

// TestR224_ABuildCacheKeepsOnlyWhatItsIndexReaches asserts R-224. BuildKit's
// local cache export adds every build's layers and never removes the last
// build's, so an app's cache grew for as long as the app existed.
func TestR224_ABuildCacheKeepsOnlyWhatItsIndexReaches(t *testing.T) {
	dir := t.TempDir()

	layer := writeBlob(t, dir, "\x1f\x8b current layer")
	config := writeBlob(t, dir, fmt.Sprintf(`{"layers":[{"blob":"sha256:%s"}]}`, layer))
	manifest := writeBlob(t, dir, fmt.Sprintf(
		`{"manifests":[{"digest":"sha256:%s"},{"digest":"sha256:%s"}]}`, layer, config))
	stale := writeBlob(t, dir, "\x1f\x8b an earlier build's layer")
	staleManifest := writeBlob(t, dir, fmt.Sprintf(`{"manifests":[{"digest":"sha256:%s"}]}`, stale))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.json"),
		[]byte(fmt.Sprintf(`{"manifests":[{"digest":"sha256:%s"}]}`, manifest)), 0o644))

	removed, err := pruneCacheDir(dir)
	require.NoError(t, err)
	require.Equal(t, 2, removed)

	for _, kept := range []string{layer, config, manifest} {
		require.FileExists(t, filepath.Join(dir, "blobs", "sha256", kept))
	}
	for _, gone := range []string{stale, staleManifest} {
		require.NoFileExists(t, filepath.Join(dir, "blobs", "sha256", gone))
	}
}

// A cache Pando cannot read the index of is left alone rather than guessed at.
func TestAnUnreadableCacheIndexPrunesNothing(t *testing.T) {
	dir := t.TempDir()
	blob := writeBlob(t, dir, "\x1f\x8b a layer")

	removed, err := pruneCacheDir(dir)
	require.NoError(t, err, "no index yet: nothing to do")
	require.Zero(t, removed)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.json"), []byte("not json"), 0o644))
	removed, err = pruneCacheDir(dir)
	require.NoError(t, err)
	require.Zero(t, removed)
	require.FileExists(t, filepath.Join(dir, "blobs", "sha256", blob))
}

// TestR224_ADeletedAppsBuildCacheIsRemoved asserts R-224: a deleted app's
// cache stayed on disk forever (issue #55).
func TestR224_ADeletedAppsBuildCacheIsRemoved(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PANDO_BUILD_CACHE_DIR", root)

	for _, ns := range []string{"app_01GONE/web", "app_01GONE/worker", "app_01KEEP"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, ns, "blobs"), 0o755))
	}

	a := &Adapter{}
	require.NoError(t, a.Forget(context.Background(), "app_01GONE"))
	require.NoDirExists(t, filepath.Join(root, "app_01GONE"))
	require.DirExists(t, filepath.Join(root, "app_01KEEP"), "another app's cache is not touched")

	require.NoError(t, a.Forget(context.Background(), "app_01GONE"), "forgetting twice is not an error")

	for _, bad := range []string{"", "..", "../etc", `a\b`} {
		require.Error(t, a.Forget(context.Background(), bad), bad)
	}
	require.DirExists(t, root)
}

// TestR224_AppBuildCachesAreEvictedLeastRecentlyUsedFirst asserts R-224 for
// the build caches together: past their total, whole apps' caches go, the
// least recently used first, and a cache in use is never removed (issue #72).
func TestR224_AppBuildCachesAreEvictedLeastRecentlyUsedFirst(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	cache := func(app string, bytes int, lastUsed time.Time) {
		dir := filepath.Join(root, app, "web", "blobs", "sha256")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "layer"), make([]byte, bytes), 0o644))
		marker := filepath.Join(root, app, lastUsedMarker)
		require.NoError(t, os.WriteFile(marker, nil, 0o644))
		require.NoError(t, os.Chtimes(marker, lastUsed, lastUsed))
	}
	cache("app_oldest", 400, now.Add(-72*time.Hour))
	cache("app_older", 400, now.Add(-48*time.Hour))
	cache("app_recent", 400, now.Add(-2*time.Hour))
	cache("app_building", 400, now.Add(-time.Minute))

	removed, err := evictCaches(root, 1000, now)
	require.NoError(t, err)
	require.Equal(t, []string{"app_oldest", "app_older"}, removed,
		"oldest first, and only until the rest fit")
	require.DirExists(t, filepath.Join(root, "app_recent"))
	require.DirExists(t, filepath.Join(root, "app_building"))

	removed, err = evictCaches(root, 100, now)
	require.NoError(t, err)
	require.Equal(t, []string{"app_recent"}, removed)
	require.DirExists(t, filepath.Join(root, "app_building"),
		"a cache a build is using is left alone, even over the limit")

	removed, err = evictCaches(root, 1<<20, now)
	require.NoError(t, err)
	require.Empty(t, removed, "under the limit nothing goes")
}

// The marker a build touches is what orders eviction.
func TestR224_ABuildMarksItsAppsCacheAsUsed(t *testing.T) {
	t.Setenv("PANDO_BUILD_CACHE_DIR", t.TempDir())
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	touchCache("app_x", at)
	info, err := os.Stat(filepath.Join(cachePath("app_x"), lastUsedMarker))
	require.NoError(t, err)
	require.True(t, info.ModTime().Equal(at))

	later := at.Add(30 * time.Minute)
	touchCache("app_x", later)
	info, err = os.Stat(filepath.Join(cachePath("app_x"), lastUsedMarker))
	require.NoError(t, err)
	require.True(t, info.ModTime().Equal(later))
}
