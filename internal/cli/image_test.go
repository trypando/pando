package cli_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// An image app is created from the CLI with its credential, the secret half
// read from the terminal rather than taken as a flag (R-261, issue #41).
func TestAnImageAppIsAddedWithItsRegistryCredential(t *testing.T) {
	api := newAPI(t).reply("POST /apps", map[string]any{"id": "app_01HQ8"})

	got := run(t, api, "ghp_token\n", "app", "add", "--image", "ghcr.io/acme/web:1.4", "--registry-username", "ben")
	require.NoError(t, got.err, got.errOut)

	body := api.bodyFor("POST /apps")
	require.JSONEq(t, `{"name":"web","source":{"type":"image","image":"ghcr.io/acme/web:1.4",
		"credential":{"kind":"basic","username":"ben","password":"ghp_token"}}}`, body)
	require.NotContains(t, got.out, "ghp_token")
}

func TestAnECRCredentialIsSetFromTheCLI(t *testing.T) {
	api := newAPI(t)

	got := run(t, api, "aws-secret\n", "app", "registry-credential", "set", "app_01HQ8",
		"--ecr-access-key-id", "AKIAEXAMPLE", "--ecr-region", "eu-west-1")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"kind":"ecr","access_key_id":"AKIAEXAMPLE","secret_access_key":"aws-secret","region":"eu-west-1"}`,
		api.bodyFor("PUT /apps/app_01HQ8/registry-credential"))
}

func TestAddNeedsARepositoryOrAnImageButNotBoth(t *testing.T) {
	api := newAPI(t)
	require.Error(t, run(t, api, "", "app", "add").err)
	require.Error(t, run(t, api, "", "app", "add", "https://github.com/acme/web", "--image", "nginx").err)
	require.Error(t, run(t, api, "", "app", "registry-credential", "set", "app_1").err)
}
