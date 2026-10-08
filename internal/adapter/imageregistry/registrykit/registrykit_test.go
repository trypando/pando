package registrykit_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/imageregistry/registrykit"
	"github.com/trypando/pando/internal/errs"
)

// TestR254_ARepositoryPerAppIsRefusedWhereTheRegistryCannotCreateOne asserts
// that the layout follows the provider's capability: a registry that does not
// create repositories on push defaults to one repository, and refuses a
// repository per app when the adapter is configured — not at the first push,
// after a build has run.
func TestR254_ARepositoryPerAppIsRefusedWhereTheRegistryCannotCreateOne(t *testing.T) {
	_, err := registrykit.Open(registrykit.Settings{URL: "registry.internal/pando", Layout: "per_app"}, false, nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "cannot hold a repository per app")

	r, err := registrykit.Open(registrykit.Settings{URL: "registry.internal/pando"}, false, nil)
	require.NoError(t, err)
	require.False(t, r.ImageRegistryCapabilities().RepositoryPerApp, "one repository by default")

	r, err = registrykit.Open(registrykit.Settings{URL: "registry.internal/pando"}, true, nil)
	require.NoError(t, err)
	require.True(t, r.ImageRegistryCapabilities().RepositoryPerApp, "a repository per app where pushes create them")
}
