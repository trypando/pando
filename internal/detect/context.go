package detect

import (
	"path"
	"strings"

	"github.com/trypando/pando/internal/core/spec"
)

// liftBuildContexts moves the directory an app is built from up to the one its
// builds reach, when a compose file builds from above it: `context: ../..`
// beside a compose file in docker/standard/, naming docker/standard/Dockerfile
// within that context.
//
// The app's directory is the root of everything its build can read, so a
// context above it was cut back to it, and the build failed on "Could not
// read the app's Dockerfile" (issue #90). The compose file names the
// repository root as its context; that is what the build is given, and each
// context is rewritten to name the same place from there. Nothing reaches
// above the repository: a path cannot climb past its root, and a context that
// tries — `..` from the repository's root, or an absolute path — is written as
// the place inside the repository the build has always been given, rather than
// left for validation to refuse (R-020).
func liftBuildContexts(src spec.Source, draft Draft) (spec.Source, Draft) {
	base := path.Clean("/" + src.Subdir)
	root, outside := reachOf(src.Subdir, draft)
	if root == base && !outside {
		return src, draft
	}

	rebase := func(p string) string {
		rel := strings.TrimPrefix(strings.TrimPrefix(path.Join(base, p), root), "/")
		if rel == "" {
			return "."
		}
		return rel
	}
	out := draft
	if out.Build.Context != "" {
		out.Build.Context = rebase(out.Build.Context)
	}
	if out.Build.ComposeFile != "" {
		out.Build.ComposeFile = rebase(out.Build.ComposeFile)
	}
	out.Workloads = append([]spec.Workload(nil), draft.Workloads...)
	for i, w := range out.Workloads {
		if w.Build == nil {
			continue
		}
		b := *w.Build
		b.Context = rebase(b.Context)
		out.Workloads[i].Build = &b
	}
	src.Subdir = strings.TrimPrefix(root, "/")
	return src, out
}

// within reports whether dir is root or below it. Both are clean and absolute.
func within(dir, root string) bool {
	return root == "/" || dir == root || strings.HasPrefix(dir, root+"/")
}

// leavesRepository reports whether a build context, relative to subdir, is
// absolute or climbs above the repository.
func leavesRepository(subdir, context string) bool {
	if path.IsAbs(context) {
		return true
	}
	rel := path.Join(strings.TrimPrefix(path.Clean("/"+subdir), "/"), context)
	return rel == ".." || strings.HasPrefix(rel, "../")
}

// reachOf is the directory, clean and absolute from the repository's root,
// that every build context in draft lies within, and whether any of them
// leaves the repository.
func reachOf(subdir string, draft Draft) (root string, outside bool) {
	base := path.Clean("/" + subdir)
	root = base
	contexts := []string{draft.Build.Context}
	for _, w := range draft.Workloads {
		if w.Build != nil {
			contexts = append(contexts, w.Build.Context)
		}
	}
	for _, context := range contexts {
		if context == "" {
			continue
		}
		outside = outside || leavesRepository(subdir, context)
		dir := path.Join(base, context)
		for !within(dir, root) {
			root = path.Dir(root)
		}
	}
	return root, outside
}
