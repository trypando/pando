package spec_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR092_OnlyHTTPSHTTPAndSSHAddressesAreFetched asserts the schemes a
// repository address may use (2026-10-16 assessment, finding 3): https,
// http and ssh, git's git@host:path shorthand among them, and nothing that
// reads Pando's own disk or speaks the unauthenticated git:// protocol.
func TestR092_OnlyHTTPSHTTPAndSSHAddressesAreFetched(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/team/app.git",
		"http://gitea.internal:3000/team/app",
		"ssh://git@git.example.com/team/app.git",
		"ssh://git@git.example.com:2222/team/app.git",
		"git@github.com:team/app.git",
		"github.com:team/app.git",
		"  https://github.com/team/app.git  ",
	} {
		require.NoError(t, spec.CheckSourceURL(spec.Source{Type: spec.SourceGit, URL: ok}), ok)
	}
	for _, bad := range []string{
		"file:///etc/hostname",
		"/etc/hostname",
		"./app",
		"C:\\repos\\app",
		"C:/repos/app",
		"git://github.com/team/app.git",
		"ftp://example.com/app.git",
		"https:///no-host",
	} {
		err := spec.CheckSourceURL(spec.Source{Type: spec.SourceGit, URL: bad})
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), bad)
		require.Contains(t, err.Error(), "https, http or ssh", bad)
	}

	// Only a repository's address is checked.
	require.NoError(t, spec.CheckSourceURL(spec.Source{Type: spec.SourceImage, URL: "file:///x"}))
	require.NoError(t, spec.CheckSourceURL(spec.Source{Type: spec.SourceGit}))
}

// TestR190_ARepositoryAddressCarryingACredentialIsRefused asserts R-190 for a
// repository address: a token or password written into it would be kept in
// the spec's append-only history and every export of it, so it is refused,
// and the message points at source connections.
func TestR190_ARepositoryAddressCarryingACredentialIsRefused(t *testing.T) {
	for bad, secret := range map[string]string{
		"https://ghp_abc123@github.com/team/app.git":         "ghp_abc123",
		"https://user:hunter2@github.com/team/app.git":       "hunter2",
		"http://oauth2:tok_9z8y@gitea.internal/team/app.git": "tok_9z8y",
		"ssh://git:s3cr3t@git.example.com/team/app.git":      "s3cr3t",
	} {
		err := spec.CheckSourceURL(spec.Source{Type: spec.SourceGit, URL: bad})
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), bad)
		require.NotContains(t, err.Error()+errs.As(err).Remedy, secret, "the message does not repeat the credential")
		require.Contains(t, errs.As(err).Remedy, "source connection")
	}
}
