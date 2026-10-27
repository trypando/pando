package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR415_AJoinedControlHostUsesTheClustersDatabase asserts R-415 on the
// joining side: once a control host has joined, the database address it was
// handed takes the place of the one its Compose file names, password file and
// all.
func TestR415_AJoinedControlHostUsesTheClustersDatabase(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	require.NoError(t, os.WriteFile(pw, []byte("local"), 0o400))
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando@postgres/pando")
	t.Setenv("PANDO_DATABASE_PASSWORD_FILE", pw)
	t.Setenv("PANDO_SERVER_CLUSTER_DIR", dir)

	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "postgres://pando:local@postgres/pando", cfg.Database.URL, "not joined: its own database")

	joined := filepath.Join(dir, ClusterDatabaseURLFile)
	require.NoError(t, os.WriteFile(joined, []byte("postgres://pando:c@db.example.com/pando\n"), 0o600))
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, "postgres://pando:c@db.example.com/pando", cfg.Database.URL)
	require.Equal(t, ":7443", cfg.Server.ClusterAddr)

	require.NoError(t, os.Remove(joined))
	require.NoError(t, os.Mkdir(joined, 0o700))
	_, err = Load("")
	require.ErrorContains(t, err, "could not read")

	d := Database{URL: "postgres://x"}
	require.NoError(t, d.joined(ClusterDatabaseURLFile))
	require.Equal(t, "postgres://x", d.URL)
}

// TestTheJoinTokenIsNeverReported asserts R-194 for PANDO_JOIN_TOKEN: it is a
// credential, and GET /config's settings leave it out.
func TestTheJoinTokenIsNeverReported(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando@postgres/pando")
	t.Setenv("PANDO_JOIN_TOKEN", "pj1.secret")
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "pj1.secret", cfg.Bootstrap.JoinToken)
	for _, s := range cfg.Settings {
		require.NotEqual(t, "bootstrap.join_token", s.Key)
	}
}
