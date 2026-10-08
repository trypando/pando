package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func registryDecl(t *testing.T, cfg *Config) AdapterDecl {
	t.Helper()
	for _, d := range cfg.Adapters {
		if d.ID == RegistryAdapterID {
			return d
		}
	}
	t.Fatal("no image registry adapter was declared")
	return AdapterDecl{}
}

// TestR271_PandoRegistryDeclaresAReadOnlyImageRegistryAdapter asserts R-271
// and R-252 for the startup shorthand (issue #153): PANDO_REGISTRY_* declares
// the install's image registry adapter, as the default, from the variable
// that set it — so the API refuses to change it and names that variable.
func TestR271_PandoRegistryDeclaresAReadOnlyImageRegistryAdapter(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")
	t.Setenv("PANDO_REGISTRY_URL", "https://registry.internal:5000")
	t.Setenv("PANDO_REGISTRY_USERNAME", "pando")
	t.Setenv("PANDO_REGISTRY_PASSWORD", "registry-secret-1")
	t.Setenv("PANDO_REGISTRY_INSECURE", "true")

	cfg, err := Load("")
	require.NoError(t, err)
	d := registryDecl(t, cfg)
	require.Equal(t, "image_registry", d.Category)
	require.Equal(t, "oci", d.Kind)
	require.True(t, d.Default)
	require.True(t, d.Enabled)
	require.Equal(t, Source{Kind: "env", Name: "PANDO_REGISTRY_URL"}, d.Source)
	require.Equal(t, map[string]any{"url": "https://registry.internal:5000", "username": "pando", "insecure": true}, d.Config)
	require.Equal(t, map[string]CredentialRef{"password": {Env: "PANDO_REGISTRY_PASSWORD"}}, d.Credentials)

	t.Setenv("PANDO_REGISTRY_URL", "")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Empty(t, cfg.Adapters, "no address, no registry")
}

// TestR194_TheRegistryPasswordIsReadAndNeverReported asserts R-194 for the
// image registry's credential: the declaration says where it is read from —
// the environment or a file — and never holds it, and it is not among the
// settings GET /config reports.
func TestR194_TheRegistryPasswordIsReadAndNeverReported(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")
	t.Setenv("PANDO_REGISTRY_URL", "https://registry.internal:5000")
	t.Setenv("PANDO_REGISTRY_USERNAME", "pando")
	t.Setenv("PANDO_REGISTRY_PASSWORD", "registry-secret-1")

	cfg, err := Load("")
	require.NoError(t, err)
	for _, s := range cfg.Settings {
		require.NotEqual(t, "registry.password", s.Key)
		require.NotEqual(t, "registry-secret-1", s.Value)
	}
	require.Equal(t, Source{Kind: "env", Name: "PANDO_REGISTRY_URL"}, settingNamed(t, cfg, "registry.url").Source)
	require.NotContains(t, registryDecl(t, cfg).Config, "password")

	file := filepath.Join(t.TempDir(), "registry-password")
	require.NoError(t, os.WriteFile(file, []byte("from-a-file\n"), 0o600))
	t.Setenv("PANDO_REGISTRY_PASSWORD_FILE", file)
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, CredentialRef{File: file}, registryDecl(t, cfg).Credentials["password"], "the file wins")
}

// TestR190_ECRIsDeclaredWithItsAccessKey asserts that PANDO_REGISTRY_KIND=ecr
// declares the ECR kind, with the access key ID as a setting and its secret
// as a credential read from the environment.
func TestR190_ECRIsDeclaredWithItsAccessKey(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")
	t.Setenv("PANDO_REGISTRY_URL", "123456789012.dkr.ecr.us-east-1.amazonaws.com/pando")
	t.Setenv("PANDO_REGISTRY_KIND", "ecr")
	t.Setenv("PANDO_REGISTRY_USERNAME", "AKIAEXAMPLE")
	t.Setenv("PANDO_REGISTRY_PASSWORD", "aws-secret")
	t.Setenv("PANDO_REGISTRY_ALWAYS", "true")

	cfg, err := Load("")
	require.NoError(t, err)
	d := registryDecl(t, cfg)
	require.Equal(t, "ecr", d.Kind)
	require.Equal(t, map[string]any{"url": "123456789012.dkr.ecr.us-east-1.amazonaws.com/pando",
		"access_key_id": "AKIAEXAMPLE", "always": true}, d.Config)
	require.Equal(t, map[string]CredentialRef{"secret_access_key": {Env: "PANDO_REGISTRY_PASSWORD"}}, d.Credentials)

	t.Setenv("PANDO_REGISTRY_KIND", "token")
	_, err = Load("")
	require.ErrorContains(t, err, "PANDO_REGISTRY_KIND is \"token\"")
}

// TestR271_ARegistryDeclaredTwiceStopsStartup asserts that a registry
// declared by PANDO_REGISTRY_URL and again as the default under adapters:
// stops startup naming both, rather than one silently winning; and that a
// password written into the config file is refused (R-190).
func TestR271_ARegistryDeclaredTwiceStopsStartup(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")
	dir := t.TempDir()
	path := filepath.Join(dir, "pando.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`adapters:
  reg_harbor:
    category: image_registry
    kind: oci
    default: true
    config:
      url: https://harbor.internal/pando
`), 0o600))
	t.Setenv("PANDO_REGISTRY_URL", "https://registry.internal:5000")
	_, err := Load(path)
	require.ErrorContains(t, err, "PANDO_REGISTRY_URL declares the install's image registry")
	require.ErrorContains(t, err, "adapters.reg_harbor")

	t.Setenv("PANDO_REGISTRY_URL", "")
	cfg, err := Load(path)
	require.NoError(t, err, "an image registry can be declared in the file like any adapter")
	require.Equal(t, "image_registry", cfg.Adapters[0].Category)

	inline := filepath.Join(dir, "inline.yaml")
	require.NoError(t, os.WriteFile(inline, []byte("registry:\n  url: https://registry.internal:5000\n  username: pando\n  password: hunter2\n"), 0o600))
	_, err = Load(inline)
	require.ErrorContains(t, err, "does not read a credential from the config file")
	require.NotContains(t, err.Error(), "hunter2")
}
