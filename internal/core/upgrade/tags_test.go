package upgrade_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/upgrade"
)

// TestR355_OnlyAMovingTagThatCoversTheTargetAllowsAnInPlaceUpgrade asserts the
// tag half of R-355.
func TestR355_OnlyAMovingTagThatCoversTheTargetAllowsAnInPlaceUpgrade(t *testing.T) {
	ok := []struct{ ref, to, tag string }{
		{"trypando/pando:latest", "0.4.0", "trypando/pando:latest"},
		{"trypando/pando", "0.4.0", "trypando/pando:latest"},
		{"docker.io/trypando/pando:latest", "1.2.0", "trypando/pando:latest"},
		{"index.docker.io/trypando/pando:0.3", "0.3.2", "trypando/pando:0.3"},
		{"trypando/pando:1", "1.5.0", "trypando/pando:1"},
	}
	for _, c := range ok {
		tag, reason := upgrade.MovingTag(c.ref, c.to)
		require.Empty(t, reason, c.ref)
		require.Equal(t, c.tag, tag, c.ref)
	}

	refused := []struct{ ref, to, says string }{
		{"trypando/pando:0.3.1", "0.3.2", "an exact version"},
		{"trypando/pando@sha256:abcd", "0.4.0", "pinned to a digest"},
		{"trypando/pando:0.3", "0.4.0", "follows 0.3's patch releases only"},
		{"trypando/pando:1", "2.0.0", "follows 1.x only"},
		{"trypando/pando:latest", "0.5.0-rc.1", "release candidate"},
		{"ghcr.io/someone/pando:latest", "0.4.0", "not the published image"},
		{"pando-dev:local", "0.4.0", "not the published image"},
	}
	for _, c := range refused {
		tag, reason := upgrade.MovingTag(c.ref, c.to)
		require.Empty(t, tag, c.ref)
		require.Contains(t, reason, c.says, c.ref)
		require.Contains(t, reason, "where Pando is deployed", "every refusal says what to change, and where: %s", c.ref)
	}
}
