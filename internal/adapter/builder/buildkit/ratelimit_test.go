package buildkit

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// TestR105_ABaseImageRefusedForTheDownloadLimitIsSaidAsThat asserts R-105 for
// a build: a base image Docker Hub refused for its pull limit names the image,
// Docker Hub and a fix that works for a build, where an app credential does
// not, since base images are pulled without one.
func TestR105_ABaseImageRefusedForTheDownloadLimitIsSaidAsThat(t *testing.T) {
	err := solveFailed(errors.New(`failed to solve: node:20: failed to resolve source metadata for docker.io/library/node:20: ` +
		`failed to copy: httpReadSeeker: failed open: unexpected status code ` +
		`https://registry-1.docker.io/v2/library/node/manifests/sha256:abc: 429 Too Many Requests - ` +
		`Server message: toomanyrequests: You have reached your unauthenticated pull rate limit.`))
	e := errs.As(err)
	require.NotNil(t, e)
	require.Equal(t, errs.AdapterRegistryRateLimited, e.Code)
	require.Contains(t, e.Message, "Docker Hub is limiting how many images this server may download")
	require.Contains(t, e.Message, "the base image docker.io/library/node:20 that the build starts from")
	require.Contains(t, e.Remedy, "FROM line")
	require.NotContains(t, e.Remedy, "Add a registry credential")

	other := errs.As(solveFailed(errors.New("process \"/bin/sh -c npm ci\" did not complete successfully: exit code: 1")))
	require.Equal(t, errs.BuildFailed, other.Code)
}
