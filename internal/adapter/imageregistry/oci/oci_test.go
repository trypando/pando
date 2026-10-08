package oci_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/imageregistry/oci"
	"github.com/trypando/pando/internal/errs"
)

// open configures an OCI registry adapter from its settings, with the
// password where core puts a credential (O-20).
func open(t *testing.T, settings map[string]any, password string) (*oci.Adapter, error) {
	t.Helper()
	if password != "" {
		settings["credentials"] = map[string]string{"password": password}
	}
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	a := oci.New()
	return a, a.Configure(context.Background(), raw)
}

func mustOpen(t *testing.T, settings map[string]any, password string) *oci.Adapter {
	t.Helper()
	a, err := open(t, settings, password)
	require.NoError(t, err)
	return a
}

type cfg = map[string]any

// TestR252_TheImageRegistryIsAnAdapterCategory asserts R-252 as amended by
// issue #153: the install's image registry is an adapter, each kind's form is
// valid, and the registry Pando probes for published images is not one.
func TestR252_TheImageRegistryIsAnAdapterCategory(t *testing.T) {
	a := oci.New()
	require.Equal(t, api.CategoryImageRegistry, a.Category())
	require.Equal(t, oci.Kind, a.Kind())
	info := oci.Info()
	require.NoError(t, info.Validate())
	require.Equal(t, api.CategoryImageRegistry, info.Category)

	r := api.NewRegistry()
	require.NoError(t, r.Register("reg_1", mustOpen(t, cfg{"url": "registry.internal"}, "")))
	got, ok := r.ImageRegistry("reg_1")
	require.True(t, ok)
	require.True(t, got.ImageRegistryCapabilities().CreatesRepositoriesOnPush)
}

func TestAnUnconfiguredRegistryRefusesToPush(t *testing.T) {
	a := oci.New()
	require.False(t, a.Owns("anything"))
	_, err := a.Target(context.Background(), "app_1", "", "dep_1")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(a.HealthCheck(context.Background())))
	n, err := a.DeleteApp(context.Background(), "app_1", nil)
	require.NoError(t, err)
	require.Zero(t, n)

	_, err = open(t, cfg{}, "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "url")
}

// TestR194_PlainHTTPOnlyWhenTheOperatorSaysSo asserts O-35: a registry
// reached over plain HTTP is refused when the adapter is configured unless
// plain HTTP is allowed for it, and the refusal names the setting.
func TestR194_PlainHTTPOnlyWhenTheOperatorSaysSo(t *testing.T) {
	_, err := open(t, cfg{"url": "http://registry.internal:5000"}, "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "Allow plain HTTP")

	r, err := open(t, cfg{"url": "http://registry.internal:5000", "insecure": true}, "")
	require.NoError(t, err)
	target, err := r.Target(context.Background(), "app_1", "", "dep_1")
	require.NoError(t, err)
	require.True(t, target.Insecure)

	r, err = open(t, cfg{"url": "https://registry.internal:5000"}, "")
	require.NoError(t, err)
	target, err = r.Target(context.Background(), "app_1", "", "dep_1")
	require.NoError(t, err)
	require.False(t, target.Insecure)
	require.Nil(t, target.Auth, "no credential configured, an anonymous push")

	_, err = open(t, cfg{"url": "ftp://registry.internal"}, "")
	require.Error(t, err)
}

func TestTheCredentialIsCompleteOrRefused(t *testing.T) {
	_, err := open(t, cfg{"url": "registry.internal", "username": "pando"}, "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "both a username and a password")

	_, err = open(t, cfg{"url": "registry.internal"}, "pw")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "a password with no username is incomplete too")

	_, err = open(t, cfg{"url": "registry.internal", "layout": "flat"}, "")
	require.Contains(t, errs.As(err).Message, "per_app (a repository per app) or single")

	_, err = open(t, cfg{"url": "registry.internal", "layout": "single"}, "")
	require.Contains(t, errs.As(err).Message, "names no repository", "single needs a repository to put images in")

	r, err := open(t, cfg{"url": "registry.internal", "username": "pando"}, "pw")
	require.NoError(t, err)
	auth, err := r.PullAuth(context.Background())
	require.NoError(t, err)
	require.Equal(t, "registry.internal", auth.Registry)
	require.Equal(t, "pando", auth.Username)
	require.Equal(t, "pw", auth.Password.Reveal())
}

func TestRepositoriesFollowTheLayout(t *testing.T) {
	r, err := open(t, cfg{"url": "https://Registry.internal:5000/Pando/"}, "")
	require.NoError(t, err)
	repo, tag := r.Repository("app_01HQ8", "", "dep_01HQ9")
	require.Equal(t, "registry.internal:5000/pando/apps/app_01hq8", repo)
	require.Equal(t, "dep_01hq9", tag)
	repo, _ = r.Repository("app_01HQ8", "Web Worker", "dep_01HQ9")
	require.Equal(t, "registry.internal:5000/pando/apps/app_01hq8/web-worker", repo)
	require.True(t, r.Owns(repo+"@sha256:abc"))
	require.False(t, r.Owns("registry.internal:5000/other/app@sha256:abc"))
	require.False(t, r.Owns("docker.io/library/nginx@sha256:abc"))

	single, err := open(t, cfg{"url": "123456789012.dkr.ecr.us-east-1.amazonaws.com/pando", "layout": "single"}, "")
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
			r, err := open(t, cfg{"url": "http://" + host + "/" + layout, "insecure": true, "layout": layout}, "")
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

// fronted is an in-memory registry behind a handler the test can make refuse:
// before sees each request first and answers it itself by returning true.
type fronted struct {
	*httptest.Server
	host   string
	before atomic.Pointer[func(w http.ResponseWriter, r *http.Request) bool]
}

func newFronted(t *testing.T) *fronted {
	t.Helper()
	f := &fronted{}
	inner := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if before := f.before.Load(); before != nil && (*before)(w, r) {
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	f.host = strings.TrimPrefix(f.URL, "http://")
	return f
}

func (f *fronted) refuse(fn func(w http.ResponseWriter, r *http.Request) bool) { f.before.Store(&fn) }

// pushTo writes a random image to the registry as one app's build.
func pushTo(t *testing.T, r *oci.Adapter, app, dep string, opts ...remote.Option) {
	t.Helper()
	repo, tag := r.Repository(app, "", dep)
	ref, err := name.NewTag(repo+":"+tag, name.Insecure)
	require.NoError(t, err)
	img, err := random.Image(64, 1)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img, opts...))
}

// registryError answers a registry API request with a registry error body.
func registryError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"errors":[{"code":"`+code+`","message":"refused by the test"}]}`)
}

// TestR224_ARegistryThatWillNotDeleteSaysWhatToChange asserts the message an
// operator sees when the registry refuses to delete a deleted app's images: it
// names the registry and the setting that allows deletes (R-105).
func TestR224_ARegistryThatWillNotDeleteSaysWhatToChange(t *testing.T) {
	t.Parallel()
	f := newFronted(t)
	r, err := open(t, cfg{"url": "http://" + f.host, "insecure": true}, "")
	require.NoError(t, err)
	pushTo(t, r, "app_1", "dep_1")

	f.refuse(func(w http.ResponseWriter, req *http.Request) bool {
		if req.Method == http.MethodDelete {
			registryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED")
			return true
		}
		return false
	})
	n, err := r.DeleteApp(context.Background(), "app_1", nil)
	require.Zero(t, n)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	e := errs.As(err)
	require.Contains(t, e.Message, f.host)
	require.Contains(t, e.Remedy, "storage.delete.enabled")
}

// TestR224_AnImageAlreadyGoneIsNotAnError asserts that a manifest deleted
// between the listing and the delete — by the registry's own GC, or another
// pass — counts as gone rather than failing the teardown.
func TestR224_AnImageAlreadyGoneIsNotAnError(t *testing.T) {
	t.Parallel()
	f := newFronted(t)
	r, err := open(t, cfg{"url": "http://" + f.host, "insecure": true}, "")
	require.NoError(t, err)
	pushTo(t, r, "app_1", "dep_1")
	pushTo(t, r, "app_1", "dep_2")

	var heads atomic.Int32
	f.refuse(func(w http.ResponseWriter, req *http.Request) bool {
		switch {
		case req.Method == http.MethodHead && heads.Add(1) == 1:
			registryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")
			return true
		case req.Method == http.MethodDelete:
			registryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")
			return true
		}
		return false
	})
	n, err := r.DeleteApp(context.Background(), "app_1", nil)
	require.NoError(t, err)
	require.Zero(t, n, "nothing this pass deleted")
}

// TestR224_ARegistryThatCannotBeReadFailsTheTeardownPassReadably asserts that
// a registry that cannot be listed, or whose images cannot be read, fails the
// pass with a message naming the registry, so the next pass tries again.
func TestR224_ARegistryThatCannotBeReadFailsTheTeardownPassReadably(t *testing.T) {
	t.Parallel()
	f := newFronted(t)
	r, err := open(t, cfg{"url": "http://" + f.host, "insecure": true}, "")
	require.NoError(t, err)
	pushTo(t, r, "app_1", "dep_1")

	f.refuse(func(w http.ResponseWriter, req *http.Request) bool {
		if req.Method == http.MethodHead {
			registryError(w, http.StatusInternalServerError, "UNKNOWN")
			return true
		}
		return false
	})
	_, err = r.DeleteApp(context.Background(), "app_1", nil)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "could not read a deleted app's image in the registry "+f.host)

	f.refuse(func(w http.ResponseWriter, req *http.Request) bool {
		if strings.HasSuffix(req.URL.Path, "/tags/list") {
			registryError(w, http.StatusForbidden, "DENIED")
			return true
		}
		return false
	})
	_, err = r.DeleteApp(context.Background(), "app_1", nil)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "could not list a deleted app's images in the registry "+f.host)
	require.Contains(t, errs.As(err).Remedy, "accepts its credential")
}

// TestR190_TeardownUsesTheRegistrysCredential asserts that deleting images
// authenticates with the install's credential, against a registry that admits
// nobody else.
func TestR190_TeardownUsesTheRegistrysCredential(t *testing.T) {
	t.Parallel()
	f := newFronted(t)
	f.refuse(func(w http.ResponseWriter, req *http.Request) bool {
		if user, pass, ok := req.BasicAuth(); ok && user == "pando" && pass == "s3cret" {
			return false
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		registryError(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return true
	})
	r, err := open(t, cfg{"url": "http://" + f.host + "/pando", "insecure": true, "username": "pando"}, "s3cret")
	require.NoError(t, err)
	auth, err := r.PullAuth(context.Background())
	require.NoError(t, err)
	pushTo(t, r, "app_1", "dep_1", remote.WithAuth(&authn.Basic{Username: auth.Username, Password: auth.Password.Reveal()}))

	n, err := r.DeleteApp(context.Background(), "app_1", nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	anonymous, err := open(t, cfg{"url": "http://" + f.host + "/pando", "insecure": true}, "")
	require.NoError(t, err)
	_, err = anonymous.DeleteApp(context.Background(), "app_1", nil)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err), "without the credential the registry refuses")
}

// TestARegistryAddressPandoCannotUseIsRefusedWhenConfigured asserts each startup
// refusal for an address when the adapter is configured: a credential in the URL (never quoted back, R-194),
// no host, and a path no repository can have.
func TestARegistryAddressPandoCannotUseIsRefusedWhenConfigured(t *testing.T) {
	t.Parallel()
	_, err := open(t, cfg{"url": "https://pando:hunter2@registry.internal"}, "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.NotContains(t, errs.As(err).Message, "hunter2")

	_, err = open(t, cfg{"url": "https://"}, "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "names no registry host")

	_, err = open(t, cfg{"url": "registry.internal/pando/bad path!"}, "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "lowercase path")

	always, err := open(t, cfg{"url": "registry.internal:5000", "always": true}, "")
	require.NoError(t, err)
	caps := always.ImageRegistryCapabilities()
	require.True(t, caps.SendsEveryBuild)
	require.True(t, caps.RepositoryPerApp)
	require.Equal(t, "registry.internal:5000", caps.Host)
}

// TestR254_TheHealthCheckSignsInToTheRegistry asserts that an image registry
// whose credential is refused is reported unhealthy, so the adapters screen
// says so before a deploy needs it.
func TestR254_TheHealthCheckSignsInToTheRegistry(t *testing.T) {
	t.Parallel()
	f := newFronted(t)
	f.refuse(func(w http.ResponseWriter, req *http.Request) bool {
		if user, pass, ok := req.BasicAuth(); ok && user == "pando" && pass == "s3cret" {
			return false
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		registryError(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return true
	})
	good := mustOpen(t, cfg{"url": "http://" + f.host, "insecure": true, "username": "pando"}, "s3cret")
	require.NoError(t, good.HealthCheck(context.Background()))

	bad := mustOpen(t, cfg{"url": "http://" + f.host, "insecure": true, "username": "pando"}, "wrong")
	err := bad.HealthCheck(context.Background())
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, f.host)
	require.NotContains(t, err.Error(), "wrong", "the password is never in the reason (R-194)")
}

// TestASingleRepositoryTagNeverExceedsTheRegistrysLimit asserts that the
// single layout's tag — app, workload and deployment in one — is cut to the
// 128 characters a registry accepts, keeping its unique end.
func TestASingleRepositoryTagNeverExceedsTheRegistrysLimit(t *testing.T) {
	t.Parallel()
	r, err := open(t, cfg{"url": "registry.internal/pando", "layout": "single"}, "")
	require.NoError(t, err)
	_, tag := r.Repository("app_"+strings.Repeat("a", 100), strings.Repeat("w", 40), "dep_01HQ9")
	require.LessOrEqual(t, len(tag), 128)
	require.True(t, strings.HasSuffix(tag, "-dep_01hq9"), "the deployment, which makes it unique, is kept")
	_, err = name.NewTag("registry.internal/pando:" + tag)
	require.NoError(t, err)
}
