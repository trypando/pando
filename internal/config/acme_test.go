package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR169_TheACMEDirectoryIsASetting asserts O-49: the CA the edge's
// certificates are ordered from defaults to Let's Encrypt, is set from the
// environment with a CA file to trust, is reported with where it came from,
// and must be an https URL.
func TestR169_TheACMEDirectoryIsASetting(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")

	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, LetsEncryptDirectory, cfg.ACME.DirectoryURL)
	require.Empty(t, cfg.ACME.CAFile)
	require.Equal(t, Source{Kind: "default"}, settingNamed(t, cfg, "acme.directory_url").Source)

	t.Setenv("PANDO_ACME_DIRECTORY_URL", "https://pebble:14000/dir")
	t.Setenv("PANDO_ACME_CA_FILE", "/etc/pando/acme/ca.pem")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, "https://pebble:14000/dir", cfg.ACME.DirectoryURL)
	require.Equal(t, "/etc/pando/acme/ca.pem", cfg.ACME.CAFile)
	require.Equal(t, Source{Kind: "env", Name: "PANDO_ACME_DIRECTORY_URL"}, settingNamed(t, cfg, "acme.directory_url").Source)
	require.Equal(t, Source{Kind: "env", Name: "PANDO_ACME_CA_FILE"}, settingNamed(t, cfg, "acme.ca_file").Source)

	t.Setenv("PANDO_ACME_DIRECTORY_URL", "http://pebble:14000/dir")
	_, err = Load("")
	require.ErrorContains(t, err, "PANDO_ACME_DIRECTORY_URL")
}
