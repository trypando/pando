package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/config"
)

// TestR256_AReplicaAdvertisesWhereTheOthersReachIt asserts the address one
// replica gives the others for relaying a deploy's live log (R-256, issue
// #72): the configured one when set, or this host on the port it listens on.
func TestR256_AReplicaAdvertisesWhereTheOthersReachIt(t *testing.T) {
	for name, tc := range map[string]struct {
		server config.Server
		want   string
	}{
		"set, trailing slash dropped": {config.Server{AdvertiseURL: "http://10.1.2.3:8080/", Addr: ":9000"}, "http://10.1.2.3:8080"},
		"default, the listen port":    {config.Server{Addr: ":8080"}, "http://pando-1:8080"},
		"default, a listen host":      {config.Server{Addr: "0.0.0.0:8443"}, "http://pando-1:8443"},
		"default, no port at all":     {config.Server{Addr: ""}, "http://pando-1"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.server.Advertise("pando-1"))
		})
	}
}

// TestR256_TheReplicaSettingsLoadFromTheEnvironment asserts that the two
// settings several replicas need are read from their variables, with the
// pool cap defaulting to 32.
func TestR256_TheReplicaSettingsLoadFromTheEnvironment(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando@db/pando")
	cfg, err := config.Load("")
	require.NoError(t, err)
	require.EqualValues(t, 32, cfg.Database.MaxConns)
	require.Empty(t, cfg.Server.AdvertiseURL)

	t.Setenv("PANDO_DATABASE_MAX_CONNS", "12")
	t.Setenv("PANDO_SERVER_ADVERTISE_URL", "http://10.0.0.7:8080")
	cfg, err = config.Load("")
	require.NoError(t, err)
	require.EqualValues(t, 12, cfg.Database.MaxConns)
	require.Equal(t, "http://10.0.0.7:8080", cfg.Server.AdvertiseURL)
}
