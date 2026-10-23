package source_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
)

// linkedRepo builds a real git repository whose commit holds files and the
// symbolic links named, each link committed with its target as written.
func linkedRepo(t *testing.T, files, links map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := r.Worktree()
	require.NoError(t, err)
	for name, body := range files {
		full := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
		_, err := wt.Add(name)
		require.NoError(t, err)
	}
	for name, target := range links {
		full := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.Symlink(target, full))
		_, err := wt.Add(name)
		require.NoError(t, err)
	}
	_, err = wt.Commit("links", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "t@example", When: time.Unix(1700000000, 0)},
	})
	require.NoError(t, err)
	return dir
}

// escape is a relative target deep enough to climb out of any temporary
// directory, to the filesystem root.
const escape = "../../../../../../../../../../../.."

// TestR020_ACheckoutKeepsNoLinkThatLeadsOutOfIt asserts R-020: a cloned
// repository's symbolic links that resolve outside the checkout are removed
// straight after the clone, and the ones that stay inside keep working.
func TestR020_ACheckoutKeepsNoLinkThatLeadsOutOfIt(t *testing.T) {
	dir := linkedRepo(t,
		map[string]string{"src/app.js": "app", "docker/Dockerfile.debian": "FROM scratch\n"},
		map[string]string{
			"out":         escape,                        // relative, out
			"etc-passwd":  escape + "/etc/passwd",        // relative, to a host file
			"nowhere":     "../pando-no-such-place",      // dangling, out
			"sub/up":      "..",                          // the checkout's root, in
			"trick":       "sub/up/..",                   // in as text, out once sub/up is followed
			"in":          "src",                         // an in-tree directory
			"Dockerfile":  "docker/Dockerfile.debian",    // an in-tree file
			"missing":     "not-yet-there",               // dangling, in
			"abs":         "/etc/passwd",                 // absolute: go-git writes it as it is
			"src/license": "../docker/Dockerfile.debian", // in, through ..
			"venv/skill":  "../.venv/lib/skill",          // dangling, in, through ..
		})

	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir})
	require.NoError(t, err)
	defer co.Close()

	for _, gone := range []string{"out", "etc-passwd", "nowhere", "trick", "abs"} {
		_, err := os.Lstat(filepath.Join(co.Dir, gone))
		require.True(t, os.IsNotExist(err), "%s leads out of the checkout and is removed", gone)
	}
	for _, kept := range []string{"sub/up", "in", "Dockerfile", "missing", "src/license", "venv/skill"} {
		info, err := os.Lstat(filepath.Join(co.Dir, kept))
		require.NoError(t, err, "%s stays inside the checkout and is kept", kept)
		require.NotZero(t, info.Mode()&os.ModeSymlink, "%s is still a link", kept)
	}

	view := co.View("")
	for name, want := range map[string]string{"in/app.js": "app", "Dockerfile": "FROM scratch\n", "src/license": "FROM scratch\n"} {
		f, err := view.Open(name)
		require.NoError(t, err, name)
		body := make([]byte, 64)
		n, _ := f.Read(body)
		_ = f.Close()
		require.Equal(t, want, string(body[:n]), name)
	}
	_, err = view.Open("out/etc/passwd")
	require.Error(t, err)
	_, err = view.Open("etc-passwd")
	require.Error(t, err)
}

// TestR020_APinnedCommitIsConfinedToo asserts R-020 on the path that checks
// out a pinned commit after the clone (R-120).
func TestR020_APinnedCommitIsConfinedToo(t *testing.T) {
	dir := linkedRepo(t, map[string]string{"a": "a"}, map[string]string{"out": escape})
	r, err := git.PlainOpen(dir)
	require.NoError(t, err)
	head, err := r.Head()
	require.NoError(t, err)

	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir, Commit: head.Hash().String()})
	require.NoError(t, err)
	defer co.Close()
	_, err = os.Lstat(filepath.Join(co.Dir, "out"))
	require.True(t, os.IsNotExist(err))
}

// TestR020_TheViewDoesNotFollowALinkOutOfTheSource asserts R-020 for a
// checkout whose links were never confined — the view itself refuses to
// follow one out, whatever put it there.
func TestR020_TheViewDoesNotFollowALinkOutOfTheSource(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("s3cret"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "compose.yml"), []byte("x"), 0o600))

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), []byte("hi"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "ctx")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "env")))

	view := (&source.Checkout{Dir: dir}).View("")
	_, err := view.Open("ctx/secret")
	require.Error(t, err)
	_, err = view.Open("env")
	require.Error(t, err)
	_, err = view.Stat("ctx/secret")
	require.Error(t, err)
	matches, err := view.Glob("compose.yml")
	require.NoError(t, err)
	require.Empty(t, matches, "a glob does not walk through a link out of the source")

	sub := (&source.Checkout{Dir: dir}).View("ctx")
	require.Equal(t, dir, sub.(interface{ CheckoutRoot() string }).CheckoutRoot(),
		"a builder checks what it reads against the checkout, not only the subdirectory")
	_, err = sub.Open("secret")
	require.Error(t, err, "a subdirectory that is a link out of the source is not the source")
	_, err = sub.Stat(".")
	require.Error(t, err)
	matches, err = sub.Glob("*")
	require.NoError(t, err)
	require.Empty(t, matches)

	f, err := view.Open("README")
	require.NoError(t, err, "a file in the source still reads")
	_ = f.Close()
}
