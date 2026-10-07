package imageregistry

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

func TestNoURLIsNoRegistry(t *testing.T) {
	r, err := New(Config{})
	require.NoError(t, err)
	require.Nil(t, r)
	require.False(t, r.Configured())
	require.False(t, r.Always())
	require.False(t, r.Owns("anything"))
	n, err := r.DeleteApp(context.Background(), "app_1", nil)
	require.NoError(t, err)
	require.Zero(t, n)
}

// TestR194_PlainHTTPOnlyWhenTheOperatorSaysSo asserts O-35: a registry
// reached over plain HTTP is refused at startup unless PANDO_REGISTRY_INSECURE
// is set, and the refusal names the setting.
func TestR194_PlainHTTPOnlyWhenTheOperatorSaysSo(t *testing.T) {
	_, err := New(Config{URL: "http://registry.internal:5000"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "PANDO_REGISTRY_INSECURE")

	r, err := New(Config{URL: "http://registry.internal:5000", Insecure: true})
	require.NoError(t, err)
	target, err := r.Target(context.Background(), "app_1", "", "dep_1")
	require.NoError(t, err)
	require.True(t, target.Insecure)

	r, err = New(Config{URL: "https://registry.internal:5000"})
	require.NoError(t, err)
	target, err = r.Target(context.Background(), "app_1", "", "dep_1")
	require.NoError(t, err)
	require.False(t, target.Insecure)
	require.Nil(t, target.Auth, "no credential configured, an anonymous push")

	_, err = New(Config{URL: "ftp://registry.internal"})
	require.Error(t, err)
}

func TestTheCredentialIsCompleteOrRefused(t *testing.T) {
	_, err := New(Config{URL: "registry.internal", Username: "pando"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "PANDO_REGISTRY_PASSWORD")

	_, err = New(Config{URL: "registry.internal", Kind: "token"})
	require.Contains(t, errs.As(err).Message, "PANDO_REGISTRY_KIND")

	_, err = New(Config{URL: "registry.internal", Layout: "flat"})
	require.Contains(t, errs.As(err).Message, "PANDO_REGISTRY_LAYOUT")

	_, err = New(Config{URL: "registry.internal", Layout: "single"})
	require.Contains(t, errs.As(err).Message, "PANDO_REGISTRY_URL", "single needs a repository to put images in")

	r, err := New(Config{URL: "registry.internal", Username: "pando", Password: secret.New("pw")})
	require.NoError(t, err)
	auth, err := r.Auth(context.Background())
	require.NoError(t, err)
	require.Equal(t, "registry.internal", auth.Registry)
	require.Equal(t, "pando", auth.Username)
	require.Equal(t, "pw", auth.Password.Reveal())
}

func TestRepositoriesFollowTheLayout(t *testing.T) {
	r, err := New(Config{URL: "https://Registry.internal:5000/Pando/"})
	require.NoError(t, err)
	repo, tag := r.Repository("app_01HQ8", "", "dep_01HQ9")
	require.Equal(t, "registry.internal:5000/pando/apps/app_01hq8", repo)
	require.Equal(t, "dep_01hq9", tag)
	repo, _ = r.Repository("app_01HQ8", "Web Worker", "dep_01HQ9")
	require.Equal(t, "registry.internal:5000/pando/apps/app_01hq8/web-worker", repo)
	require.True(t, r.Owns(repo+"@sha256:abc"))
	require.False(t, r.Owns("registry.internal:5000/other/app@sha256:abc"))
	require.False(t, r.Owns("docker.io/library/nginx@sha256:abc"))

	single, err := New(Config{URL: "123456789012.dkr.ecr.us-east-1.amazonaws.com/pando", Layout: "single"})
	require.NoError(t, err)
	repo, tag = single.Repository("app_01HQ8", "web", "dep_01HQ9")
	require.Equal(t, "123456789012.dkr.ecr.us-east-1.amazonaws.com/pando", repo)
	require.Equal(t, "app_01hq8-web-dep_01hq9", tag)
	require.True(t, single.Owns(repo+"@sha256:abc"))

	for _, ref := range []string{repo + ":" + tag} {
		_, err := name.ParseReference(ref)
		require.NoError(t, err, "a valid reference")
	}
}

// TestR224_ADeletedAppsImagesAreRemovedFromTheRegistry asserts R-224 for the
// registry: deleting an app deletes every manifest of its builds — the
// app-wide image and each separately built workload, every build — and
// leaves another app's alone.
func TestR224_ADeletedAppsImagesAreRemovedFromTheRegistry(t *testing.T) {
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	for _, layout := range []string{"per_app", "single"} {
		t.Run(layout, func(t *testing.T) {
			r, err := New(Config{URL: "http://" + host + "/" + layout, Insecure: true, Layout: layout})
			require.NoError(t, err)

			push := func(app, workload, dep string) string {
				repo, tag := r.Repository(app, workload, dep)
				ref, err := name.NewTag(repo+":"+tag, name.Insecure)
				require.NoError(t, err)
				img, err := random.Image(64, 1)
				require.NoError(t, err)
				require.NoError(t, remote.Write(ref, img))
				d, err := img.Digest()
				require.NoError(t, err)
				// By digest: what a deployment records, and what a registry
				// that deletes a manifest no longer serves.
				return repo + "@" + d.String()
			}
			gone := []string{
				push("app_1", "", "dep_1"), push("app_1", "", "dep_2"),
				push("app_1", "api", "dep_2"),
			}
			kept := push("app_2", "", "dep_3")

			n, err := r.DeleteApp(context.Background(), "app_1", []string{"api", "api"})
			require.NoError(t, err)
			require.Equal(t, 3, n)

			for _, ref := range gone {
				parsed, err := name.ParseReference(ref, name.Insecure)
				require.NoError(t, err)
				_, err = remote.Head(parsed)
				require.Error(t, err, ref+" is gone")
			}
			parsed, err := name.ParseReference(kept, name.Insecure)
			require.NoError(t, err)
			_, err = remote.Head(parsed)
			require.NoError(t, err, "another app's image is untouched")

			n, err = r.DeleteApp(context.Background(), "app_1", []string{"api"})
			require.NoError(t, err)
			require.Zero(t, n, "a second pass finds nothing and is not an error")

			n, err = r.DeleteApp(context.Background(), "app_never_built", nil)
			require.NoError(t, err)
			require.Zero(t, n)
		})
	}
}
