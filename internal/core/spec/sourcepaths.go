package spec

import (
	"path"
	"path/filepath"
	"strings"
)

// ConfineStoredPaths rewrites, in a spec read back from storage, a
// subdirectory or build context that is absolute or climbs above the
// repository as the place inside the repository a build was always given for
// it (R-020).
//
// Before validation refused such paths, the source view and the builder each
// read them rooted where they started: an absolute path as one under the
// directory, and ".." as no higher than it. A revision written then deployed
// from there, and rewriting it the same way keeps it deploying — on a
// redeploy, a rollback or a pin — instead of failing the stricter validation
// that new specs meet. A path inside the repository is left exactly as it is.
// A Dockerfile is not rewritten: one that named a place outside the
// repository never built from the repository.
func ConfineStoredPaths(s *AppSpec) {
	if s == nil {
		return
	}
	if escapesRepository("", s.Source.Subdir) {
		s.Source.Subdir = rooted(s.Source.Subdir)
	}
	subdir := rooted(s.Source.Subdir)
	if escapesRepository(subdir, s.Build.Context) {
		s.Build.Context = rooted(s.Build.Context)
	}
	for i, w := range s.Workloads {
		if w.Build == nil || !escapesRepository(subdir, w.Build.Context) {
			continue
		}
		b := *w.Build
		b.Context = rooted(b.Context)
		s.Workloads[i].Build = &b
	}
}

// escapesRepository reports whether value, relative to base, is absolute or
// climbs above the repository. An empty value is the base itself.
func escapesRepository(base, value string) bool {
	if value == "" {
		return false
	}
	slashed := filepath.ToSlash(value)
	if path.IsAbs(slashed) || filepath.IsAbs(value) {
		return true
	}
	joined := path.Clean(path.Join(base, slashed))
	return joined == ".." || strings.HasPrefix(joined, "../")
}

// rooted is value read as a path that cannot climb past its start, without
// its leading slash: "../x" and "/x" are both "x", and "..", "/" and "." are
// "".
func rooted(value string) string {
	return strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(value)), "/")
}
