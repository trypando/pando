package source

import (
	"context"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// A key that does not parse is refused saying where to replace it, and the
// refusal carries nothing of the key (R-194).
func TestAnSSHKeyThatCannotBeReadIsRefusedWithoutQuotingIt(t *testing.T) {
	const key = "not-a-key-but-secret-material-all-the-same"
	_, err := authMethod(api.GitCredential{SSHPrivateKey: secret.New(key), KnownHosts: "x"}, "ssh://git@git.example.com/acme/api.git")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "SSH private key could not be read")
	require.NotEmpty(t, errs.As(err).Remedy)
	require.NotContains(t, err.Error(), "secret-material")
}

// Known hosts that are not known hosts are refused, with what to put there
// instead.
func TestKnownHostsThatCannotBeReadAreRefused(t *testing.T) {
	_, err := knownHosts("git.example.com this-is-not-a-key", "git.example.com:22")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "known hosts could not be read")
	require.Contains(t, errs.As(err).Remedy, "ssh-keyscan")

	_, err = knownHosts("  \n", "git.example.com:22")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "pins no host key")
}

// A token or password without a username is sent as git's, and no
// credential at all is anonymous.
func TestAPasswordWithoutAUsernameSignsInAsGit(t *testing.T) {
	auth, err := authMethod(api.GitCredential{Password: secret.New("t")}, "https://git.example.com/a/b.git")
	require.NoError(t, err)
	basic, ok := auth.(*githttp.BasicAuth)
	require.True(t, ok)
	require.Equal(t, "git", basic.Username)

	auth, err = authMethod(api.GitCredential{Username: "bob", Password: secret.New("t")}, "https://git.example.com/a/b.git")
	require.NoError(t, err)
	require.Equal(t, "bob", auth.(*githttp.BasicAuth).Username)

	auth, err = authMethod(api.GitCredential{}, "https://git.example.com/a/b.git")
	require.NoError(t, err)
	require.Nil(t, auth)
}

// The app a fetch is for travels in the context, so a connection's use is
// audited against it; a context that names none names none.
func TestTheAppAFetchIsForTravelsInTheContext(t *testing.T) {
	require.Empty(t, AppFrom(context.Background()))
	require.Equal(t, "app_01", AppFrom(ForApp(context.Background(), "app_01")))
}
