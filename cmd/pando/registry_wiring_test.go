package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	registryecr "github.com/trypando/pando/internal/adapter/imageregistry/ecr"
	registryoci "github.com/trypando/pando/internal/adapter/imageregistry/oci"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/errs"
)

// ociRegistry configures an OCI image registry adapter for a test.
func ociRegistry(t *testing.T, settings map[string]any, password string) *registryoci.Adapter {
	t.Helper()
	if password != "" {
		settings["credentials"] = map[string]string{"password": password}
	}
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	a := registryoci.New()
	require.NoError(t, a.Configure(context.Background(), raw))
	return a
}

// TestR271_TheDeclaredRegistryIsBuiltWithItsPasswordFromAFile asserts how
// main builds the image registry PANDO_REGISTRY_* declares: as an adapter of
// the image_registry category, configured with the password read from the
// file the declaration names, and the one builds are pushed to.
func TestR271_TheDeclaredRegistryIsBuiltWithItsPasswordFromAFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	passwordFile := filepath.Join(t.TempDir(), "registry-password")
	require.NoError(t, os.WriteFile(passwordFile, []byte("from-a-file\n"), 0o600))
	decl := config.AdapterDecl{ID: config.RegistryAdapterID, Category: string(adapterapi.CategoryImageRegistry), Kind: registryoci.Kind,
		Default: true, Enabled: true,
		Config:      map[string]any{"url": "https://registry.internal:5000", "username": "pando", "always": true},
		Credentials: map[string]config.CredentialRef{"password": {File: passwordFile}}}

	a := newAdapter(decl.Category, decl.Kind, nil)
	require.NotNil(t, a)
	creds, err := declaredCredentials(decl.Credentials)
	require.NoError(t, err)
	raw, err := json.Marshal(decl.Config)
	require.NoError(t, err)
	raw, err = withCredentials(raw, creds)
	require.NoError(t, err)
	require.NoError(t, a.Configure(ctx, raw))
	registry := adapterapi.NewRegistry()
	require.NoError(t, registry.Register(decl.ID, a))
	require.NoError(t, registry.SetDefault(adapterapi.CategoryImageRegistry, decl.ID))

	svc := &imageregistry.Service{New: newImageRegistryAdapter, Declared: declaredImageRegistries(registry)}
	reg, err := svc.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, config.RegistryAdapterID, reg.ID())
	require.Equal(t, "registry.internal:5000", reg.Host())
	require.True(t, reg.Always())
	auth, err := reg.Auth(ctx)
	require.NoError(t, err)
	require.Equal(t, "from-a-file", auth.Password.Reveal(), "read from the file, trimmed")

	require.Nil(t, newAdapter(string(adapterapi.CategoryImageRegistry), "nexus", nil), "an unknown kind is nil, not a typed nil")
}

// TestR194_TheReconcilerGetsTheRegistryCredentialOnlyForImagesPandoPushed
// asserts the reconciler's view of the install registry: its credential for an
// image Pando pushed there, and nothing for any other image or when the
// credential cannot be had.
func TestR194_TheReconcilerGetsTheRegistryCredentialOnlyForImagesPandoPushed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const built = "registry.internal:5000/apps/app_01hq8@sha256:aaa"

	reg := imageregistry.Of("reg_1", ociRegistry(t, map[string]any{"url": "https://registry.internal:5000", "username": "pando"}, "registry-password-9"))
	auth := builtImageAuth(imageregistry.Static(reg))
	require.Equal(t, "registry-password-9", auth(ctx, built).Password.Reveal())
	require.Nil(t, auth(ctx, "docker.io/library/redis:7"), "a published image is not given the install's credential")

	ecr := registryecr.New()
	ecr.Endpoint = "http://127.0.0.1:1"
	require.NoError(t, ecr.Configure(ctx, json.RawMessage(`{"url":"123456789012.dkr.ecr.us-east-1.amazonaws.com/pando",`+
		`"access_key_id":"AKIAEXAMPLE","credentials":{"secret_access_key":"ecr-secret"}}`)))
	require.Nil(t, builtImageAuth(imageregistry.Static(imageregistry.Of("reg_ecr", ecr)))(ctx,
		"123456789012.dkr.ecr.us-east-1.amazonaws.com/pando@sha256:aaa"), "a credential that cannot be minted is none")

	require.Nil(t, builtImageAuth(failingRegistry{})(ctx, built), "settings that cannot be read give none")
	require.Nil(t, builtImageAuth(imageregistry.Static(nil))(ctx, built), "no registry, no credential")
}

type failingRegistry struct{}

func (failingRegistry) Current(context.Context) (*imageregistry.Registry, error) {
	return nil, errs.New(errs.Internal, "Pando could not read the install registry's settings.")
}
