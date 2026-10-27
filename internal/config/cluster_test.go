package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestR421_TheClustersTimingIsSetAtStartup asserts R-421's and R-420's
// timings are startup settings: T, the probe interval, the standby pass and
// the ship interval default to the requirement's [P] values, are set from the
// environment, and are refused, saying what would do, outside their bounds or
// with ships more often than the pass that makes them.
func TestR421_TheClustersTimingIsSetAtStartup(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando@postgres/pando")
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, Cluster{LivenessPeriod: DefaultLivenessPeriod, StandbyEvery: DefaultStandbyEvery, ShipEvery: DefaultShipEvery}, cfg.Cluster)

	t.Setenv("PANDO_CLUSTER_LIVENESS_PERIOD", "12s")
	t.Setenv("PANDO_CLUSTER_LIVENESS_INTERVAL", "1s")
	t.Setenv("PANDO_CLUSTER_STANDBY_EVERY", "10s")
	t.Setenv("PANDO_CLUSTER_SHIP_EVERY", "30s")
	want := Cluster{LivenessPeriod: 12 * time.Second, LivenessInterval: time.Second, StandbyEvery: 10 * time.Second, ShipEvery: 30 * time.Second}
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, want, cfg.Cluster)

	// The host agent reads them without a database.
	t.Setenv("PANDO_DATABASE_URL", "")
	got, err := LoadCluster("")
	require.NoError(t, err)
	require.Equal(t, want, got)

	for env, bad := range map[string]string{
		"PANDO_CLUSTER_LIVENESS_PERIOD":   "1s",
		"PANDO_CLUSTER_LIVENESS_INTERVAL": "3s",
		"PANDO_CLUSTER_STANDBY_EVERY":     "2h0m0s",
		"PANDO_CLUSTER_SHIP_EVERY":        "5s",
	} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, bad)
			_, err := LoadCluster("")
			require.ErrorContains(t, err, env+" is "+bad+". Valid answer:")
		})
	}
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando@postgres/pando")
	t.Setenv("PANDO_CLUSTER_LIVENESS_PERIOD", "11m")
	_, err = Load("")
	require.ErrorContains(t, err, "PANDO_CLUSTER_LIVENESS_PERIOD")

	t.Setenv("PANDO_CLUSTER_SHIP_EVERY", "soon")
	_, err = LoadCluster("")
	require.ErrorContains(t, err, "parsing configuration")
	_, err = LoadCluster(filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorContains(t, err, "reading config file")
}

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
	require.Equal(t, ":7444", cfg.Server.ClusterAddr)

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
