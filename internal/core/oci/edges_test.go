package oci_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

func TestAReferenceThatDoesNotParseSaysWhatOneLooksLike(t *testing.T) {
	_, err := oci.Inspector{}.Inspect(context.Background(), "Not A Reference", nil, amd64)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "ghcr.io/acme/web:1.4")

	_, err = oci.Registry("Not A Reference")
	require.Error(t, err)
	got, err := oci.Registry("nginx:1.27")
	require.NoError(t, err)
	require.Equal(t, "index.docker.io", got)
}

// A registry that does not answer is a different failure from one that
// refuses, and says to check the network rather than the name.
func TestARegistryThatDoesNotAnswerSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	host := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	_, err := oci.Inspector{Insecure: true, Transport: http.DefaultTransport}.Inspect(context.Background(), host+"/acme/web:1", nil, amd64)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "reachable")
}

func TestEveryFormOfHealthcheckIsRead(t *testing.T) {
	host, in := serve(t)
	for tag, test := range map[string][]string{
		"cmd":   {"CMD", "/healthz", "--quick"},
		"none":  {"NONE"},
		"bare":  {"/bin/check"},
		"shell": {"CMD-SHELL", "curl -f localhost"},
	} {
		img, err := random.Image(16, 1)
		require.NoError(t, err)
		cfg, err := img.ConfigFile()
		require.NoError(t, err)
		cfg = cfg.DeepCopy()
		cfg.OS, cfg.Architecture = "linux", "amd64"
		cfg.Config.Healthcheck = &v1.HealthConfig{Test: test, Timeout: 5 * time.Second}
		img, err = mutate.ConfigFile(img, cfg)
		require.NoError(t, err)
		push(t, host+"/acme/health:"+tag, img)
	}

	read := func(tag string) *oci.Health {
		got, err := in.Inspect(context.Background(), host+"/acme/health:"+tag, nil, amd64)
		require.NoError(t, err)
		return got.Config.Healthcheck
	}
	require.Equal(t, []string{"/healthz", "--quick"}, read("cmd").Command)
	require.Nil(t, read("none"), "NONE turns a base image's check off")
	require.Equal(t, []string{"/bin/check"}, read("bare").Command)
	require.Equal(t, 5*time.Second, read("shell").Timeout)
}

func TestPlatformsAreNamedTheWayRegistriesNameThem(t *testing.T) {
	p, ok := oci.ParsePlatform("Linux/ARMv7l/v7")
	require.True(t, ok)
	require.Equal(t, "linux/arm/v7", p.String())
	require.False(t, oci.Platform{OS: "linux", Architecture: "arm", Variant: "v6"}.Runs(p))
	require.True(t, oci.Platform{OS: "linux", Architecture: "arm"}.Runs(p))
	require.False(t, oci.Platform{OS: "windows", Architecture: "amd64"}.Runs(amd64))

	for in, want := range map[string]string{"i686": "386", "x86-64": "amd64", "riscv64": "riscv64"} {
		require.Equal(t, want, oci.NormalizeArch(in), in)
	}

	err := oci.PlatformMismatch("acme/web:1", arm64, nil)
	require.Equal(t, errs.PlanImagePlatformUnsupported, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "no platform Pando recognizes")
}

func TestAnImageReferenceIsWhatAnImageSourcePulls(t *testing.T) {
	require.Equal(t, "nginx@sha256:aa", oci.Reference(spec.Source{Type: spec.SourceImage, Image: "nginx:1", Digest: "sha256:aa"}))
	require.Equal(t, "nginx:1", oci.Reference(spec.Source{Type: spec.SourceImage, Image: "nginx:1"}))
	require.Empty(t, oci.Reference(spec.Source{Type: spec.SourceGit, URL: "https://github.com/acme/web"}))
}

// ecr answers GetAuthorizationToken with whatever reply gives.
func ecr(t *testing.T, reply func(w http.ResponseWriter)) oci.Resolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reply(w) }))
	t.Cleanup(srv.Close)
	return oci.Resolver{ECREndpoint: srv.URL}
}

func ecrToken(w http.ResponseWriter, token any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(map[string]any{"authorizationData": []map[string]any{{"authorizationToken": token}}})
}

func TestECRFailuresAreSaidPlainly(t *testing.T) {
	c := oci.Credential{Kind: oci.CredentialECR, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: secret.New("s"), Region: "eu-west-1"}
	ref := "123456789012.dkr.ecr.us-east-1.amazonaws.com/team/api:v3"
	ctx := context.Background()

	refused := ecr(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"UnrecognizedClientException","message":"The security token included in the request is invalid."}`))
	})
	_, err := refused.Resolve(ctx, c, ref)
	require.Contains(t, errs.As(err).Message, "AWS refused the app's access key")
	require.Contains(t, errs.As(err).Message, "eu-west-1", "the credential's own region, not the host's")

	for name, reply := range map[string]func(http.ResponseWriter){
		"empty":    func(w http.ResponseWriter) { ecrToken(w, nil) },
		"not64":    func(w http.ResponseWriter) { ecrToken(w, "%%%") },
		"no colon": func(w http.ResponseWriter) { ecrToken(w, base64.StdEncoding.EncodeToString([]byte("justatoken"))) },
	} {
		_, err := ecr(t, reply).Resolve(ctx, c, ref)
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err), name)
	}

	_, err = oci.Resolver{}.Resolve(ctx, oci.Credential{Kind: oci.CredentialBasic}, ref)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "an incomplete credential is refused before any call")
	_, err = oci.Resolver{}.Resolve(ctx, oci.Credential{Kind: oci.CredentialBasic, Username: "u", Password: secret.New("p")}, "Not A Reference")
	require.Error(t, err)
}

func TestImagesWithNoStoreHaveNoCredential(t *testing.T) {
	ctx := context.Background()
	var none *oci.Images
	auth, err := none.Auth(ctx, "app_1", "nginx")
	require.NoError(t, err)
	require.Nil(t, auth)

	_, ok, err := none.DescribeCredential(ctx, "app_1")
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, none.RemoveCredential(ctx, "app_1"))

	err = (&oci.Images{}).SaveCredential(ctx, "app_1", oci.Credential{Kind: oci.CredentialBasic, Username: "u", Password: secret.New("p")})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	err = (&oci.Images{Credentials: memStore{}}).SaveCredential(ctx, "app_1", oci.Credential{Kind: "token"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}

type failingStore struct{ memStore }

func (failingStore) Resolve(context.Context, string) (map[string]secret.Value, error) {
	return nil, errors.New("sealed rows could not be read")
}
func (failingStore) Delete(context.Context, string, string) error {
	return errors.New("sealed rows could not be removed")
}

func TestAStoreThatFailsStopsTheRead(t *testing.T) {
	im := &oci.Images{Credentials: failingStore{}}
	ctx := context.Background()
	_, err := im.Auth(ctx, "app_1", "nginx")
	require.Error(t, err)
	_, err = im.Inspect(ctx, "app_1", "nginx", amd64)
	require.Error(t, err)
	_, _, err = im.DescribeCredential(ctx, "app_1")
	require.Error(t, err)
	require.Error(t, im.SaveCredential(ctx, "app_1", oci.Credential{Kind: oci.CredentialBasic, Username: "u", Password: secret.New("p")}))
}

// A registry signed in to through a browser leaves an identity token rather
// than a password, and that is what is passed on.
func TestADockerLoginWithAnIdentityTokenIsCarried(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"auths":{"ghcr.io":{"auth":"`+base64.StdEncoding.EncodeToString([]byte("<token>:"))+`","identitytoken":"idt-123"}}}`), 0o600))
	t.Setenv("DOCKER_CONFIG", dir)
	im := &oci.Images{Docker: oci.DockerLogin()}

	auth, err := im.Auth(context.Background(), "", "ghcr.io/acme/private:1")
	require.NoError(t, err)
	require.Equal(t, "idt-123", auth.IdentityToken.Reveal())

	auth, err = im.Auth(context.Background(), "", "Not A Reference")
	require.NoError(t, err)
	require.Nil(t, auth)
}

func TestADockerLoginThatCannotBeReadSaysWhere(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"credsStore":"pando-test-helper-that-does-not-exist"}`), 0o600))
	t.Setenv("DOCKER_CONFIG", dir)
	_, err := (&oci.Images{Docker: oci.DockerLogin()}).Auth(context.Background(), "", "ghcr.io/acme/private:1")
	require.Error(t, err, "a credential helper that cannot run is a failure to say, not an anonymous pull")
	require.Contains(t, errs.As(err).Message, "Docker login on its server")
}
