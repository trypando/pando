package imageregistry

import (
	"context"
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

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

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
func pushTo(t *testing.T, r *Registry, app, dep string, opts ...remote.Option) {
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
	r, err := New(Config{URL: "http://" + f.host, Insecure: true})
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
	r, err := New(Config{URL: "http://" + f.host, Insecure: true})
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
	r, err := New(Config{URL: "http://" + f.host, Insecure: true})
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
	r, err := New(Config{URL: "http://" + f.host + "/pando", Insecure: true, Username: "pando", Password: secret.New("s3cret")})
	require.NoError(t, err)
	auth, err := r.Auth(context.Background())
	require.NoError(t, err)
	pushTo(t, r, "app_1", "dep_1", remote.WithAuth(&authn.Basic{Username: auth.Username, Password: auth.Password.Reveal()}))

	n, err := r.DeleteApp(context.Background(), "app_1", nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	anonymous, err := New(Config{URL: "http://" + f.host + "/pando", Insecure: true})
	require.NoError(t, err)
	_, err = anonymous.DeleteApp(context.Background(), "app_1", nil)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err), "without the credential the registry refuses")
}

// TestAnECRCredentialForARegistryThatIsNotECRIsRefusedBeforeAnyPush asserts
// that a credential the registry cannot use fails the push target and the
// teardown with the resolver's own explanation, rather than an anonymous push.
func TestAnECRCredentialForARegistryThatIsNotECRIsRefusedBeforeAnyPush(t *testing.T) {
	t.Parallel()
	r, err := New(Config{URL: "https://registry.internal", Kind: "ecr", Username: "AKIAEXAMPLE", Password: secret.New("key")})
	require.NoError(t, err)

	_, err = r.Target(context.Background(), "app_1", "", "dep_1")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "not an ECR registry")

	_, err = r.DeleteApp(context.Background(), "app_1", nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))

	_, err = New(Config{URL: "https://registry.internal", Kind: "ecr"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "ECR always needs its access key")
}

// TestARegistryAddressPandoCannotUseIsRefusedAtStartup asserts each startup
// refusal for an address: a credential in the URL (never quoted back, R-194),
// no host, and a path no repository can have.
func TestARegistryAddressPandoCannotUseIsRefusedAtStartup(t *testing.T) {
	t.Parallel()
	_, err := New(Config{URL: "https://pando:hunter2@registry.internal"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.NotContains(t, errs.As(err).Message, "hunter2")

	_, err = New(Config{URL: "https://"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "names no registry host")

	_, err = New(Config{URL: "registry.internal/pando/bad path!"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "lowercase path")

	var none *Registry
	require.Empty(t, none.Host())
	_, err = none.Target(context.Background(), "app_1", "", "dep_1")
	require.Equal(t, errs.Internal, errs.CodeOf(err))

	always, err := New(Config{URL: "registry.internal:5000", Always: true})
	require.NoError(t, err)
	require.True(t, always.Always())
	require.Equal(t, "registry.internal:5000", always.Host())
}

// TestASingleRepositoryTagNeverExceedsTheRegistrysLimit asserts that the
// single layout's tag — app, workload and deployment in one — is cut to the
// 128 characters a registry accepts, keeping its unique end.
func TestASingleRepositoryTagNeverExceedsTheRegistrysLimit(t *testing.T) {
	t.Parallel()
	r, err := New(Config{URL: "registry.internal/pando", Layout: "single"})
	require.NoError(t, err)
	_, tag := r.Repository("app_"+strings.Repeat("a", 100), strings.Repeat("w", 40), "dep_01HQ9")
	require.LessOrEqual(t, len(tag), 128)
	require.True(t, strings.HasSuffix(tag, "-dep_01hq9"), "the deployment, which makes it unique, is kept")
	_, err = name.NewTag("registry.internal/pando:" + tag)
	require.NoError(t, err)
}
