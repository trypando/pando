package registrykit_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/imageregistry/registrykit"
)

// TestSavingARegistryWarnsWhatWillNotWorkAsExpected asserts the save-time
// warnings: Docker Hub keeps a deleted app's images and limits pulls, and an
// address on ghcr.io or Artifact Registry without the path those registries
// need says which part is missing. A complete address warns of nothing.
func TestSavingARegistryWarnsWhatWillNotWorkAsExpected(t *testing.T) {
	warn := func(settings map[string]any) []string {
		raw, err := json.Marshal(settings)
		require.NoError(t, err)
		return registrykit.Warnings(raw)
	}
	got := warn(map[string]any{"url": "docker.io/acme/pando"})
	require.Len(t, got, 1)
	require.Contains(t, got[0], "delete them on Docker Hub")
	require.Contains(t, got[0], "download limits")

	got = warn(map[string]any{"url": "https://ghcr.io"})
	require.Len(t, got, 1)
	require.Contains(t, got[0], "no owner on ghcr.io")
	require.Contains(t, got[0], "ghcr.io/acme")
	got = warn(map[string]any{"url": "ghcr.io/acme", "layout": "single"})
	require.Len(t, got, 1)
	require.Contains(t, got[0], "ghcr.io/acme/pando")
	require.Empty(t, warn(map[string]any{"url": "ghcr.io/acme"}))

	got = warn(map[string]any{"url": "us-east1-docker.pkg.dev/my-project"})
	require.Len(t, got, 1)
	require.Contains(t, got[0], "project and repository")
	got = warn(map[string]any{"url": "us-east1-docker.pkg.dev/p/r", "layout": "single"})
	require.Len(t, got, 1)
	require.Contains(t, got[0], "image name")
	require.Empty(t, warn(map[string]any{"url": "us-east1-docker.pkg.dev/p/r"}))

	require.Empty(t, warn(map[string]any{"url": "https://registry.internal:5000"}))
	require.Empty(t, registrykit.Warnings(nil))
	require.Empty(t, registrykit.Warnings(json.RawMessage(`not json`)))
}
