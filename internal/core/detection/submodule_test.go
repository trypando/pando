package detection

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// TestR107_ABuildThatNeedsAnUnfetchedSubmoduleSaysSo asserts R-107 held to
// R-105, under the decision on O-60 (2026-10-13): submodules are not fetched,
// and a build that fails reading one fails saying which, and why it is empty
// (issue #90). A submodule the log does not name, or one that has its files,
// changes nothing.
func TestR107_ABuildThatNeedsAnUnfetchedSubmoduleSaysSo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte(
		"[submodule \"realworld\"]\n\tpath = realworld\n\turl = https://example.com/realworld.git\n"+
			"[submodule \"docs/theme\"]\n\tpath = docs/theme\n\turl = https://example.com/theme.git\n"+
			"[submodule \"vendor/lib\"]\n\tpath = vendor/lib\n\turl = https://example.com/lib.git\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "realworld"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "vendor", "lib"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vendor", "lib", "x.c"), []byte(""), 0o644))

	log := "✘ [ERROR] Could not resolve \"realworld/assets/theme/styles.css\"\n    angular:styles/global:styles:1:8:\n vendor/lib/x.c\n"
	require.Equal(t, []string{"realworld"}, missingSubmodules(dir, "", log),
		"docs/theme is not in the log, and vendor/lib has its files")
	require.Empty(t, missingSubmodules(dir, "", "npm ERR! missing script: build\n"))
	require.Empty(t, missingSubmodules(t.TempDir(), "", log), "no .gitmodules, nothing to say")
	require.Equal(t, []string{"docs/theme"}, missingSubmodules(dir, "docs", "cannot open theme/layouts/index.html\n"),
		"the log names paths from the app's directory")

	e := errs.As(submoduleFailure([]string{"realworld"}, log))
	require.Equal(t, errs.BuildFailed, e.Code)
	require.Equal(t, "The build of this app failed reading realworld/, which is a git submodule. Pando builds only the "+
		"files in the repository itself and does not fetch submodules, so in the build it is empty.", e.Message)
	require.Contains(t, e.Remedy, "Commit the submodule's files into the repository")
	require.Equal(t, log, e.Details["build_log"])
}
