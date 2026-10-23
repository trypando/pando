package buildkit

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// msgPrepareBuild is the message for a build input Pando could not write.
const msgPrepareBuild = "Could not prepare the build."

// msgPreparePlan is the message for a build plan Pando could not set up.
const msgPreparePlan = "Could not prepare the build plan."

// msgReadSource is the message for an app's source Pando could not read.
const msgReadSource = "Could not read the app's source."

// Everything this builder writes into a checkout goes through an os.Root at
// the build context, which refuses to follow a symbolic link out of it
// (R-020). A name is checked as text before it gets here; the root is what
// stops a link in the checkout from carrying the write somewhere else.

// writeContextFile writes body to name, a path relative to dir, making the
// directories it needs.
func writeContextFile(dir, name string, body []byte) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if parent := filepath.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	// G306: 0644 is deliberate. Every file written here is a build input in
	// the throwaway checkout, read by the rootless builder as a different
	// user. None holds a secret — a secret reaches a build as a build secret,
	// never as a file in the context.
	return root.WriteFile(name, body, 0o644) //nolint:gosec
}

// mkdirContext makes the directory name, relative to dir, and its parents.
func mkdirContext(dir, name string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.MkdirAll(name, 0o755)
}

// readContextFile reads name, relative to dir, without following a link out
// of it.
func readContextFile(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(name)
}

// statContext reports whether name, relative to dir, exists inside it.
func statContext(dir, name string) bool {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	_, err = root.Stat(name)
	return err == nil
}

// confined refuses a path a build would read from that resolves, through any
// symbolic link, outside the checkout (R-020). A path that does not exist yet
// is left for the build to report as missing.
func confined(checkout, p, what string) error {
	if outside(filepath.Clean(checkout), filepath.Clean(p)) {
		return outsideSource(what)
	}
	base, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return errs.Wrap(errs.BuildFailed, msgReadSource, err)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errs.Wrap(errs.BuildFailed, msgReadSource, err)
	}
	if outside(base, resolved) {
		return outsideSource(what)
	}
	return nil
}

// outside reports whether p is not base or below it.
func outside(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// outsideSource is the error for a build input outside the app's source.
func outsideSource(what string) error {
	return errs.Newf(errs.BuildFailed,
		"The %s for this build is outside the app's source.", what).
		WithRemedy("Build from a directory inside the repository, and name a Dockerfile inside it, by a path that stays inside the repository and through no symbolic link that leads out of it.")
}

// removeContextFile removes name, relative to dir, if it is there. A link is
// removed itself, not followed.
func removeContextFile(dir, name string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
