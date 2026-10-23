package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR020_AnAbsoluteLinkIntoTheCheckoutIsKeptAsARelativeOne asserts R-020
// for a link whose absolute target is inside the checkout, as a checkout
// written with go-git's re-rooting filesystem has: it is kept, rewritten as
// the relative link it means, so it still resolves wherever the tree is read.
func TestR020_AnAbsoluteLinkIntoTheCheckoutIsKeptAsARelativeOne(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "app.js"), []byte("app"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(dir, "src", "app.js"), filepath.Join(dir, "src", "deep", "abs")))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.Symlink("/", filepath.Join(dir, ".git", "ignored")))

	require.NoError(t, confineLinks(dir))

	target, err := os.Readlink(filepath.Join(dir, "src", "deep", "abs"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join("..", "app.js"), target)
	_, err = os.Lstat(filepath.Join(dir, ".git", "ignored"))
	require.NoError(t, err, "git's own directory is go-git's and is not walked")
}

func TestConfineLinksReportsACheckoutItCannotRead(t *testing.T) {
	require.Error(t, confineLinks(filepath.Join(t.TempDir(), "gone")))
}

func TestConfineLinksLeavesATreeWithoutLinksAlone(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o644))
	require.NoError(t, confineLinks(dir))
	require.FileExists(t, filepath.Join(dir, "a"))
}
