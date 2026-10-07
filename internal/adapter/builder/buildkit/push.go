package buildkit

import (
	"context"
	"strings"

	"github.com/docker/cli/cli/config/types"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Pushing a build to the install's registry (issue #72, PR 5).
//
// The credential reaches buildkitd through the client session's auth provider
// for the duration of this one build: buildkitd asks the session for a
// registry's credential when it pushes, and the session answers from memory.
// It is never written to buildkitd's configuration or to disk (R-194).

// digestKey is where the image exporter reports the pushed manifest's digest.
const digestKey = "containerimage.digest"

// pushExport is the image exporter set to push the build to target.
func pushExport(target *api.PushTarget) bkclient.ExportEntry {
	attrs := map[string]string{
		"name": target.Repository + ":" + strings.ToLower(target.Tag),
		"push": "true",
	}
	if target.Insecure {
		// Plain HTTP, only when the operator said so (O-35).
		attrs["registry.insecure"] = "true"
	}
	return bkclient.ExportEntry{Type: bkclient.ExporterImage, Attrs: attrs}
}

// pushAuth answers buildkitd's credential requests for the push target's
// registry, and for no other host.
func pushAuth(target *api.PushTarget) session.Attachable {
	host := registryHost(target.Repository)
	return authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		AuthConfigProvider: func(_ context.Context, asked string, _ []string, _ authprovider.ExpireCachedAuthCheck) (types.AuthConfig, error) {
			return credentialFor(target.Auth, host, asked), nil
		},
	})
}

// credentialFor is the credential for a host buildkitd asked about: the push
// target's, when the host is the target's registry, and none otherwise. A base
// image on another registry is pulled anonymously, as it is without a push.
func credentialFor(auth *api.RegistryAuth, host, asked string) types.AuthConfig {
	if auth == nil || !strings.EqualFold(asked, host) {
		return types.AuthConfig{}
	}
	return types.AuthConfig{
		Username:      auth.Username,
		Password:      auth.Password.Reveal(),
		IdentityToken: auth.IdentityToken.Reveal(),
		ServerAddress: host,
	}
}

// registryHost is the registry part of a repository: everything before the
// first slash.
func registryHost(repository string) string {
	host, _, _ := strings.Cut(repository, "/")
	return host
}

// pushedResult reads the pushed manifest's digest out of the solve's response.
// A push that reports no digest is a failure: what runs is repository@digest,
// and without one there is nothing to pin (R-120).
func pushedResult(target *api.PushTarget, resp *bkclient.SolveResponse) (api.BuildResult, error) {
	var digest string
	if resp != nil {
		digest = resp.ExporterResponse[digestKey]
	}
	if !strings.HasPrefix(digest, "sha256:") {
		return api.BuildResult{}, errs.Newf(errs.BuildFailed,
			"The build finished and was pushed to %s, and the registry reported no digest for it.",
			registryHost(target.Repository)).
			WithRemedy("Check that the registry accepts pushes from the build service, then deploy again.")
	}
	return api.BuildResult{ImageRef: target.Repository + "@" + digest, Digest: digest}, nil
}
