package registryprobe_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect/registryprobe"
)

// The owner/name convention is the whole basis of tier 1, so what it does and
// does not recognize matters more than the network calls that follow it.
func TestWhichSourceURLsHaveARegistryConvention(t *testing.T) {
	p := registryprobe.New()

	for name, src := range map[string]spec.Source{
		"no URL at all":       {Type: spec.SourceGit},
		"not a known host":    {Type: spec.SourceGit, URL: "https://git.internal.example/acme/notes"},
		"no owner":            {Type: spec.SourceGit, URL: "https://github.com/notes"},
		"a project inside it": {Type: spec.SourceGit, URL: "https://github.com/acme/monorepo", Subdir: "apps/web"},
	} {
		t.Run(name, func(t *testing.T) {
			found, err := p.Published(context.Background(), src)
			require.NoError(t, err, "an unrecognized URL is not an error, it is a skipped tier")
			require.Empty(t, found)
		})
	}
}

// A subdir is the case worth its own test: the repository may well publish an
// image, and that image is not the project one directory down.
func TestASubdirectoryIsNotTheRepositorysImage(t *testing.T) {
	found, err := registryprobe.New().Published(context.Background(), spec.Source{
		Type:   spec.SourceGit,
		URL:    "https://github.com/vercel/turbo",
		Subdir: "examples/with-docker/apps/web",
	})
	require.NoError(t, err)
	require.Empty(t, found,
		"vercel/turbo may publish an image; it is not the image for a project inside it")
}

// ghcr.io and Docker Hub are not the same kind of evidence, and the default
// reflects that rather than treating "tier 1" as one thing.
func TestDockerHubIsOffByDefault(t *testing.T) {
	require.False(t, registryprobe.New().IncludeDockerHub,
		"a Docker Hub username has no relationship to a GitHub owner of the same name, "+
			"so a match there is not evidence that this is the project's own image")
}
