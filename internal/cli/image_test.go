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

func TestTheRegistryCredentialIsShownAndRemovedFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("GET /apps/app_1/registry-credential",
		map[string]any{"set": true, "credential": map[string]any{"kind": "basic", "username": "ben"}})

	got := run(t, api, "", "app", "registry-credential", "show", "app_1")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, `"username": "ben"`)

	got = run(t, api, "", "app", "registry-credential", "remove", "app_1")
	require.NoError(t, got.err, got.errOut)
	require.True(t, api.sawPath("/apps/app_1/registry-credential"))
	require.Contains(t, got.out, "Removed")

	got = run(t, api, "tok\n", "app", "registry-credential", "set", "app_1", "--registry-username", "ben")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{"kind":"basic","username":"ben","password":"tok"}`, api.bodyFor("PUT /apps/app_1/registry-credential"))
}

func TestAnImageAppIsNamedAfterItsImage(t *testing.T) {
	for ref, name := range map[string]string{
		"ghcr.io/acme/web:1.4": "web",
		"nginx":                "nginx",
		"localhost:5000/acme/api@sha256:" + "a1b2": "api",
	} {
		api := newAPI(t).reply("POST /apps", map[string]any{"id": "app_1"})
		got := run(t, api, "", "app", "add", "--image", ref)
		require.NoError(t, got.err, got.errOut)
		require.Contains(t, api.bodyFor("POST /apps"), `"name":"`+name+`"`, ref)
	}
}

func TestCredentialFlagsThatContradictEachOtherAreRefused(t *testing.T) {
	api := newAPI(t)
	both := run(t, api, "", "app", "add", "--image", "nginx", "--registry-username", "ben", "--ecr-access-key-id", "AKIA")
	require.ErrorContains(t, both.err, "not both")
	region := run(t, api, "", "app", "add", "--image", "nginx", "--ecr-region", "eu-west-1")
	require.ErrorContains(t, region.err, "--ecr-region needs --ecr-access-key-id")
	require.Empty(t, api.calls, "nothing is sent")
}

func TestAddNeedsARepositoryOrAnImageButNotBoth(t *testing.T) {
	api := newAPI(t)
	require.Error(t, run(t, api, "", "app", "add").err)
	require.Error(t, run(t, api, "", "app", "add", "https://github.com/acme/web", "--image", "nginx").err)
	require.Error(t, run(t, api, "", "app", "registry-credential", "set", "app_1").err)
}
