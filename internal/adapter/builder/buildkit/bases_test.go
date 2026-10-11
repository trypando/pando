package buildkit

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBasesAreReadFromTheirDockerfile(t *testing.T) {
	require.Equal(t, map[string]string{"a": "nginx:1", "b": "debian:x"}, parseBases(
		"# FROM ignored:1 AS comment\nFROM nginx:1 AS a\n\nfrom debian:x as b\nFROM scratch\nRUN true\n"))

	// Every stage the builder asks for is there.
	require.Equal(t, bases["static-server"], staticServerImage)
	require.Equal(t, bases["program-runtime"], programRuntimeImage)

	require.PanicsWithValue(t, "buildkit: bases/Dockerfile has no stage missing", func() { base("missing") })
}

var nginxLine = regexp.MustCompile(`^nginx:1\.(\d+)-alpine$`)

// TestBaseImagesStayOnMaintainedLines refuses a base that is not rebuilt for
// security fixes. A Dependabot pull request proposing nginx's mainline (an odd
// minor) fails here rather than merging; moving to the next stable line passes.
func TestBaseImagesStayOnMaintainedLines(t *testing.T) {
	m := nginxLine.FindStringSubmatch(staticServerImage)
	require.NotNil(t, m, "the static server is nginx on alpine, pinned to a minor: %s", staticServerImage)
	minor, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	require.Zero(t, minor%2, "nginx 1.%d is a mainline release; pin a stable (even) minor", minor)
	require.GreaterOrEqual(t, minor, 30, "nginx 1.%d is older than the stable line this was written on", minor)

	// Debian releases that are past regular security support. Dependabot cannot
	// move a codename tag, so this only stops a step back.
	for _, retired := range []string{"buster", "bullseye", "bookworm"} {
		require.False(t, strings.Contains(programRuntimeImage, retired),
			"%s is past Debian's regular security support: %s", retired, programRuntimeImage)
	}
}
