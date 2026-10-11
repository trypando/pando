package buildkit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A site with a build script, no framework Pando knows and nothing that starts
// a server is served from the directory its committed copy is in (issue #172).
func TestASiteBuiltIntoACommittedDistIsServedFromIt(t *testing.T) {
	root := withFiles(t, map[string]string{
		"package.json":    `{"scripts":{"build":"gulp build"},"devDependencies":{"gulp":"^5"}}`,
		"dist/index.html": "<html></html>",
	})
	site, ok := readStaticSiteBuild(root)
	require.True(t, ok)
	require.Equal(t, "dist", site.OutDir)

	_, ok = readStaticSiteBuild(withFiles(t, map[string]string{
		"package.json": `{"scripts":{"build":"gulp build"}}`,
	}))
	require.False(t, ok, "without a committed copy there is nothing saying where the build writes")
}
