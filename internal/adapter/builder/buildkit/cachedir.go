package buildkit

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/trypando/pando/internal/errs"
)

// Forget removes a deleted app's build cache.
//
// Every build exports its cache to a directory on Pando's own disk, one per
// workload under the app's ID, and nothing removed it: a deleted app's cache
// stayed forever, hundreds of megabytes for an ordinary Python app. The GC
// calls this when it tears the app down.
func (a *Adapter) Forget(_ context.Context, namespace string) error {
	app := bundleOf(namespace)
	if app == "" || strings.ContainsAny(app, `/\`) || app == "." || app == ".." {
		return errs.Newf(errs.ValidInvalid, "%q does not name an app's build cache.", namespace)
	}
	if err := os.RemoveAll(cachePath(app)); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not remove the app's build cache.", err)
	}
	return nil
}

// digestRef finds every digest a manifest or cache config mentions.
var digestRef = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

// maxManifestBytes bounds how much of a blob is read to look for references.
// Manifests and BuildKit's cache config are kilobytes; layers are gzip, and
// are never parsed.
const maxManifestBytes = 16 << 20

// pruneCacheDir removes blobs that nothing in the cache refers to any more.
//
// BuildKit's local cache export adds the layers of each build and rewrites
// index.json to point at them, and never removes what the previous build
// wrote. An app's cache grew with every build it ever had. What index.json
// reaches — directly, or through a manifest or cache config it names — is kept;
// the rest is an earlier build's.
//
// Conservative by construction: anything a JSON blob mentions is kept, and a
// cache whose index cannot be read is left alone rather than guessed at.
func pruneCacheDir(dir string) (removed int, err error) {
	index, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	blobs := filepath.Join(dir, "blobs", "sha256")
	keep := map[string]bool{}
	queue := digestRef.FindAllString(string(index), -1)
	if len(queue) == 0 {
		return 0, nil
	}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		hex := strings.TrimPrefix(d, "sha256:")
		if keep[hex] {
			continue
		}
		keep[hex] = true
		for _, ref := range referencesIn(filepath.Join(blobs, hex)) {
			if !keep[strings.TrimPrefix(ref, "sha256:")] {
				queue = append(queue, ref)
			}
		}
	}

	entries, err := os.ReadDir(blobs)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) != 64 || keep[name] {
			continue
		}
		if os.Remove(filepath.Join(blobs, name)) == nil {
			removed++
		}
	}
	return removed, nil
}

// referencesIn returns the digests a JSON blob mentions. Anything that is not
// JSON — a layer — refers to nothing.
func referencesIn(path string) []string {
	// The name is a hex digest digestRef matched, joined under the cache's own
	// blobs directory, so it cannot leave it.
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, 1)
	if _, err := io.ReadFull(f, head); err != nil || head[0] != '{' {
		return nil
	}
	rest, err := io.ReadAll(io.LimitReader(f, maxManifestBytes))
	if err != nil {
		return nil
	}
	return digestRef.FindAllString(string(head)+string(rest), -1)
}

// defaultLocalCacheMaxBytes is how much disk every app's build cache under
// Pando's data directory may use together. [P] (issue #72)
//
// One app's cache is pruned to what its last build reaches (pruneCacheDir),
// but there was no total: at a few hundred megabytes an app, a thousand apps
// is hundreds of gigabytes on the disk Pando and its database share. 20 GiB
// keeps the caches of the apps that are being built and lets the rest go,
// least recently used first. An evicted app's next build starts cold and
// takes longer; nothing else changes.
const defaultLocalCacheMaxBytes int64 = 20 << 30

// cacheInUseFor is how recently a cache must have been used for eviction to
// leave it alone, whatever its size. Longer than a build's default timeout
// (R-119), so no build still running — on this replica or another sharing the
// directory, which this process cannot see — has its cache removed under it.
const cacheInUseFor = 30 * time.Minute

// lastUsedMarker is touched at the start of every build of an app. Its time
// is when the app's cache was last used; the directory's own changes whenever
// anything inside it does, including a prune.
const lastUsedMarker = ".last-used"

// cacheRoot is the directory every app's build cache is under.
func cacheRoot() string {
	if root := os.Getenv("PANDO_BUILD_CACHE_DIR"); root != "" {
		return root
	}
	return "/var/lib/pando/buildcache"
}

// touchCache records that an app's cache is being used now.
func touchCache(app string, now time.Time) {
	dir := cachePath(app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	marker := filepath.Join(dir, lastUsedMarker)
	if err := os.Chtimes(marker, now, now); err != nil {
		_ = os.WriteFile(marker, nil, 0o644) //nolint:gosec // an empty marker file
		_ = os.Chtimes(marker, now, now)
	}
}

// localCacheLimit is the configured total, or the default. Negative leaves the
// caches unbounded.
func (a *Adapter) localCacheLimit() int64 {
	if a.config.LocalCacheMaxBytes == 0 {
		return defaultLocalCacheMaxBytes
	}
	return a.config.LocalCacheMaxBytes
}

// trimLocalCaches evicts whole apps' build caches, least recently used first,
// until they fit under the configured total. Failures are ignored, as for the
// build service's own cache: too much cache is the state before this existed.
func (a *Adapter) trimLocalCaches() {
	limit := a.localCacheLimit()
	if limit < 0 {
		return
	}
	a.localCacheMu.Lock()
	defer a.localCacheMu.Unlock()
	_, _ = evictCaches(cacheRoot(), limit, time.Now())
}

// cacheEntry is one app's cache directory as eviction sees it.
type cacheEntry struct {
	app      string
	bytes    int64
	lastUsed time.Time
}

// evictCaches removes app caches under root, least recently used first, until
// what is left totals limit or less, and returns the apps it removed.
//
// A cache used within cacheInUseFor is never removed, so the total may stay
// over the limit while many apps build at once; it comes down after.
func evictCaches(root string, limit int64, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var caches []cacheEntry
	var total int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		c := cacheEntry{app: e.Name(), bytes: dirBytes(dir)}
		if info, err := os.Stat(filepath.Join(dir, lastUsedMarker)); err == nil {
			c.lastUsed = info.ModTime()
		} else if info, err := e.Info(); err == nil {
			c.lastUsed = info.ModTime()
		}
		caches = append(caches, c)
		total += c.bytes
	}
	if total <= limit {
		return nil, nil
	}

	sort.Slice(caches, func(i, j int) bool { return caches[i].lastUsed.Before(caches[j].lastUsed) })
	var removed []string
	for _, c := range caches {
		if total <= limit {
			break
		}
		if now.Sub(c.lastUsed) < cacheInUseFor {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, c.app)); err != nil {
			continue
		}
		total -= c.bytes
		removed = append(removed, c.app)
	}
	return removed, nil
}

// dirBytes is the total size of the regular files under dir.
func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry counts as nothing
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}
