package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

// TestR401_TheDatabasePasswordIsReadFromAFile asserts R-401 on Pando's side:
// the bundled install gives Pando its database password as a file, never in
// PANDO_DATABASE_URL, and Load puts it into the URL it connects with.
func TestR401_TheDatabasePasswordIsReadFromAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "postgres-password")
	require.NoError(t, os.WriteFile(file, []byte("s3cret/with+chars\n"), 0o400))

	t.Setenv("PANDO_DATABASE_URL", "postgres://pando@postgres:5432/pando?sslmode=disable")
	t.Setenv("PANDO_DATABASE_PASSWORD_FILE", file)
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "postgres://pando:s3cret%2Fwith+chars@postgres:5432/pando?sslmode=disable", cfg.Database.URL,
		"the file's password, escaped for a URL, without its trailing newline")
}

func TestADatabasePasswordFileThatCannotBeUsedIsRefused(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, []byte("\n"), 0o400))
	full := filepath.Join(dir, "full")
	require.NoError(t, os.WriteFile(full, []byte("pw"), 0o400))

	for _, c := range []struct{ url, file, want string }{
		{"postgres://pando@postgres/pando", filepath.Join(dir, "missing"), "could not read"},
		{"postgres://pando@postgres/pando", empty, "is empty"},
		{"postgres://postgres/pando", full, "does not name a user"},
		{"postgres://pando:inline@postgres/pando", full, "Set only one of them"},
	} {
		t.Setenv("PANDO_DATABASE_URL", c.url)
		t.Setenv("PANDO_DATABASE_PASSWORD_FILE", c.file)
		_, err := Load("")
		require.ErrorContains(t, err, c.want, c.url)
		require.NotContains(t, err.Error(), "inline", "the error never repeats a password from the URL")
	}
}

// TestR401_TheBundledInstallHasNoDefaultDatabasePassword asserts R-401 on the
// Compose file that ships: Postgres and Pando both take the password from the
// file the secrets service makes, and neither names one in the file itself.
func TestR401_TheBundledInstallHasNoDefaultDatabasePassword(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docker-compose.yml"))
	require.NoError(t, err)
	var compose struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			DependsOn   map[string]struct {
				Condition string `yaml:"condition"`
			} `yaml:"depends_on"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &compose))

	const file = "/run/pando-secrets/postgres-password"
	postgres, pando := compose.Services["postgres"], compose.Services["pando"]
	require.Equal(t, file, postgres.Environment["POSTGRES_PASSWORD_FILE"])
	require.NotContains(t, postgres.Environment, "POSTGRES_PASSWORD", "no password, default or otherwise, in the file")
	require.Equal(t, file, pando.Environment["PANDO_DATABASE_PASSWORD_FILE"])
	require.NotContains(t, pando.Environment["PANDO_DATABASE_URL"], ":${", "the URL carries no password")
	require.NotContains(t, pando.Environment["PANDO_DATABASE_URL"], "pando:pando@")

	// Both wait for the password to exist before they start.
	for _, name := range []string{"postgres", "pando"} {
		require.Equal(t, "service_completed_successfully", compose.Services[name].DependsOn["secrets"].Condition, name)
	}
}

func TestWithoutAPasswordFileTheURLIsUsedAsGiven(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:given@db:5432/pando")
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "postgres://pando:given@db:5432/pando", cfg.Database.URL)
}
