package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR046_TheFirstAdminPasswordCanComeFromAFile asserts R-046's startup
// password with R-194: it may be given as a file, so it is in neither the
// Compose file nor `docker inspect`, and once read it is gone from the
// process environment either way.
func TestR046_TheFirstAdminPasswordCanComeFromAFile(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")
	dir := t.TempDir()
	file := filepath.Join(dir, "admin-password")
	require.NoError(t, os.WriteFile(file, []byte("correct horse battery\n"), 0o400))

	t.Setenv("PANDO_ADMIN_PASSWORD_FILE", file)
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "correct horse battery", cfg.Bootstrap.AdminPassword, "a trailing newline is not part of it")

	// What Compose mounts for a config whose content is unset: no password,
	// so the administrator is set up in the console.
	t.Setenv("PANDO_ADMIN_PASSWORD_FILE", dir)
	cfg, err = Load("")
	require.NoError(t, err)
	require.Empty(t, cfg.Bootstrap.AdminPassword)

	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, nil, 0o400))
	t.Setenv("PANDO_ADMIN_PASSWORD_FILE", empty)
	cfg, err = Load("")
	require.NoError(t, err)
	require.Empty(t, cfg.Bootstrap.AdminPassword)

	// A path that is not there is a typo, not a choice.
	t.Setenv("PANDO_ADMIN_PASSWORD_FILE", filepath.Join(dir, "missing"))
	_, err = Load("")
	require.ErrorContains(t, err, "does not exist")

	// Both is ambiguous.
	t.Setenv("PANDO_ADMIN_PASSWORD_FILE", file)
	t.Setenv("PANDO_ADMIN_PASSWORD", "hunter2hunter2")
	_, err = Load("")
	require.ErrorContains(t, err, "Set only one of them")
}

// TestR194_TheAdminPasswordIsNotLeftInTheEnvironment asserts that the plain
// variable still works, and is unset once read so a child process does not
// inherit it.
func TestR194_TheAdminPasswordIsNotLeftInTheEnvironment(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")
	t.Setenv("PANDO_ADMIN_PASSWORD", "hunter2hunter2")

	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "hunter2hunter2", cfg.Bootstrap.AdminPassword)
	_, still := os.LookupEnv("PANDO_ADMIN_PASSWORD")
	require.False(t, still, "read once, then unset")
}
