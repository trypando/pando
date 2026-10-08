package ecr_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/imageregistry/ecr"
	"github.com/trypando/pando/internal/errs"
)

const repository = "123456789012.dkr.ecr.us-east-1.amazonaws.com/pando"

// fakeECR answers GetAuthorizationToken with a new password each call.
func fakeECR(t *testing.T) (endpoint string, calls *atomic.Int32) {
	t.Helper()
	calls = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		require.Equal(t, "AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken", r.Header.Get("X-Amz-Target"))
		require.Contains(t, r.Header.Get("Authorization"), "AKIAEXAMPLE/", "signed with the registry's key")
		token := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("AWS:minted-%d", n)))
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]any{"authorizationData": []map[string]any{{"authorizationToken": token}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, calls
}

func open(t *testing.T, endpoint string, settings map[string]any, secretKey string) (*ecr.Adapter, error) {
	t.Helper()
	if secretKey != "" {
		settings["credentials"] = map[string]string{"secret_access_key": secretKey}
	}
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	a := ecr.New()
	a.Endpoint = endpoint
	return a, a.Configure(context.Background(), raw)
}

// TestR254_ECRIsOneRepositoryAndAMintedPassword asserts the two ways ECR
// differs from an OCI registry, as the category reports them (R-254): it
// does not create repositories on push, so every build goes into the one
// repository its address names, tagged by app and deployment; and each push
// and pull signs in with a password minted from the access key just before.
func TestR254_ECRIsOneRepositoryAndAMintedPassword(t *testing.T) {
	endpoint, calls := fakeECR(t)
	a, err := open(t, endpoint, map[string]any{"url": repository, "access_key_id": "AKIAEXAMPLE"}, "s3cr3t")
	require.NoError(t, err)

	caps := a.ImageRegistryCapabilities()
	require.False(t, caps.CreatesRepositoriesOnPush)
	require.False(t, caps.RepositoryPerApp)
	require.Equal(t, "123456789012.dkr.ecr.us-east-1.amazonaws.com", caps.Host)

	target, err := a.Target(context.Background(), "app_01HQ8", "web", "dep_01HQ9")
	require.NoError(t, err)
	require.Equal(t, repository, target.Repository)
	require.Equal(t, "app_01hq8-web-dep_01hq9", target.Tag)
	require.Equal(t, "AWS", target.Auth.Username)
	require.Equal(t, "minted-1", target.Auth.Password.Reveal())
	require.True(t, a.Owns(repository+"@sha256:abc"))

	auth, err := a.PullAuth(context.Background())
	require.NoError(t, err)
	require.Equal(t, "minted-2", auth.Password.Reveal(), "minted again, never kept")
	require.EqualValues(t, 2, calls.Load())

	require.NoError(t, ecr.Info().Validate())
	require.Equal(t, api.CategoryImageRegistry, a.Category())
}

// TestAnECRRegistryThatCannotWorkIsRefusedWhenConfigured asserts each
// refusal: an address that is not ECR, no repository to put builds in, and a
// key that is missing half.
func TestAnECRRegistryThatCannotWorkIsRefusedWhenConfigured(t *testing.T) {
	_, err := open(t, "", map[string]any{"url": "https://registry.internal/pando", "access_key_id": "AKIAEXAMPLE"}, "key")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "not an ECR registry")
	require.Contains(t, errs.As(err).Remedy, "OCI registry")

	_, err = open(t, "", map[string]any{"url": "123456789012.dkr.ecr.us-east-1.amazonaws.com", "access_key_id": "AKIAEXAMPLE"}, "key")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "names no repository")

	_, err = open(t, "", map[string]any{"url": repository}, "key")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "access key ID and its secret access key")

	_, err = open(t, "", map[string]any{"url": repository, "access_key_id": "AKIAEXAMPLE"}, "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}

// TestR105_ARefusedECRKeySaysWhichCredentialToReplace asserts that a key AWS
// refuses fails the push target with a message naming the image registry's
// credential, not an app's, and never the secret key (R-194).
func TestR105_ARefusedECRKeySaysWhichCredentialToReplace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"UnrecognizedClientException","message":"The security token included in the request is invalid."}`))
	}))
	t.Cleanup(srv.Close)
	a, err := open(t, srv.URL, map[string]any{"url": repository, "access_key_id": "AKIAEXAMPLE"}, "s3cr3t")
	require.NoError(t, err)

	_, err = a.Target(context.Background(), "app_1", "", "dep_1")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	e := errs.As(err)
	require.Contains(t, e.Message, "the image registry's access key")
	require.Contains(t, e.Remedy, "image registry adapter's settings")
	require.NotContains(t, err.Error(), "s3cr3t")
}
