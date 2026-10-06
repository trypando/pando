package oci_test

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/secret"
)

// memStore is an app's sealed credential fields, in memory.
type memStore map[string]map[string]secret.Value

func (m memStore) Put(_ context.Context, app, field string, v secret.Value) error {
	if m[app] == nil {
		m[app] = map[string]secret.Value{}
	}
	m[app][field] = v
	return nil
}

func (m memStore) Delete(_ context.Context, app, field string) error {
	delete(m[app], field)
	return nil
}

func (m memStore) Resolve(_ context.Context, app string) (map[string]secret.Value, error) {
	return m[app], nil
}

// dockerLogin writes a config.json as `docker login` would and points
// DOCKER_CONFIG at it.
func dockerLogin(t *testing.T, registry, user, pass string) {
	t.Helper()
	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"auths":{"`+registry+`":{"auth":"`+auth+`"}}}`), 0o600))
	t.Setenv("DOCKER_CONFIG", dir)
}

// With apps.docker_credentials on, an app with no credential of its own pulls
// with the Docker login on the Pando server (issue #41).
func TestAnAppWithNoCredentialPullsWithTheServersDockerLogin(t *testing.T) {
	dockerLogin(t, "ghcr.io", "operator", "ghp_server-login")
	im := &oci.Images{Credentials: memStore{}, Docker: oci.DockerLogin()}

	auth, err := im.Auth(context.Background(), "app_1", "ghcr.io/acme/private:1")
	require.NoError(t, err)
	require.NotNil(t, auth)
	require.Equal(t, "operator", auth.Username)
	require.Equal(t, "ghp_server-login", auth.Password.Reveal())

	// A registry the login has no entry for is anonymous, not an error.
	auth, err = im.Auth(context.Background(), "app_1", "quay.io/acme/web:1")
	require.NoError(t, err)
	require.Nil(t, auth)
}

// The app's own credential is the one used, whatever the server is signed in
// to: it is the one somebody chose for this app.
func TestTheAppsOwnCredentialBeatsTheServersDockerLogin(t *testing.T) {
	dockerLogin(t, "ghcr.io", "operator", "ghp_server-login")
	store := memStore{}
	im := &oci.Images{Credentials: store, Docker: oci.DockerLogin()}
	require.NoError(t, im.SaveCredential(context.Background(), "app_1",
		oci.Credential{Kind: oci.CredentialBasic, Username: "ben", Password: secret.New("ghp_app")}))

	auth, err := im.Auth(context.Background(), "app_1", "ghcr.io/acme/private:1")
	require.NoError(t, err)
	require.Equal(t, "ben", auth.Username)
}

// Off by default: an install that has not chosen it lends no app the server's
// login.
func TestTheServersDockerLoginIsNotUsedUnlessTheInstallChoseIt(t *testing.T) {
	dockerLogin(t, "ghcr.io", "operator", "ghp_server-login")
	im := &oci.Images{Credentials: memStore{}}

	auth, err := im.Auth(context.Background(), "app_1", "ghcr.io/acme/private:1")
	require.NoError(t, err)
	require.Nil(t, auth)
}

// Replacing a credential leaves nothing of the previous kind behind.
func TestSavingACredentialReplacesTheWholeOfThePreviousOne(t *testing.T) {
	store := memStore{}
	im := &oci.Images{Credentials: store}
	ctx := context.Background()
	require.NoError(t, im.SaveCredential(ctx, "app_1",
		oci.Credential{Kind: oci.CredentialBasic, Username: "ben", Password: secret.New("ghp_app")}))
	require.NoError(t, im.SaveCredential(ctx, "app_1",
		oci.Credential{Kind: oci.CredentialECR, AccessKeyID: "AKIA", SecretAccessKey: secret.New("s")}))

	require.NotContains(t, store["app_1"], "password")
	summary, ok, err := im.DescribeCredential(ctx, "app_1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, oci.CredentialECR, summary.Kind)
	require.Empty(t, summary.Username)

	require.NoError(t, im.RemoveCredential(ctx, "app_1"))
	_, ok, err = im.DescribeCredential(ctx, "app_1")
	require.NoError(t, err)
	require.False(t, ok)
}
