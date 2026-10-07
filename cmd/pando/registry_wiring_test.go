package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR271_TheStartupRegistryIsFixedFieldByFieldWithItsPasswordFromAFile
// asserts how main builds the install registry from startup configuration:
// each field the configuration sets is fixed, with where it was set, and the
// password may come from a file so it need not be in the environment.
func TestR271_TheStartupRegistryIsFixedFieldByFieldWithItsPasswordFromAFile(t *testing.T) {
	t.Parallel()
	passwordFile := filepath.Join(t.TempDir(), "registry-password")
	require.NoError(t, os.WriteFile(passwordFile, []byte("from-a-file\n"), 0o600))

	cfg := &config.Config{
		Registry: config.Registry{URL: "https://registry.internal:5000", Username: "pando",
			PasswordFile: passwordFile, Always: true},
		RegistrySet: map[string]config.Source{
			"url":      {Kind: "env", Name: "PANDO_REGISTRY_URL"},
			"username": {Kind: "env", Name: "PANDO_REGISTRY_USERNAME"},
			"password": {Kind: "env", Name: "PANDO_REGISTRY_PASSWORD_FILE"},
		},
	}
	svc, err := installRegistry(cfg, nil)
	require.NoError(t, err)
	require.Equal(t, imageregistry.Source{Kind: "env", Name: "PANDO_REGISTRY_URL"}, svc.Fixed["url"])
	require.Len(t, svc.Fixed, 3, "only what the startup configuration set")

	reg, err := svc.Current(context.Background())
	require.NoError(t, err)
	require.Equal(t, "registry.internal:5000", reg.Host())
	auth, err := reg.Auth(context.Background())
	require.NoError(t, err)
	require.Equal(t, "from-a-file", auth.Password.Reveal(), "read from the file, trimmed")

	cfg.Registry.PasswordFile = filepath.Join(t.TempDir(), "missing")
	_, err = installRegistry(cfg, nil)
	require.ErrorContains(t, err, "PANDO_REGISTRY_PASSWORD_FILE")
}

// TestR194_TheReconcilerGetsTheRegistryCredentialOnlyForImagesPandoPushed
// asserts the reconciler's view of the install registry: its credential for an
// image Pando pushed there, and nothing for any other image or when the
// credential cannot be had.
func TestR194_TheReconcilerGetsTheRegistryCredentialOnlyForImagesPandoPushed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const built = "registry.internal:5000/apps/app_01hq8@sha256:aaa"

	reg, err := imageregistry.New(imageregistry.Config{URL: "https://registry.internal:5000",
		Username: "pando", Password: secret.New("registry-password-9")})
	require.NoError(t, err)
	auth := builtImageAuth(imageregistry.Static(reg))
	require.Equal(t, "registry-password-9", auth(ctx, built).Password.Reveal())
	require.Nil(t, auth(ctx, "docker.io/library/redis:7"), "a published image is not given the install's credential")

	ecr, err := imageregistry.New(imageregistry.Config{URL: "https://registry.internal:5000", Kind: "ecr",
		Username: "AKIAEXAMPLE", Password: secret.New("ecr-secret")})
	require.NoError(t, err)
	require.Nil(t, builtImageAuth(imageregistry.Static(ecr))(ctx, built), "a credential that cannot be minted is none")

	require.Nil(t, builtImageAuth(failingRegistry{})(ctx, built), "settings that cannot be read give none")
	require.Nil(t, builtImageAuth(imageregistry.Static(nil))(ctx, built), "no registry, no credential")
}

type failingRegistry struct{}

func (failingRegistry) Current(context.Context) (*imageregistry.Registry, error) {
	return nil, errs.New(errs.Internal, "Pando could not read the install registry's settings.")
}
