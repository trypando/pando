package oci_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

var (
	amd64 = oci.Platform{OS: "linux", Architecture: "amd64"}
	arm64 = oci.Platform{OS: "linux", Architecture: "arm64"}
)

// image builds an image for a platform with the configuration an app's
// Dockerfile would give it.
func image(t *testing.T, p oci.Platform) v1.Image {
	t.Helper()
	img, err := random.Image(64, 1)
	require.NoError(t, err)
	cfg, err := img.ConfigFile()
	require.NoError(t, err)
	cfg = cfg.DeepCopy()
	cfg.OS, cfg.Architecture = p.OS, p.Architecture
	cfg.Config.ExposedPorts = map[string]struct{}{"8080/tcp": {}, "53/udp": {}, "3000": {}}
	cfg.Config.Volumes = map[string]struct{}{"/data": {}, "/var/lib/app": {}}
	cfg.Config.Cmd = []string{"server", "--listen", ":8080"}
	cfg.Config.Healthcheck = &v1.HealthConfig{
		Test: []string{"CMD-SHELL", "wget -qO- localhost:8080/healthz"}, Interval: 30 * time.Second, Retries: 3,
	}
	img, err = mutate.ConfigFile(img, cfg)
	require.NoError(t, err)
	return img
}

func serve(t *testing.T) (host string, in oci.Inspector) {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), oci.Inspector{Insecure: true}
}

func push(t *testing.T, ref string, img v1.Image) {
	t.Helper()
	r, err := name.ParseReference(ref, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(r, img))
}

// A tag resolves to a digest, and the configuration says where the app
// listens, what it keeps and how it is checked — everything detection has no
// repository to read for an image (issue #41).
func TestATagResolvesToADigestAndItsConfiguration(t *testing.T) {
	host, in := serve(t)
	img := image(t, amd64)
	push(t, host+"/acme/web:1.4", img)
	want, err := img.Digest()
	require.NoError(t, err)

	got, err := in.Inspect(context.Background(), host+"/acme/web:1.4", nil, amd64)
	require.NoError(t, err)

	require.Equal(t, want.String(), got.Digest)
	require.Equal(t, host+"/acme/web@"+want.String(), got.Pinned())
	require.Equal(t, []oci.Platform{amd64}, got.Platforms)
	require.NotNil(t, got.Config)
	require.Equal(t, []int{3000, 8080}, got.Config.ExposedPorts, "UDP is not a port the proxy can send HTTP to")
	require.Equal(t, []string{"/data", "/var/lib/app"}, got.Config.Volumes)
	require.Equal(t, []string{"/bin/sh", "-c", "wget -qO- localhost:8080/healthz"}, got.Config.Healthcheck.Command)
	require.Equal(t, 30*time.Second, got.Config.Healthcheck.Interval)
	require.Equal(t, []string{"server", "--listen", ":8080"}, got.Config.Cmd)
}

// A multi-platform image pins the index, so the runtime still picks its own
// build from it, and the configuration is the host's build's.
func TestAMultiPlatformImagePinsTheIndexAndReadsTheHostsBuild(t *testing.T) {
	host, in := serve(t)
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: image(t, amd64), Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: image(t, arm64), Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}}},
		mutate.IndexAddendum{Add: image(t, amd64), Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "unknown", Architecture: "unknown"}}},
	)
	r, err := name.ParseReference(host+"/acme/web:latest", name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.WriteIndex(r, idx))
	want, err := idx.Digest()
	require.NoError(t, err)

	got, err := in.Inspect(context.Background(), host+"/acme/web:latest", nil, arm64)
	require.NoError(t, err)
	require.Equal(t, want.String(), got.Digest)
	require.Len(t, got.Platforms, 2, "an attestation is not a platform")
	require.True(t, got.Supports(arm64))
	require.NotNil(t, got.Config)
	require.Equal(t, "arm64", got.Matched.Architecture)
}

func TestAnImageWithNoBuildForTheHostHasNoConfiguration(t *testing.T) {
	host, in := serve(t)
	push(t, host+"/acme/web:1", image(t, amd64))

	got, err := in.Inspect(context.Background(), host+"/acme/web:1", nil, arm64)
	require.NoError(t, err)
	require.False(t, got.Supports(arm64))
	require.Nil(t, got.Config)
}

func TestAMissingImageIsSaidPlainly(t *testing.T) {
	host, in := serve(t)
	_, err := in.Inspect(context.Background(), host+"/acme/nothing:1", nil, amd64)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "has no image")
}

// A private image without a credential names the fix; with a wrong one, says
// it was refused. Neither says "401".
func TestARegistryThatWantsCredentialsSaysWhatToAdd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	in := oci.Inspector{Insecure: true}

	_, err := in.Inspect(context.Background(), host+"/acme/private:1", nil, amd64)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "without signing in")
	require.Contains(t, errs.As(err).Remedy, "registry credential")

	_, err = in.Inspect(context.Background(), host+"/acme/private:1",
		&oci.Auth{Username: "ben", Password: secret.New("wrong")}, amd64)
	require.Contains(t, errs.As(err).Message, "refused the app's registry credential")
	require.NotContains(t, fmt.Sprint(err), "wrong")
}

func TestPinReplacesTheTagAndKeepsTheNameAsWritten(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	require.Equal(t, "nginx@"+d, oci.Pin("nginx:1.27", d))
	require.Equal(t, "nginx@"+d, oci.Pin("nginx", d))
	require.Equal(t, "localhost:5000/acme/web@"+d, oci.Pin("localhost:5000/acme/web:2", d))
	require.Equal(t, "localhost:5000/acme/web@"+d, oci.Pin("localhost:5000/acme/web", d))
	require.Equal(t, "ghcr.io/acme/web@"+d, oci.Pin("ghcr.io/acme/web:1@sha256:"+strings.Repeat("b", 64), d))
	require.Equal(t, "nginx:1.27", oci.Pin("nginx:1.27", ""))
}

func TestPlatformsCompareTheWayRuntimesNameThem(t *testing.T) {
	host, ok := oci.ParsePlatform("linux/aarch64")
	require.True(t, ok)
	require.True(t, oci.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}.Runs(host))
	require.False(t, amd64.Runs(host))

	x86, _ := oci.ParsePlatform("linux/x86_64")
	require.True(t, amd64.Runs(x86))
	_, ok = oci.ParsePlatform("linux")
	require.False(t, ok)
}

func TestACredentialRoundTripsThroughItsStoredFields(t *testing.T) {
	c := oci.Credential{Kind: oci.CredentialECR, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: secret.New("s3cr3t"), Region: "eu-west-1"}
	back, ok := oci.CredentialFrom(c.Fields())
	require.True(t, ok)
	require.Equal(t, c.AccessKeyID, back.AccessKeyID)
	require.Equal(t, "s3cr3t", back.SecretAccessKey.Reveal())
	require.Equal(t, "eu-west-1", back.Region)
	require.NotContains(t, fmt.Sprintf("%v %+v", back, back), "s3cr3t", "R-194")

	_, ok = oci.CredentialFrom(nil)
	require.False(t, ok)

	require.Error(t, oci.Credential{Kind: oci.CredentialBasic, Username: "ben"}.Validate())
	require.Error(t, oci.Credential{Kind: "token"}.Validate())
	require.Error(t, oci.Credential{Kind: oci.CredentialECR, AccessKeyID: "A", SecretAccessKey: secret.New("x"), Region: "nowhere"}.Validate())
}

func TestECRRegionIsReadFromTheRegistryHost(t *testing.T) {
	region, ok := oci.ECRRegion("123456789012.dkr.ecr.us-east-1.amazonaws.com")
	require.True(t, ok)
	require.Equal(t, "us-east-1", region)
	_, ok = oci.ECRRegion("ghcr.io")
	require.False(t, ok)
}

// ECR's registry passwords last twelve hours, so one is minted from the
// access key for every pull rather than stored.
func TestAnECRCredentialMintsARegistryPasswordForEachPull(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken", r.Header.Get("X-Amz-Target"))
		require.Contains(t, r.Header.Get("Authorization"), "AKIAEXAMPLE/", "signed with the app's key")
		token := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("AWS:minted-%d", calls)))
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorizationData": []map[string]any{{"authorizationToken": token}},
		})
	}))
	t.Cleanup(srv.Close)

	r := oci.Resolver{ECREndpoint: srv.URL}
	c := oci.Credential{Kind: oci.CredentialECR, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: secret.New("s3cr3t")}
	ref := "123456789012.dkr.ecr.us-east-1.amazonaws.com/team/api:v3"

	first, err := r.Resolve(context.Background(), c, ref)
	require.NoError(t, err)
	require.Equal(t, "AWS", first.Username)
	require.Equal(t, "minted-1", first.Password.Reveal())
	require.Equal(t, "123456789012.dkr.ecr.us-east-1.amazonaws.com", first.Registry)

	second, err := r.Resolve(context.Background(), c, ref)
	require.NoError(t, err)
	require.Equal(t, "minted-2", second.Password.Reveal())

	_, err = r.Resolve(context.Background(), c, "ghcr.io/acme/web:1")
	require.Contains(t, errs.As(err).Message, "not an ECR registry")
}
