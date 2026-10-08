//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/config"
)

// TestR252_AnImageRegistryIsConfiguredLikeAnyAdapterAndUsedWithoutARestart
// asserts R-252 as amended by issue #153, and R-190 and R-194 for its
// password: the install's image registry is configured with POST /adapters
// like any adapter, is in use from the moment it is saved — no restart, never
// pending one — is listed with its capabilities, and its password is never
// read back or written to the audit log.
func TestR252_AnImageRegistryIsConfiguredLikeAnyAdapterAndUsedWithoutARestart(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	added := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "reg_main", "category": "image_registry", "kind": "oci", "name": "Registry",
		"config":      map[string]any{"url": "https://127.0.0.1:1/pando", "username": "pando"},
		"credentials": map[string]string{"password": "registry-do-not-leak"},
	})
	require.Equal(t, http.StatusCreated, added.Code, added.String())
	require.NotContains(t, added.String(), "registry-do-not-leak")
	require.Contains(t, added.String(), "in use from now on")
	require.NotContains(t, added.String(), "restart")

	listed := i.do(admin, http.MethodGet, "/adapters", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.String())
	require.NotContains(t, listed.String(), "registry-do-not-leak")
	var body struct {
		Adapters []map[string]any `json:"adapters"`
	}
	listed.JSON(t, &body)
	var entry map[string]any
	for _, a := range body.Adapters {
		if a["id"] == "reg_main" {
			entry = a
		}
	}
	require.NotNil(t, entry, listed.String())
	require.Nil(t, entry["pending_restart"], "built from its row each time it is used")
	require.Equal(t, []any{"password"}, entry["credentials_set"])
	caps, ok := entry["capabilities"].(map[string]any)
	require.True(t, ok, listed.String())
	require.Equal(t, "127.0.0.1:1", caps["host"])
	require.Equal(t, true, caps["creates_repositories_on_push"])
	require.Equal(t, "unreachable", entry["status"], "nothing listens there, and the health check signs in")

	audit := i.do(admin, http.MethodGet, "/audit?action=adapter.configure", nil)
	require.Equal(t, http.StatusOK, audit.Code, audit.String())
	require.Contains(t, audit.String(), "reg_main")
	require.NotContains(t, audit.String(), "registry-do-not-leak")

	require.Equal(t, http.StatusForbidden, i.do(i.user("crewmate"), http.MethodPost, "/adapters", map[string]any{
		"id": "reg_evil", "category": "image_registry", "kind": "oci", "config": map[string]any{"url": "https://evil.example"},
	}).Code, "configuring one needs install.adapters.manage")
}

// TestR105_AnImageRegistryPandoCannotUseIsRefusedBeforeItIsSaved asserts
// that a registry the adapter refuses — plain HTTP not allowed, half a
// credential — is refused by POST /adapters with the adapter's reason and is
// not saved, so it never becomes what the next build pushes to.
func TestR105_AnImageRegistryPandoCannotUseIsRefusedBeforeItIsSaved(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	plain := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "reg_plain", "category": "image_registry", "kind": "oci", "config": map[string]any{"url": "http://registry.internal:5000"},
	})
	require.Equal(t, http.StatusBadRequest, plain.Code, plain.String())
	require.Contains(t, plain.String(), "plain HTTP")
	require.Contains(t, plain.String(), "Allow plain HTTP")

	half := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "reg_half", "category": "image_registry", "kind": "oci",
		"config": map[string]any{"url": "https://registry.internal:5000", "username": "pando"},
	})
	require.Equal(t, http.StatusBadRequest, half.Code, half.String())
	require.Contains(t, half.String(), "both a username and a password")

	listed := i.do(admin, http.MethodGet, "/adapters", nil)
	require.NotContains(t, listed.String(), "reg_plain")
	require.NotContains(t, listed.String(), "reg_half")

	// One being turned off is saved whatever its settings: it will not be used.
	off := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "reg_off", "category": "image_registry", "kind": "oci", "enabled": false,
		"config": map[string]any{"url": "http://registry.internal:5000"},
	})
	require.Equal(t, http.StatusCreated, off.Code, off.String())
}

// TestR271_ARegistryDeclaredByTheEnvironmentIsReadOnlyAndSaysWhere asserts
// R-271 for the image registry PANDO_REGISTRY_URL declares: it is listed as
// declared, and a change to it is refused naming the variable.
func TestR271_ARegistryDeclaredByTheEnvironmentIsReadOnlyAndSaysWhere(t *testing.T) {
	t.Parallel()
	i := newInstallWith(t, nil, &config.Config{Adapters: []config.AdapterDecl{{
		ID: config.RegistryAdapterID, Category: "image_registry", Kind: "oci", Name: "Image registry",
		Default: true, Enabled: true, Source: config.Source{Kind: "env", Name: "PANDO_REGISTRY_URL"},
		Config: map[string]any{"url": "https://registry.internal:5000"},
	}}})
	admin := i.admin()

	refused := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": config.RegistryAdapterID, "category": "image_registry", "kind": "oci",
		"config": map[string]any{"url": "https://elsewhere.internal"},
	})
	require.Equal(t, http.StatusConflict, refused.Code, refused.String())
	require.Contains(t, refused.String(), "environment variable PANDO_REGISTRY_URL")

	listed := i.do(admin, http.MethodGet, "/adapters", nil)
	require.Contains(t, listed.String(), `"declared":true`)
	require.Contains(t, listed.String(), "PANDO_REGISTRY_URL")
}
