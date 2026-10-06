//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// An image app can be created with the credential its image is pulled with,
// and nothing reads the secret part of it back (R-194, issue #41).
func TestAnImageAppIsCreatedWithARegistryCredentialNobodyCanReadBack(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	created := i.do(admin, http.MethodPost, "/apps", map[string]any{
		"name": "private-web",
		"source": map[string]any{
			"type": "image", "image": "ghcr.io/acme/private:1",
			"credential": map[string]string{"kind": "basic", "username": "ben", "password": "ghp_do-not-leak"},
		},
	})
	require.Equal(t, http.StatusAccepted, created.Code, created.String())
	require.NotContains(t, created.String(), "ghp_do-not-leak")
	var app struct {
		ID     string `json:"id"`
		Source struct {
			CredentialRef string `json:"credential_ref"`
		} `json:"source"`
	}
	created.JSON(t, &app)
	require.Equal(t, "registry", app.Source.CredentialRef, "the spec says the pull is authenticated, not with what")

	got := i.do(admin, http.MethodGet, "/apps/"+app.ID+"/registry-credential", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.NotContains(t, got.String(), "ghp_do-not-leak")
	var summary struct {
		Set        bool `json:"set"`
		Credential struct {
			Kind     string `json:"kind"`
			Username string `json:"username"`
		} `json:"credential"`
	}
	got.JSON(t, &summary)
	require.True(t, summary.Set)
	require.Equal(t, "basic", summary.Credential.Kind)
	require.Equal(t, "ben", summary.Credential.Username)

	// Nor is it an app secret, where an env entry could hand it to the app.
	secrets := i.do(admin, http.MethodGet, "/apps/"+app.ID+"/secrets", nil)
	require.NotContains(t, secrets.String(), "password")

	// Replaced with AWS keys, nothing of the token is left.
	replaced := i.do(admin, http.MethodPut, "/apps/"+app.ID+"/registry-credential", map[string]string{
		"kind": "ecr", "access_key_id": "AKIAEXAMPLE", "secret_access_key": "aws-do-not-leak",
	})
	require.Equal(t, http.StatusNoContent, replaced.Code, replaced.String())
	got = i.do(admin, http.MethodGet, "/apps/"+app.ID+"/registry-credential", nil)
	require.Contains(t, got.String(), `"kind":"ecr"`)
	require.NotContains(t, got.String(), "ben")
	require.NotContains(t, got.String(), "aws-do-not-leak")

	removed := i.do(admin, http.MethodDelete, "/apps/"+app.ID+"/registry-credential", nil)
	require.Equal(t, http.StatusNoContent, removed.Code, removed.String())
	got = i.do(admin, http.MethodGet, "/apps/"+app.ID+"/registry-credential", nil)
	require.JSONEq(t, `{"set":false}`, got.String())
}

// A bad credential refuses the request rather than creating an app without it.
func TestAnIncompleteRegistryCredentialRefusesTheApp(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	refused := i.do(admin, http.MethodPost, "/apps", map[string]any{
		"name": "half",
		"source": map[string]any{
			"type": "image", "image": "ghcr.io/acme/private:1",
			"credential": map[string]string{"kind": "basic", "username": "ben"},
		},
	})
	require.Equal(t, string(errs.ValidInvalid), refused.ErrorCode(), refused.String())

	forGit := i.do(admin, http.MethodPost, "/apps", map[string]any{
		"name": "git-with-registry",
		"source": map[string]any{
			"type": "git", "url": "https://github.com/acme/notes",
			"credential": map[string]string{"kind": "basic", "username": "ben", "password": "x"},
		},
	})
	require.Equal(t, string(errs.ValidInvalid), forGit.ErrorCode(), forGit.String())

	apps := i.do(admin, http.MethodGet, "/apps", nil)
	require.NotContains(t, apps.String(), `"half"`)

	unknown := i.do(admin, http.MethodPost, "/apps", map[string]any{
		"name": "ftp", "source": map[string]any{"type": "ftp", "url": "ftp://example.com"},
	})
	require.Equal(t, string(errs.ValidInvalid), unknown.ErrorCode(), unknown.String())
}

// TestR092_AnImageOutsideTheAllowlistIsRefusedAtCreation asserts R-092 at the
// API for images: before any pull, an image from a registry or namespace the
// allowlist does not name is refused. It used to be admitted, because the
// check was handed the source's URL and an image has none (issue #41).
func TestR092_AnImageOutsideTheAllowlistIsRefusedAtCreation(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	saved := i.do(admin, http.MethodPut, "/policy", map[string]any{
		"source_allowlist": []string{"github.com", "ghcr.io/acme"},
	})
	require.Equal(t, http.StatusOK, saved.Code, saved.String())

	blocked := i.do(admin, http.MethodPost, "/apps", map[string]any{
		"name":   "stranger",
		"source": map[string]string{"type": "image", "image": "docker.io/stranger/miner:latest"},
	})
	require.Equal(t, string(errs.PolicySourceNotAllowed), blocked.ErrorCode(), blocked.String())

	allowed := i.do(admin, http.MethodPost, "/apps", map[string]any{
		"name":   "ours",
		"source": map[string]string{"type": "image", "image": "ghcr.io/acme/web:1"},
	})
	require.Equal(t, http.StatusAccepted, allowed.Code, allowed.String())

	// An upload names no host, so this allowlist does not admit one.
	upload := i.do(admin, http.MethodPost, "/apps", map[string]any{
		"name": "from-laptop", "source": map[string]string{"type": "upload"},
	})
	require.Equal(t, string(errs.PolicySourceNotAllowed), upload.ErrorCode(), upload.String())
}
