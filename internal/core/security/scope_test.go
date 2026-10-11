package security_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/state"
)

// TestR311_AScanSaysWhatItRead asserts the scope the API reports beside a
// score (R-311): a number covers only what its scan read, and a scan of the
// source alone taken before there was a revision is not the app's score.
func TestR311_AScanSaysWhatItRead(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)

	detected := state.Scan{Commit: "3f9a2c1"}
	require.Equal(t, security.ScopeSource, security.ScopeOf(detected))
	require.True(t, security.AwaitsImage(detected), "detection's scan of a repository Pando builds")

	compose := state.Scan{SpecID: "spec_1", Commit: "3f9a2c1"}
	require.Equal(t, security.ScopeSource, security.ScopeOf(compose))
	require.False(t, security.AwaitsImage(compose), "a revision scored by the source alone says so, and keeps its score")

	published := state.Scan{Commit: "3f9a2c1", Image: "ghcr.io/acme/web@" + digest}
	require.Equal(t, security.ScopeImageAndSource, security.ScopeOf(published))
	require.False(t, security.AwaitsImage(published), "a plan that scanned the published image is scored")

	imageApp := state.Scan{Commit: digest, Image: "ghcr.io/acme/web@" + digest}
	require.Equal(t, security.ScopeImage, security.ScopeOf(imageApp))

	scanNow := state.Scan{SpecID: "spec_1", Image: digest}
	require.Equal(t, security.ScopeImage, security.ScopeOf(scanNow))
}
