package source

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// confineLinks keeps every symbolic link in a checkout pointing inside it
// (R-020: an app's source is confined to its checkout).
//
// go-git writes a link's target as the repository records it, so a link to
// "../../.." or to "/" points out of the checkout, and whatever later opens a
// path through it — a detector, a compose bind mount, a generated build file,
// the build context itself — follows it onto the host.
//
// A link that resolves outside the checkout is removed, and so is one that
// cannot be resolved and names a place outside it; an absolute link is outside
// unless its target is under the checkout, when it is rewritten as the
// relative link it means. Removed rather than refused, so the repository
// around it still deploys. Every other link is kept.
func confineLinks(dir string) error {
	links, err := linksIn(dir)
	if err != nil || len(links) == 0 {
		return err
	}
	for _, rel := range links {
		if err := rerootAbsolute(dir, rel); err != nil {
			return err
		}
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, rel := range links {
		if linkStaysInside(root, rel) {
			continue
		}
		if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// linksIn lists the symbolic links under dir, relative to it. Git's own
// directory is go-git's and holds none.
func linksIn(dir string) ([]string, error) {
	var links []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" && filepath.Dir(p) == dir {
			return filepath.SkipDir
		}
		if d.Type()&fs.ModeSymlink != 0 {
			rel, relErr := filepath.Rel(dir, p)
			if relErr != nil {
				return relErr
			}
			links = append(links, rel)
		}
		return nil
	})
	return links, err
}

// rerootAbsolute rewrites a link whose absolute target lies inside dir as the
// relative link to the same place.
func rerootAbsolute(dir, rel string) error {
	full := filepath.Join(dir, rel)
	target, err := os.Readlink(full)
	if err != nil || !filepath.IsAbs(target) {
		return err
	}
	inTree, err := filepath.Rel(dir, filepath.Clean(target))
	if err != nil || escapes(inTree) {
		// Outside the checkout: left for linkStaysInside to remove.
		return nil //nolint:nilerr // Rel failing means the target is not under dir.
	}
	relative, err := filepath.Rel(filepath.Dir(full), filepath.Join(dir, inTree))
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		return err
	}
	return os.Symlink(relative, full)
}

// linkStaysInside reports whether the link at rel resolves inside root. A
// link that resolves is checked by resolving it through root, which follows
// every link along the way and refuses one that leaves. A dangling link is
// kept only when the target it names is inside, so that creating that target
// later cannot turn it into a way out.
func linkStaysInside(root *os.Root, rel string) bool {
	_, err := root.Stat(rel)
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	target, err := root.Readlink(rel)
	if err != nil || filepath.IsAbs(target) {
		return false
	}
	return !escapes(filepath.Join(filepath.Dir(rel), target))
}

// escapes reports whether a cleaned relative path climbs above its base.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}
