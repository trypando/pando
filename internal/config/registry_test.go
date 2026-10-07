package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR194_TheRegistryPasswordIsReadAndNeverReported asserts R-194 for the
// install registry's credential (issue #72, PR 5): it is read from the
// environment or from a file, and it is not among the settings GET /config
// reports, while the rest of the registry's settings are.
func TestR194_TheRegistryPasswordIsReadAndNeverReported(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")
	t.Setenv("PANDO_REGISTRY_URL", "https://registry.internal:5000")
	t.Setenv("PANDO_REGISTRY_USERNAME", "pando")
	t.Setenv("PANDO_REGISTRY_PASSWORD", "registry-secret-1")
	t.Setenv("PANDO_REGISTRY_INSECURE", "true")

	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "https://registry.internal:5000", cfg.Registry.URL)
	require.Equal(t, "pando", cfg.Registry.Username)
	require.True(t, cfg.Registry.Insecure)
	require.False(t, cfg.Registry.Always)
	require.Equal(t, "per_app", cfg.Registry.Layout)
	pw, err := cfg.Registry.Secret()
	require.NoError(t, err)
	require.Equal(t, "registry-secret-1", pw)

	for _, s := range cfg.Settings {
		require.NotEqual(t, "registry.password", s.Key)
		require.NotEqual(t, "registry-secret-1", s.Value)
	}
	require.Equal(t, Source{Kind: "env", Name: "PANDO_REGISTRY_URL"}, settingNamed(t, cfg, "registry.url").Source)

	file := filepath.Join(t.TempDir(), "registry-password")
	require.NoError(t, os.WriteFile(file, []byte("from-a-file\n"), 0o600))
	t.Setenv("PANDO_REGISTRY_PASSWORD_FILE", file)
	cfg, err = Load("")
	require.NoError(t, err)
	pw, err = cfg.Registry.Secret()
	require.NoError(t, err)
	require.Equal(t, "from-a-file", pw, "the file wins, without its trailing newline")

	cfg.Registry.PasswordFile = filepath.Join(t.TempDir(), "missing")
	_, err = cfg.Registry.Secret()
	require.ErrorContains(t, err, "PANDO_REGISTRY_PASSWORD_FILE")
}
