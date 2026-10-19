package oci_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/imageregistry/oci"
	"github.com/trypando/pando/internal/adapter/imageregistry/registrykit"
	"github.com/trypando/pando/internal/errs"
)

// fakeGitHub is GitHub's packages API for one token: packages by request
// path, the token's login, and the scopes it reports.
type fakeGitHub struct {
	*httptest.Server
	mu       sync.Mutex
	login    string
	scopes   *string
	versions map[string]string // escaped list path -> JSON body
	refuse   int               // status for every DELETE, when set
	deleted  []string
}

func newFakeGitHub(t *testing.T, login string) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{login: login, versions: map[string]string{}}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer ghp_token", r.Header.Get("Authorization"), "the registry's own token")
		g.mu.Lock()
		defer g.mu.Unlock()
		path := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodGet && path == "/user":
			if g.scopes != nil {
				w.Header().Set("X-OAuth-Scopes", *g.scopes)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"login": g.login})
		case r.Method == http.MethodGet:
			body, ok := g.versions[path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodDelete:
			if g.refuse != 0 {
				w.WriteHeader(g.refuse)
				return
			}
			g.deleted = append(g.deleted, path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func openGHCR(t *testing.T, gh *fakeGitHub, settings map[string]any) *oci.Adapter {
	t.Helper()
	settings["username"] = "pando"
	settings["credentials"] = map[string]string{"password": "ghp_token"}
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	a := oci.New()
	a.GitHubAPI = gh.URL
	require.NoError(t, a.Configure(context.Background(), raw))
	return a
}

// TestR224_GHCRImagesAreDeletedThroughGitHubsPackagesAPI asserts that a
// deleted app's images on GitHub Container Registry, which refuses a
// manifest DELETE, are deleted through GitHub's packages API with the
// registry's token: an app's own repository is its own package, deleted
// whole; a package that was never pushed is nothing to delete.
func TestR224_GHCRImagesAreDeletedThroughGitHubsPackagesAPI(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t, "someone-else")
	gh.versions["/orgs/acme/packages/container/pando%2Fapps%2Fapp_1/versions"] =
		`[{"id":11,"name":"sha256:aa","metadata":{"container":{"tags":["dep_1"]}}},{"id":12,"name":"sha256:bb","metadata":{"container":{"tags":[]}}}]`
	a := openGHCR(t, gh, cfg{"url": "https://registry.test/acme/pando"})

	n, err := a.DeleteApp(context.Background(), "app_1", []string{"api"})
	require.NoError(t, err)
	require.Equal(t, 2, n, "every version of the app's package")
	require.Equal(t, []string{"/orgs/acme/packages/container/pando%2Fapps%2Fapp_1"}, gh.deleted,
		"the package, its name's slashes escaped; the never-pushed api package is left alone")
}

// TestR224_InOneGHCRRepositoryOnlyTheAppsVersionsAreDeleted asserts the
// single layout on a user's own GHCR: the token's user owns the package, and
// only versions tagged for the deleted app are deleted.
func TestR224_InOneGHCRRepositoryOnlyTheAppsVersionsAreDeleted(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t, "Me")
	gh.versions["/user/packages/container/pando/versions"] = `[
		{"id":1,"name":"sha256:a","metadata":{"container":{"tags":["app_1-dep_1"]}}},
		{"id":2,"name":"sha256:b","metadata":{"container":{"tags":["app_2-dep_2"]}}},
		{"id":3,"name":"sha256:c","metadata":{"container":{"tags":["app_1-api-dep_3"]}}}]`
	a := openGHCR(t, gh, cfg{"url": "https://registry.test/me/pando", "layout": "single"})

	n, err := a.DeleteApp(context.Background(), "app_1", nil)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, []string{"/user/packages/container/pando/versions/1", "/user/packages/container/pando/versions/3"}, gh.deleted)
}

// TestR105_GHCRRefusingADeleteSaysWhichScopeToAdd asserts that a token GitHub
// will not let delete fails with the scope to add, never the token (R-194).
func TestR105_GHCRRefusingADeleteSaysWhichScopeToAdd(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t, "x")
	gh.versions["/orgs/acme/packages/container/apps%2Fapp_1/versions"] = `[{"id":1,"name":"sha256:a","metadata":{"container":{"tags":["dep"]}}}]`
	gh.refuse = http.StatusForbidden
	a := openGHCR(t, gh, cfg{"url": "https://registry.test/acme"})

	_, err := a.DeleteApp(context.Background(), "app_1", nil)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "delete:packages")
	require.NotContains(t, err.Error(), "ghp_token")

	gh.refuse = http.StatusBadRequest
	_, err = a.DeleteApp(context.Background(), "app_1", nil)
	require.Contains(t, errs.As(err).Message, "last tagged version")
}

// TestAGHCRAdapterWithNoTokenCannotDelete asserts that deleting on GHCR
// without a token says what to set rather than trying anonymously.
func TestAGHCRAdapterWithNoTokenCannotDelete(t *testing.T) {
	t.Parallel()
	a := oci.New()
	a.GitHubAPI = "http://127.0.0.1:1"
	require.NoError(t, a.Configure(context.Background(), json.RawMessage(`{"url":"https://registry.test/acme"}`)))
	_, err := a.DeleteApp(context.Background(), "app_1", nil)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "delete:packages")
}

// TestR254_TheHealthCheckNotesAGHCRTokenThatCannotDelete asserts that a
// classic token without delete:packages is a note, not unreachable, and a
// token that has it, or a fine-grained token that reports no scopes, is not.
func TestR254_TheHealthCheckNotesAGHCRTokenThatCannotDelete(t *testing.T) {
	t.Parallel()
	f := newFronted(t)
	gh := newFakeGitHub(t, "x")
	a := openGHCR(t, gh, cfg{"url": "http://" + f.host + "/acme", "insecure": true})

	scopes := "repo, write:packages"
	gh.scopes = &scopes
	notes, err := a.Health(context.Background())
	require.NoError(t, err, "pushing and pulling still work")
	require.Len(t, notes, 1)
	require.Contains(t, notes[0], "delete:packages")
	require.NoError(t, a.HealthCheck(context.Background()))

	scopes = "write:packages, delete:packages"
	notes, err = a.Health(context.Background())
	require.NoError(t, err)
	require.Empty(t, notes)

	gh.scopes = nil
	notes, err = a.Health(context.Background())
	require.NoError(t, err)
	require.Empty(t, notes, "a token that reports no scopes says nothing either way")
}

// TestR254_TheHealthCheckNotesARegistryThatWillNotDelete asserts the delete
// probe: a registry that refuses the credential's delete, or deletes
// altogether, is a note rather than unreachable; one that answers not found
// permits it; and the probe touches no image the registry holds.
func TestR254_TheHealthCheckNotesARegistryThatWillNotDelete(t *testing.T) {
	t.Parallel()
	f := newFronted(t)
	for _, layout := range []string{"per_app", "single"} {
		a := mustOpen(t, cfg{"url": "http://" + f.host + "/pando", "insecure": true, "layout": layout}, "")
		pushTo(t, a, "app_1", "dep_1")

		var probed []string
		var mu sync.Mutex
		answer := 0
		f.refuse(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != http.MethodDelete {
				return false
			}
			mu.Lock()
			probed = append(probed, r.URL.Path)
			mu.Unlock()
			switch answer {
			case http.StatusMethodNotAllowed:
				registryError(w, answer, "UNSUPPORTED")
			case http.StatusForbidden:
				registryError(w, answer, "DENIED")
			default:
				return false
			}
			return true
		})

		notes, err := a.Health(context.Background())
		require.NoError(t, err)
		require.Empty(t, notes, layout+": a not-found answer means deletes are permitted")

		answer = http.StatusMethodNotAllowed
		notes, err = a.Health(context.Background())
		require.NoError(t, err)
		require.Len(t, notes, 1)
		require.Contains(t, notes[0], "storage.delete.enabled")

		answer = http.StatusForbidden
		notes, err = a.Health(context.Background())
		require.NoError(t, err)
		require.Len(t, notes, 1)
		require.Contains(t, notes[0], "permission to delete")
		require.NoError(t, a.HealthCheck(context.Background()), "a note is not unreachable")

		for _, p := range probed {
			require.True(t, strings.HasSuffix(p, "/manifests/sha256:"+strings.Repeat("0", 64)), p)
		}
		repo, tag := a.Repository("app_1", "", "dep_1")
		ref, err := name.NewTag(repo+":"+tag, name.Insecure)
		require.NoError(t, err)
		_, err = remote.Head(ref)
		require.NoError(t, err, "the probe deleted nothing real")
		f.refuse(func(http.ResponseWriter, *http.Request) bool { return false })
	}
}

// TestAProvidersOwnDeleteRemedyIsTheNote asserts that a provider which
// names what its credential needs to delete, as ECR does, gets that in the
// note rather than advice for a registry Pando runs.
func TestAProvidersOwnDeleteRemedyIsTheNote(t *testing.T) {
	t.Parallel()
	f := newFronted(t)
	reg, err := registrykit.Open(registrykit.Settings{
		URL: "http://" + f.host + "/pando", Insecure: true, Layout: "single",
		DeleteRemedy: "Allow ecr:BatchDeleteImage.",
	}, false, nil)
	require.NoError(t, err)
	for _, answer := range []int{http.StatusMethodNotAllowed, http.StatusForbidden} {
		f.refuse(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != http.MethodDelete {
				return false
			}
			registryError(w, answer, "DENIED")
			return true
		})
		notes, err := reg.Health(context.Background())
		require.NoError(t, err)
		require.Len(t, notes, 1)
		require.Contains(t, notes[0], "Allow ecr:BatchDeleteImage.")
		require.NotContains(t, notes[0], "storage.delete.enabled")
	}
}

// TestSavingARegistrySaysWhatWillNotWork asserts that saving one returns
// registrykit's warnings.
func TestSavingARegistrySaysWhatWillNotWork(t *testing.T) {
	t.Parallel()
	require.NotEmpty(t, oci.Info().Warn(json.RawMessage(`{"url":"ghcr.io"}`)))
}

// TestDockerHubIsOneRepositoryOrRefused asserts that Docker Hub, whose
// repositories are one namespace and one name, defaults to one repository,
// refuses a repository per app when configured rather than at the first
// push, and refuses an address that is not namespace/name (R-105).
func TestDockerHubIsOneRepositoryOrRefused(t *testing.T) {
	t.Parallel()
	a, err := open(t, cfg{"url": "docker.io/acme/pando"}, "")
	require.NoError(t, err)
	require.False(t, a.ImageRegistryCapabilities().RepositoryPerApp)
	repo, _ := a.Repository("app_1", "", "dep_1")
	require.Equal(t, "docker.io/acme/pando", repo)

	_, err = open(t, cfg{"url": "https://docker.io/acme/pando", "layout": "per_app"}, "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "cannot hold a repository per app")
	require.Contains(t, errs.As(err).Remedy, "layout single")

	for _, url := range []string{"docker.io/acme", "registry-1.docker.io", "docker.io/acme/pando/more"} {
		_, err = open(t, cfg{"url": url}, "")
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), url)
		require.Contains(t, errs.As(err).Remedy, "docker.io/acme/pando", url)
	}
}
